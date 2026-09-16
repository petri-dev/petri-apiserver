package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/petri-dev/petri-apiserver/test/load"
)

func signToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	encode := func(value any) string {
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}

	unsigned := encode(map[string]string{"alg": "RS256", "kid": kid}) + "." + encode(claims)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestOIDCPipeline(t *testing.T) {
	var logs strings.Builder
	output := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(output)

	oldKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	var rotated, unavailable, discoveryUnavailable atomic.Bool
	var fetches atomic.Int32
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			if discoveryUnavailable.Load() {
				http.Error(w, "sensitive discovery details", http.StatusServiceUnavailable)
				return
			}
			body = map[string]any{
				"issuer":                                issuer,
				"jwks_uri":                              issuer + "/keys",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			}
		case "/keys":
			fetches.Add(1)
			if unavailable.Load() {
				http.Error(w, "sensitive JWKS details", http.StatusServiceUnavailable)
				return
			}
			key, kid := oldKey, "old"
			if rotated.Load() {
				key, kid = newKey, "new"
			}
			body = map[string]any{"keys": []map[string]string{{
				"kty": "RSA", "alg": "RS256", "use": "sig", "kid": kid,
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}}}
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	issuer = server.URL

	initCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	v, err := NewOIDCVerifier(
		initCtx,
		issuer,
		"petri",
		ClaimNames{Groups: "teams", Organization: "company", Repository: "project"},
	)
	cancel()
	if err != nil {
		t.Fatal(err)
	}

	validClaims := func() map[string]any {
		return map[string]any{
			"iss": issuer, "sub": "user-123", "aud": "petri", "exp": time.Now().Add(time.Hour).Unix(),
			"teams": []string{"engineering", "operations"}, "company": "Acme", "project": "Acme/service",
			"private": "do-not-retain-or-log", "groups": 123,
		}
	}

	want := Principal{
		Issuer:       issuer,
		Subject:      "user-123",
		Groups:       []string{"engineering", "operations"},
		Organization: "Acme",
		Repository:   "Acme/service",
	}

	check := func(t *testing.T, verifier Verifier, raw string, expected *Principal) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		p, err := verifier.Verify(ctx, raw)
		if expected == nil {
			if err == nil || !reflect.DeepEqual(p, Principal{}) {
				t.Fatalf("invalid token returned principal %+v, error %v", p, err)
			}
		} else if err != nil || !reflect.DeepEqual(p, *expected) {
			t.Fatalf("principal = %+v, error = %v; want %+v", p, err, *expected)
		}

		called := false
		handler := Middleware(verifier, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			identity, ok := PrincipalFromContext(r.Context())
			if expected == nil || !ok || !reflect.DeepEqual(identity, *expected) {
				t.Errorf("unexpected downstream identity: %+v", identity)
			}
			if len(identity.Groups) > 0 {
				identity.Groups[0] = "mutated"
				again, _ := PrincipalFromContext(r.Context())
				if again.Groups[0] == "mutated" {
					t.Error("accessor exposed mutable context groups")
				}
			}
			w.WriteHeader(http.StatusNoContent)
		}))

		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/environments", http.NoBody)
		req.Header.Set("Authorization", "Bearer "+raw)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if expected == nil {
			if called || rec.Code != http.StatusUnauthorized ||
				rec.Body.String() != "{\"error\":{\"code\":\"unauthenticated\",\"message\":\"authentication failed\"}}\n" {
				t.Fatalf("failure reached handler or exposed details: %d %s", rec.Code, rec.Body.String())
			}
		} else if !called || rec.Code != http.StatusNoContent {
			t.Fatalf("valid request rejected: %d", rec.Code)
		}
		if strings.Contains(logs.String(), raw) || strings.Contains(logs.String(), "sensitive") ||
			strings.Contains(logs.String(), "do-not-retain-or-log") {
			t.Fatal("authentication data leaked to logs")
		}
	}

	t.Run("valid after initialization cancellation", func(t *testing.T) {
		check(t, v, signToken(t, oldKey, "old", validClaims()), &want)
	})

	t.Run("bounded local OIDC load", func(t *testing.T) {
		if os.Getenv("PETRI_LOCAL_SOAK") != "true" {
			t.Skip("set PETRI_LOCAL_SOAK=true for 30s signed local OIDC exercise")
		}
		policy, err := NewPolicy("Acme", "")
		if err != nil {
			t.Fatal(err)
		}
		api := httptest.NewServer(
			Middleware(v, policy.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p, ok := PrincipalFromContext(r.Context())
				if !ok || p.Subject != want.Subject {
					t.Error("verified identity missing under load")
				}
				_, _ = io.WriteString(w, `{"items":[]}`)
			}))),
		)
		defer api.Close()

		r, err := load.Run(t.Context(), load.Config{Target: api.URL, Token: signToken(t, oldKey, "old", validClaims()),
			Rate: 20, Concurrency: 4, MaxRequests: 600, Duration: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}

		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(string(data))
		if !r.Healthy() {
			t.Fatal("local OIDC load failed")
		}
	})

	t.Run("authorization pipeline", func(t *testing.T) {
		for _, tt := range []struct {
			name, organizations, repositories string
			organization, repository          any
			status                            int
		}{
			{"organization", " Acme, Other ", "", "Acme", "unlisted/repo", 204},
			{"repository", "", " Acme/service, Other/tool ", "unlisted", "Acme/service", 204},
			{"organization OR", "Acme", "Other/tool", "Acme", "unlisted/repo", 204},
			{"repository OR cross organization", "Other", "Acme/service", "unlisted", "Acme/service", 204},
			{"unlisted", "Other", "Other/tool", "Acme", "Acme/service", 403},
			{"organization case", "acme", "", "Acme", "Acme/service", 403},
			{"repository case", "", "Acme/Service", "Acme", "Acme/service", 403},
			{"repository owner case", "", "acme/service", "Acme", "Acme/service", 403},
			{"same repository different owner", "", "Other/service", "Other", "Acme/service", 403},
			{"organization substring", "Acme", "", "AcmeExtra", "Acme/service", 403},
			{"repository substring", "", "Acme/service", "Acme", "Acme/serviceExtra", 403},
			{"organization whitespace unchanged", "Acme", "", " Acme ", "Acme/service", 403},
			{"repository whitespace unchanged", "", "Acme/service", "Acme", " Acme/service ", 403},
			{"missing organization", "Acme", "", nil, "Acme/service", 403},
			{"missing repository", "", "Acme/service", "Acme", nil, 403},
			{"missing both", "Acme", "Acme/service", nil, nil, 403},
			{"missing organization still repository OR", "Acme", "Acme/service", nil, "Acme/service", 204},
			{"missing repository still organization OR", "Acme", "Acme/service", "Acme", nil, 204},
			{"empty claims", "Acme", "Acme/service", "", "", 403},
			{"malformed organization despite repository match", "Acme", "Acme/service", []string{"Acme"}, "Acme/service", 401},
			{"malformed repository despite organization match", "Acme", "Acme/service", "Acme", 42, 401},
			{"null selected claim", "Acme", "", json.RawMessage("null"), "Acme/service", 401},
		} {
			t.Run(tt.name, func(t *testing.T) {
				policy, err := NewPolicy(tt.organizations, tt.repositories)
				if err != nil {
					t.Fatal(err)
				}

				claims := validClaims()
				delete(claims, "company")
				delete(claims, "project")
				if tt.organization != nil {
					claims["company"] = tt.organization
				}
				if tt.repository != nil {
					claims["project"] = tt.repository
				}

				raw := signToken(t, oldKey, "old", claims)
				called := false
				h := Middleware(v, policy.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					p, ok := PrincipalFromContext(r.Context())
					if !ok || p.Subject != "user-123" || p.Issuer != issuer {
						t.Error("verified identity missing")
					}
					w.WriteHeader(http.StatusNoContent)
				})))

				req := httptest.NewRequestWithContext(t.Context(), "POST", "/environments", http.NoBody)
				req.Header.Set("Authorization", "Bearer "+raw)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)

				if rec.Code != tt.status || called != (tt.status == 204) {
					t.Fatalf("status %d, called %t; want %d", rec.Code, called, tt.status)
				}
				if tt.status == 403 &&
					rec.Body.String() != "{\"error\":{\"code\":\"forbidden\",\"message\":\"access denied\"}}\n" {
					t.Fatal("policy denial exposed details")
				}
				if tt.status == 401 &&
					rec.Body.String() != "{\"error\":{\"code\":\"unauthenticated\",\"message\":\"authentication failed\"}}\n" {
					t.Fatal("authentication failure exposed details")
				}
				if strings.Contains(logs.String(), raw) || strings.Contains(logs.String(), "sensitive") ||
					strings.Contains(logs.String(), "do-not-retain-or-log") {
					t.Fatal("authorization pipeline leaked credentials or claims")
				}
			})
		}
	})

	for _, tt := range []struct {
		name, claim string
		value       any
	}{
		{"wrong issuer", "iss", issuer + "/other"}, {"wrong audience", "aud", "other"},
		{"expired", "exp", time.Now().Add(-time.Minute).Unix()},
		{"empty subject", "sub", ""}, {"numeric subject", "sub", 42},
		{"missing issuer", "iss", nil}, {"missing subject", "sub", nil},
		{"missing audience", "aud", nil}, {"missing expiry", "exp", nil},
		{"null issuer", "iss", json.RawMessage("null")}, {"null subject", "sub", json.RawMessage("null")},
		{"null audience", "aud", json.RawMessage("null")}, {"null expiry", "exp", json.RawMessage("null")},
		{"string expiry", "exp", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)},
		{"audience null element", "aud", []any{"petri", nil}},
		{"groups string", "teams", "engineering"}, {"groups mixed", "teams", []any{"engineering", 42}},
		{"groups null element", "teams", []any{nil}}, {"groups null", "teams", json.RawMessage("null")},
		{"organization array", "company", []string{"Acme"}}, {"organization null", "company", json.RawMessage("null")},
		{"repository object", "project", map[string]string{"name": "service"}}, {"repository null", "project", json.RawMessage("null")},
		{"expiry malformed", "exp", "tomorrow"}, {"audience malformed", "aud", 42},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claims := validClaims()
			if tt.value == nil {
				delete(claims, tt.claim)
			} else {
				claims[tt.claim] = tt.value
			}
			check(t, v, signToken(t, oldKey, "old", claims), nil)
		})
	}

	t.Run("optional claims absent", func(t *testing.T) {
		claims := validClaims()
		delete(claims, "teams")
		delete(claims, "company")
		delete(claims, "project")
		check(t, v, signToken(t, oldKey, "old", claims), &Principal{Issuer: issuer, Subject: "user-123"})
	})

	t.Run("audience array", func(t *testing.T) {
		claims := validClaims()
		claims["aud"] = []string{"other", "petri"}
		check(t, v, signToken(t, oldKey, "old", claims), &want)
	})

	t.Run("unconfigured claims ignored", func(t *testing.T) {
		generic, err := NewOIDCVerifier(t.Context(), issuer, "petri", ClaimNames{})
		if err != nil {
			t.Fatal(err)
		}
		check(t, generic, signToken(t, oldKey, "old", validClaims()), &Principal{Issuer: issuer, Subject: "user-123"})
	})

	t.Run("GitHub claim profile", func(t *testing.T) {
		profile, err := NewOIDCVerifier(
			t.Context(),
			issuer,
			"petri",
			ClaimNames{Organization: "repository_owner", Repository: "repository"},
		)
		if err != nil {
			t.Fatal(err)
		}

		claims := validClaims()
		claims["repository_owner"] = "Acme"
		claims["repository"] = "Acme/service"
		check(
			t,
			profile,
			signToken(t, oldKey, "old", claims),
			&Principal{Issuer: issuer, Subject: "user-123", Organization: "Acme", Repository: "Acme/service"},
		)
	})

	t.Run("wrong signature", func(t *testing.T) { check(t, v, signToken(t, newKey, "old", validClaims()), nil) })
	t.Run("unknown key", func(t *testing.T) { check(t, v, signToken(t, newKey, "unknown", validClaims()), nil) })
	t.Run("malformed JWT", func(t *testing.T) { check(t, v, "not-a-jwt", nil) })

	t.Run("unsigned JWT", func(t *testing.T) {
		raw := signToken(t, oldKey, "old", validClaims())
		parts := strings.Split(raw, ".")
		check(t, v, base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))+"."+parts[1]+".", nil)
	})

	t.Run("JWKS outage", func(t *testing.T) {
		unavailable.Store(true)
		defer unavailable.Store(false)

		before := fetches.Load()
		check(t, v, signToken(t, oldKey, "old", validClaims()), &want)
		if fetches.Load() != before {
			t.Error("cached key unexpectedly required network")
		}

		check(t, v, signToken(t, newKey, "new", validClaims()), nil)
		cold, err := NewOIDCVerifier(t.Context(), issuer, "petri", ClaimNames{})
		if err != nil {
			t.Fatal(err)
		}
		check(t, cold, signToken(t, oldKey, "old", validClaims()), nil)
	})

	t.Run("rotation", func(t *testing.T) {
		rotated.Store(true)

		before := fetches.Load()
		check(t, v, signToken(t, newKey, "new", validClaims()), &want)
		if fetches.Load() <= before {
			t.Error("new kid did not refresh JWKS")
		}

		unavailable.Store(true)
		defer unavailable.Store(false)
		check(t, v, signToken(t, newKey, "new", validClaims()), &want)
	})

	t.Run("discovery failure", func(t *testing.T) {
		discoveryUnavailable.Store(true)
		v, err := NewOIDCVerifier(t.Context(), issuer, "petri", ClaimNames{})
		if err == nil || v != nil || strings.Contains(err.Error(), "sensitive") {
			t.Fatalf("discovery did not fail safely: %v", err)
		}
	})
}

