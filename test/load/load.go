package load

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	httpClientTimeout    = 3 * time.Second
	responseBodyLimit    = 4 << 20
	httpSuccessMin       = 200
	clientErrorMax       = 400
	serverErrorThreshold = 500
	p95Percentile        = 0.95
)

// Config is deliberately not serialized, since it contains credentials.
type Config struct {
	Target, Token                  string
	Rate, Concurrency, MaxRequests int
	Duration                       time.Duration
	AllowExternal                  bool
}

// FromEnv requires every traffic bound and credentials explicitly, with no CLI secrets.
func FromEnv(getenv func(string) string) (Config, error) {
	c := Config{Target: getenv("PETRI_LOAD_TARGET"), Token: getenv("PETRI_LOAD_TOKEN")}
	var err error

	c.Rate, err = strconv.Atoi(getenv("PETRI_LOAD_RATE"))
	if err != nil {
		return Config{}, errors.New("PETRI_LOAD_RATE must be an integer")
	}
	c.Concurrency, err = strconv.Atoi(getenv("PETRI_LOAD_CONCURRENCY"))
	if err != nil {
		return Config{}, errors.New("PETRI_LOAD_CONCURRENCY must be an integer")
	}
	c.MaxRequests, err = strconv.Atoi(getenv("PETRI_LOAD_MAX_REQUESTS"))
	if err != nil {
		return Config{}, errors.New("PETRI_LOAD_MAX_REQUESTS must be an integer")
	}
	c.Duration, err = time.ParseDuration(getenv("PETRI_LOAD_DURATION"))
	if err != nil {
		return Config{}, errors.New("PETRI_LOAD_DURATION must be a duration")
	}

	switch getenv("PETRI_LOAD_ALLOW_EXTERNAL") {
	case "":
	case "I_HAVE_TARGET_OWNER_AUTHORIZATION":
		c.AllowExternal = true
	default:
		return Config{}, errors.New("invalid external target authorization opt-in")
	}

	return c, c.Validate()
}

func (c Config) Validate() error {
	if err := c.validateBounds(); err != nil {
		return err
	}
	if len(c.Token) == 0 || len(c.Token) > 16384 || strings.ContainsAny(c.Token, " \t\r\n") {
		return errors.New("nonempty bounded bearer credential required; whitespace forbidden")
	}
	return c.validateTarget()
}

func (c Config) validateBounds() error {
	if c.Rate < 1 || c.Rate > 200 || c.Concurrency < 1 || c.Concurrency > 32 || c.MaxRequests < 1 ||
		c.MaxRequests > 100000 ||
		c.Duration < 100*time.Millisecond ||
		c.Duration > 30*time.Minute {
		return errors.New(
			"load bounds required: rate 1..200/s, concurrency 1..32, max requests 1..100000, duration 100ms..30m",
		)
	}
	return nil
}

func (c Config) validateTarget() error {
	u, err := url.Parse(c.Target)
	if err != nil || !validTargetURL(u) {
		return errors.New("target must be an HTTP(S) origin without credentials, path, query or fragment")
	}
	if port := u.Port(); port != "" {
		portNum, parseErr := strconv.Atoi(port)
		if parseErr != nil || portNum < 1 || portNum > 65535 {
			return errors.New("invalid target port")
		}
	}
	return c.validateTargetHost(u.Hostname(), u.Scheme)
}

func validTargetURL(u *url.URL) bool {
	return u.Hostname() != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" &&
		u.RawPath == "" &&
		(u.Path == "" || u.Path == "/") &&
		(u.Scheme == "http" || u.Scheme == "https")
}

func (c Config) validateTargetHost(hostname, scheme string) error {
	ip := net.ParseIP(hostname)
	if ip == nil || !ip.IsLoopback() {
		if !c.AllowExternal || scheme != "https" {
			return errors.New("non-loopback targets require HTTPS and explicit target owner authorization")
		}
	}
	return nil
}

// Report contains only measurements, never URLs, credentials, bodies or raw errors.
type Report struct {
	Started                                         time.Time
	ElapsedSeconds                                  float64
	Rate, Concurrency, MaxRequests                  int
	DurationSeconds                                 float64
	Offered, StartedRequests, Dropped, NotOffered   int
	Statuses                                        map[int]int
	TransportErrors, Client4xx, Eligible, Server5xx int
	Availability                                    *float64
	P95Milliseconds                                 *float64
	AllP95Milliseconds                              *float64
	OfferedPerSecond, AchievedPerSecond             float64
	Canceled                                        bool
}

// Healthy is a validation check, not an SLO assertion. Even one 4xx fails this workload.
func (r Report) Healthy() bool {
	return !r.Canceled && r.StartedRequests > 0 && r.Statuses[http.StatusOK] == r.StartedRequests &&
		r.TransportErrors == 0 &&
		r.Dropped == 0
}

