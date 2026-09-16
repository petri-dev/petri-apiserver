package load

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExplicitTarget(t *testing.T) {
	t.Parallel()
	if os.Getenv("PETRI_LOAD_RUN") != "true" {
		t.Skip("opt-in only: PETRI_LOAD_RUN=true and explicit load environment required")
	}
	c, err := FromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}

	r, err := Run(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}

	t.Log(string(data))
	if !r.Healthy() {
		t.Fatal("load validation failed; inspect sanitized counters (not an SLO verdict)")
	}
}

func TestBoundsAndMeasurements(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer secret-canary" {
			t.Error("credential missing")
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := Config{
		Target:      server.URL,
		Token:       "secret-canary",
		Rate:        20,
		Concurrency: 2,
		MaxRequests: 5,
		Duration:    time.Second,
	}
	r, err := Run(t.Context(), c)
	if err != nil || !r.Healthy() || r.Eligible != 5 || calls.Load() != 5 || r.P95Milliseconds == nil ||
		*r.P95Milliseconds < 20 {
		t.Fatalf("bad measurement: %+v %v", r, err)
	}

	data, err := json.Marshal(r)
	if err != nil || strings.Contains(string(data), c.Token) || strings.Contains(string(data), c.Target) {
		t.Fatal("report leaked configuration")
	}

	for _, mutate := range []func(*Config){
		func(c *Config) { c.Rate = 0 }, func(c *Config) { c.Rate = 201 },
		func(c *Config) { c.Concurrency = 0 }, func(c *Config) { c.Concurrency = 33 },
		func(c *Config) { c.MaxRequests = 0 }, func(c *Config) { c.MaxRequests = 100001 },
		func(c *Config) { c.Duration = 0 }, func(c *Config) { c.Duration = 31 * time.Minute },
		func(c *Config) { c.Token = "secret-canary\n" }, func(c *Config) { c.Token = "" },
		func(c *Config) { c.Target = "https://secret-canary@example.invalid" },
		func(c *Config) { c.Target = "https://example.invalid" },
		func(c *Config) { c.Target += "?secret-canary" },
		func(c *Config) { c.Target = "http://localhost:8080" },
		func(c *Config) { c.Target = "http://192.168.1.1"; c.AllowExternal = true },
	} {
		bad := c
		mutate(&bad)
		_, err := Run(t.Context(), bad)
		if err == nil || strings.Contains(err.Error(), "secret-canary") {
			t.Fatal("invalid config accepted or leaked")
		}
	}
	if calls.Load() != 5 {
		t.Fatal("invalid config sent traffic")
	}

	env := map[string]string{
		"PETRI_LOAD_TARGET":       c.Target,
		"PETRI_LOAD_TOKEN":        c.Token,
		"PETRI_LOAD_RATE":         "20",
		"PETRI_LOAD_CONCURRENCY":  "2",
		"PETRI_LOAD_MAX_REQUESTS": "5",
		"PETRI_LOAD_DURATION":     "1s",
	}
	for _, key := range []string{"PETRI_LOAD_RATE", "PETRI_LOAD_CONCURRENCY", "PETRI_LOAD_MAX_REQUESTS", "PETRI_LOAD_DURATION", "PETRI_LOAD_ALLOW_EXTERNAL"} {
		old := env[key]
		env[key] = "secret-canary"
		if _, err := FromEnv(
			func(k string) string { return env[k] },
		); err == nil ||
			strings.Contains(err.Error(), "secret-canary") {
			t.Fatal("env validation leaked or passed")
		}
		env[key] = old
	}
}

func TestFailureAccountingAndCancellation(t *testing.T) {
	t.Parallel()
	for _, code := range []int{200, 401, 403, 429, 500, 302} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "https://never-contact.invalid")
				w.WriteHeader(code)
			}))
			defer server.Close()

			r, err := Run(
				t.Context(),
				Config{
					Target:      server.URL,
					Token:       "canary",
					Rate:        10,
					Concurrency: 1,
					MaxRequests: 2,
					Duration:    time.Second,
				},
			)
			if err != nil || r.Statuses[code] != 2 || r.Healthy() != (code == 200) {
				t.Fatalf("incorrect validation: %+v %v", r, err)
			}
			if code >= 400 && code < 500 && (r.Eligible != 0 || r.Availability != nil || r.P95Milliseconds != nil) {
				t.Fatal("4xx counted as SLO success")
			}
			if code == 500 && (r.Server5xx != 2 || *r.Availability != 0) {
				t.Fatal("5xx hidden")
			}
		})
	}

	server := httptest.NewServer(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }),
	)
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	r, err := Run(
		ctx,
		Config{Target: server.URL, Token: "canary", Rate: 100, Concurrency: 1, MaxRequests: 100, Duration: time.Second},
	)
	if err != nil || !r.Canceled || r.Healthy() || r.StartedRequests != 1 || r.TransportErrors != 1 || r.Dropped == 0 ||
		r.NotOffered == 0 ||
		r.Offered != r.StartedRequests+r.Dropped ||
		time.Since(start) > time.Second {
		t.Fatalf("unbounded or hidden saturation: %+v %v", r, err)
	}
}

func TestRequestDeadline(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }),
	)
	defer server.Close()

	start := time.Now()
	r, err := Run(
		t.Context(),
		Config{
			Target:      server.URL,
			Token:       "canary",
			Rate:        1,
			Concurrency: 1,
			MaxRequests: 1,
			Duration:    100 * time.Millisecond,
		},
	)
	if err != nil || r.TransportErrors != 1 || r.Healthy() || r.Availability != nil ||
		time.Since(start) > 4*time.Second {
		t.Fatalf("request deadline failed: %+v %v", r, err)
	}
}