func TestOIDCDiscoveryCancellation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if v, err := NewOIDCVerifier(ctx, server.URL, "petri", ClaimNames{}); err == nil || v != nil {
		t.Fatal("discovery ignored deadline")
	}

	if time.Since(start) > time.Second {
		t.Fatal("discovery did not honor the shorter caller deadline")
	}
}

func TestOIDCInvalidDiscovery(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ name, issuer, jwks string }{
		{"wrong issuer", "https://wrong.example", "/keys"},
		{"missing JWKS", "", ""},
		{"relative JWKS", "", "/keys"},
		{"invalid JWKS scheme", "", "ftp://issuer.example/keys"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var issuer string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).
					Encode(map[string]string{"issuer": issuer, "jwks_uri": tt.jwks}); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()

			issuer = tt.issuer
			if issuer == "" {
				issuer = server.URL
			}

			if v, err := NewOIDCVerifier(t.Context(), server.URL, "petri", ClaimNames{}); err == nil || v != nil {
				t.Fatal("invalid discovery accepted")
			}
		})
	}
}

func TestPrincipalAbsentAndEmptyToken(t *testing.T) {
	t.Parallel()
	if _, ok := PrincipalFromContext(t.Context()); ok {
		t.Fatal("unexpected identity")
	}
	if _, err := NewTokenVerifier("").Verify(t.Context(), ""); err == nil {
		t.Fatal("empty configured token accepted")
	}
}

func TestOIDCJWKSRequestDeadline(t *testing.T) {
	t.Parallel()

	var issuer string
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/keys" {
			select {
			case <-r.Context().Done():
			case <-finished:
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"issuer":"`+issuer+`","jwks_uri":"`+issuer+`/keys"}`); err != nil {
			t.Error(err)
		}
	}))
	defer func() {
		close(finished)
		server.Close()
	}()

	issuer = server.URL
	v, err := NewOIDCVerifier(t.Context(), issuer, "petri", ClaimNames{})
	if err != nil {
		t.Fatal(err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	raw := signToken(
		t,
		key,
		"key",
		map[string]any{"iss": issuer, "sub": "user", "aud": "petri", "exp": time.Now().Add(time.Hour).Unix()},
	)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := v.Verify(ctx, raw); err == nil {
		t.Fatal("JWKS wait ignored request deadline")
	}
	if time.Since(start) > time.Second {
		t.Fatal("JWKS wait did not honor the shorter caller deadline")
	}
}