// Run alternates bounded first-page lists. There are no mutations, redirects, proxies, retries or response logs. Fresh connections avoid net/http's implicit GET retries.
// Scheduling ends at Duration, in-flight requests have at most three seconds to drain.
func Run(ctx context.Context, c Config) (Report, error) {
	if err := c.Validate(); err != nil {
		return Report{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, c.Duration+3*time.Second)
	defer cancel()

	transport := &http.Transport{DisableKeepAlives: true, MaxConnsPerHost: c.Concurrency,
		DialContext: (&net.Dialer{Timeout: httpClientTimeout}).DialContext, TLSHandshakeTimeout: httpClientTimeout}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: httpClientTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}

	r := Report{Started: time.Now().UTC(), Rate: c.Rate, Concurrency: c.Concurrency, MaxRequests: c.MaxRequests,
		DurationSeconds: c.Duration.Seconds(), Statuses: map[int]int{}}

	start := time.Now()
	interval := time.Second / time.Duration(c.Rate)
	planned := min(c.MaxRequests, int((c.Duration-1)/interval)+1)

	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, c.Concurrency)
	latencies := make([]float64, 0, planned)
	eligibleLatencies := make([]float64, 0, planned)

	for i := range planned {
		due := start.Add(time.Duration(i) * interval)
		if !awaitSlot(ctx, due) {
			break
		}

		r.Offered++
		if time.Since(due) >= interval {
			r.Dropped++
			continue
		}

		select {
		case slots <- struct{}{}:
		default:
			r.Dropped++
			continue
		}

		r.StartedRequests++
		path := []string{"/v1/templates?limit=100", "/v1/environments?limit=100"}[i%2]
		wg.Go(func() {
			defer func() { <-slots }()
			begin := time.Now()
			req, err := http.NewRequestWithContext(
				ctx,
				http.MethodGet,
				strings.TrimSuffix(c.Target, "/")+path,
				http.NoBody,
			)
			status := 0
			if err == nil {
				req.Header.Set("Authorization", "Bearer "+c.Token)
				status, err = doRequest(client, req)
			}
			elapsed := float64(time.Since(begin)) / float64(time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			recordResult(&r, &latencies, &eligibleLatencies, status, elapsed, err)
		})
	}

	wg.Wait()
	finalizeReport(&r, ctx, start, planned, latencies, eligibleLatencies)
	return r, nil
}

func finalizeReport(
	r *Report,
	ctx context.Context,
	start time.Time,
	planned int,
	latencies, eligibleLatencies []float64,
) {
	r.Canceled = ctx.Err() != nil
	r.NotOffered = planned - r.Offered
	r.ElapsedSeconds = time.Since(start).Seconds()
	r.OfferedPerSecond = float64(r.Offered) / r.ElapsedSeconds
	r.AchievedPerSecond = float64(r.StartedRequests-r.TransportErrors) / r.ElapsedSeconds

	if r.Eligible > 0 {
		r.Availability = new(1 - float64(r.Server5xx)/float64(r.Eligible))
	}

	computePercentiles(r, latencies, eligibleLatencies)
}

func awaitSlot(ctx context.Context, due time.Time) bool {
	timer := time.NewTimer(max(0, time.Until(due)))
	select {
	case <-ctx.Done():
		timer.Stop()
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

func recordResult(r *Report, latencies, eligibleLatencies *[]float64, status int, elapsed float64, err error) {
	*latencies = append(*latencies, elapsed)
	if status != 0 {
		r.Statuses[status]++
	}
	if err != nil {
		r.TransportErrors++
	}
	if status >= 400 && status < 500 {
		r.Client4xx++
	}
	if (status >= httpSuccessMin && status < clientErrorMax) || status >= serverErrorThreshold {
		r.Eligible++
		*eligibleLatencies = append(*eligibleLatencies, elapsed)
		if status >= serverErrorThreshold {
			r.Server5xx++
		}
	}
}

func computePercentiles(r *Report, latencies, eligibleLatencies []float64) {
	for _, population := range []struct {
		samples []float64
		result  **float64
	}{{latencies, &r.AllP95Milliseconds}, {eligibleLatencies, &r.P95Milliseconds}} {
		if len(population.samples) > 0 {
			slices.Sort(population.samples)
			*population.result = new(
				population.samples[int(math.Ceil(p95Percentile*float64(len(population.samples))))-1],
			)
		}
	}
}

func doRequest(client *http.Client, req *http.Request) (int, error) {
	response, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(io.Discard, io.LimitReader(response.Body, responseBodyLimit+1))
	if closeErr := response.Body.Close(); err == nil {
		err = closeErr
	}
	if n > responseBodyLimit {
		return response.StatusCode, errors.New("response limit exceeded")
	}
	return response.StatusCode, err
}
