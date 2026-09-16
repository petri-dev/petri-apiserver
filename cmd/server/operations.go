package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/petri-dev/petri-apiserver/internal/httpjson"
	"github.com/petri-dev/petri-apiserver/internal/requeststate"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	httpSuccessMin       = 200
	serverErrorStatusMin = 500

	pathMetrics    = "/metrics"
	labelRoute     = "route"
	labelMethod    = "method"
	labelStatus    = "status"
	resultRejected = "rejected"
)

type operations struct {
	logger    *slog.Logger
	namespace string
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	metrics   http.Handler
}

func newOperations(logger *slog.Logger, namespace string) *operations {
	registry := prometheus.NewRegistry()
	o := &operations{
		logger:    logger,
		namespace: namespace,
		requests: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "petri_http_requests_total", Help: "Completed application requests."},
			[]string{labelRoute, labelMethod, labelStatus},
		),
		duration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "petri_http_request_duration_seconds",
				Help:    "Application handler duration, not ingress or operator latency.",
				Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, .75, 1, 2.5, 5, 10, 30},
			},
			[]string{labelRoute, labelMethod, labelStatus},
		),
	}
	registry.MustRegister(o.requests, o.duration)

	scrape := promhttp.HandlerFor(
		registry,
		promhttp.HandlerOpts{MaxRequestsInFlight: metricsMaxInflight, Timeout: metricsScrapeTimeout},
	)
	o.metrics = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathMetrics || r.URL.EscapedPath() != r.URL.Path {
			httpjson.NotFound(w)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			httpjson.MethodNotAllowed(w, "GET, HEAD")
			return
		}
		scrape.ServeHTTP(w, r)
	})

	return o
}

func canonicalRoute(r *http.Request) string {
	if r.URL.EscapedPath() != r.URL.Path {
		return "unmatched"
	}
	switch r.URL.Path {
	case pathHealthz, pathReadyz, pathEnvironments, pathTemplates:
		return r.URL.Path
	}
	if name, ok := strings.CutPrefix(
		r.URL.Path,
		"/v1/environments/",
	); ok && name != "" &&
		!strings.Contains(name, "/") {
		return "/v1/environments/{name}"
	}
	return "unmatched"
}

func boundedMethod(method string) string {
	switch method {
	case "GET", "HEAD", "POST", "DELETE", "PUT", "PATCH", "OPTIONS", "CONNECT", "TRACE":
		return method
	default:
		return "OTHER"
	}
}

func safeRequestID(values []string) (string, error) {
	if len(values) == 1 && len(values[0]) >= 1 && len(values[0]) <= 64 &&
		strings.Trim(values[0], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") == "" {
		return values[0], nil
	}

	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate request ID: %w", err)
	}

	return hex.EncodeToString(id[:]), nil
}

func auditIdentity(value string) string {
	if len(value) <= 256 && strings.IndexFunc(value, func(r rune) bool { return r < 32 || r > 126 }) == -1 {
		return value
	}

	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type responseWriter struct {
	http.ResponseWriter
	status      int
	writeFailed bool
}

func (w *responseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *responseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}

	if status >= httpSuccessMin {
		w.status = status
	}

	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}

	n, err := w.ResponseWriter.Write(body)
	w.writeFailed = w.writeFailed || err != nil
	return n, err
}

func (o *operations) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id, err := safeRequestID(r.Header.Values("X-Request-ID"))

		route, method := canonicalRoute(r), boundedMethod(r.Method)
		state := &requeststate.State{}
		if route == "/v1/environments/{name}" {
			name := strings.TrimPrefix(r.URL.Path, "/v1/environments/")
			if len(validation.IsDNS1123Label(name)) == 0 {
				state.Name = name
			}
		}

		rw := &responseWriter{ResponseWriter: w}
		defer o.finishRequest(rw, r, id, route, method, state, start)

		if err != nil {
			o.logger.Error("request failed", "error", err)
			httpjson.Error(rw, http.StatusInternalServerError, "internal_error", "request failed")
			return
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(rw, r.WithContext(requeststate.WithContext(r.Context(), state)))
	})
}

func (o *operations) finishRequest(
	rw *responseWriter,
	r *http.Request,
	id, route, method string,
	state *requeststate.State,
	start time.Time,
) {
	panicked := recover() != nil
	if panicked && rw.status == 0 {
		httpjson.Error(rw, http.StatusInternalServerError, "internal_error", "request failed")
	}
	if rw.status == 0 {
		rw.status = http.StatusOK
	}

	duration := time.Since(start).Seconds()
	status := strconv.Itoa(rw.status)
	o.requests.WithLabelValues(route, method, status).Inc()
	o.duration.WithLabelValues(route, method, status).Observe(duration)

	attrs := []any{
		"request_id",
		id,
		labelRoute,
		route,
		labelMethod,
		method,
		labelStatus,
		rw.status,
		"duration_seconds",
		duration,
	}
	if rw.writeFailed || panicked {
		attrs = append(attrs, "response_failed", true)
	}
	o.logger.Info("access", attrs...)

	if isEnvironmentMutation(method, r.URL.Path) {
		o.auditEnvironmentMutation(state, rw.status, panicked, method, attrs)
	}

	if panicked {
		panic(http.ErrAbortHandler)
	}
}

func isEnvironmentMutation(method, path string) bool {
	return (method == "POST" || method == "DELETE") &&
		(path == pathEnvironments || strings.HasPrefix(path, pathEnvironments+"/"))
}

func (o *operations) auditEnvironmentMutation(
	state *requeststate.State,
	status int,
	panicked bool,
	method string,
	baseAttrs []any,
) {
	result := state.Result
	if result == "" {
		result = resultRejected
		if status >= serverErrorStatusMin || panicked {
			result = "failed"
		}
	}
	action := "upsert"
	if method == "DELETE" {
		action = "delete"
	}
	attrs := append(
		baseAttrs,
		"issuer",
		auditIdentity(state.Issuer),
		"subject",
		auditIdentity(state.Subject),
		"action",
		action,
		"resource",
		"ephemeralenvironments",
		"name",
		state.Name,
		"namespace",
		o.namespace,
		"result",
		result,
	)
	o.logger.Info("audit", attrs...)
}
