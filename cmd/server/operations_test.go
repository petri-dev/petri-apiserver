package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/petri-dev/petri-apiserver/internal/auth"
	"github.com/petri-dev/petri-apiserver/internal/handler"
	"github.com/petri-dev/petri-apiserver/internal/httpjson"
	"github.com/petri-dev/petri-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestDependencyReadiness(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"healthy", "missing-crd", "missing-template-crd", "missing-namespace", "terminating-namespace", "denied", "namespace-denied", "down", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("readiness attempted a write")
				}
				if failure == "timeout" {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/namespaces/management" {
					switch failure {
					case "missing-namespace":
						w.WriteHeader(http.StatusNotFound)
					case "namespace-denied":
						w.WriteHeader(http.StatusForbidden)
					case "terminating-namespace":
						_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"management","deletionTimestamp":"2026-01-01T00:00:00Z"}}`)
					default:
						_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"management"}}`)
					}
					return
				}
				if r.URL.Path != "/apis/core.petri.run/v1alpha1/namespaces/management/ephemeralenvironments" && r.URL.Path != "/apis/core.petri.run/v1alpha1/namespaces/management/environmenttemplates" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				if r.URL.Query().Get("limit") != "1" {
					t.Error("unbounded list")
				}
				switch {
				case failure == "missing-crd", failure == "missing-template-crd" && strings.HasSuffix(r.URL.Path, "/environmenttemplates"):
					w.WriteHeader(http.StatusNotFound)
				case failure == "denied":
					w.WriteHeader(http.StatusForbidden)
				default:
					_, _ = io.WriteString(w, `{"apiVersion":"core.petri.run/v1alpha1","kind":"List","items":[]}`)
				}
			}))
			defer api.Close()

			cl, err := dynamic.NewForConfig(&rest.Config{Host: api.URL})
			if err != nil {
				t.Fatal(err)
			}
			if failure == "down" {
				api.Close()
			}

			check := dependencyCheck(cl, "management")
			security := &authConfig{verifier: auth.NewTokenVerifier("secret"), policy: auth.NewDevelopmentPolicy()}
			router := newRouter(security, nil, nil, check)
			start := time.Now()
			r := httptest.NewRecorder()
			router.ServeHTTP(r, httptest.NewRequestWithContext(t.Context(), "GET", "/readyz", http.NoBody))

			want := 503
			if failure == "healthy" {
				want = 200
			}
			if r.Code != want {
				t.Fatalf("got %d: %s", r.Code, r.Body)
			}
			if want == 503 && r.Body.String() != "{\"error\":{\"code\":\"internal_error\",\"message\":\"dependencies unavailable\"}}\n" {
				t.Fatalf("unsanitized readiness: %s", r.Body)
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("readiness exceeded timeout")
			}

			for _, method := range []string{"GET", "HEAD"} {
				r = httptest.NewRecorder()
				req := httptest.NewRequestWithContext(t.Context(), method, "/healthz", http.NoBody)
				req.Header.Set("Authorization", "invalid")
				router.ServeHTTP(r, req)
				if r.Code != 200 {
					t.Fatal("liveness depends on auth or Kubernetes")
				}
			}
		})
	}
}

func TestRequestIDs(t *testing.T) {
	t.Parallel()
	ops := newOperations(slog.New(slog.NewJSONHandler(io.Discard, nil)), "management")
	h := ops.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))

	for _, values := range [][]string{nil, {""}, {"safe.ID_123-abc"}, {"spoof\r\nheader: value"}, {"with space"}, {strings.Repeat("a", 65)}, {"one", "two"}, {"unicode-☃"}, {strings.Repeat("a", 64)}} {
		id, err := safeRequestID(values)
		if err != nil {
			t.Fatal(err)
		}
		if len(id) > 64 || strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") != "" {
			t.Fatalf("unsafe ID %q", id)
		}
		valid := len(values) == 1 && (values[0] == "safe.ID_123-abc" || len(values[0]) == 64)
		if valid && id != values[0] {
			t.Fatal("valid ID replaced")
		}
		nextID, err := safeRequestID(values)
		if err != nil {
			t.Fatal(err)
		}
		if !valid && id == nextID {
			t.Fatal("replacement ID not random")
		}

		r := httptest.NewRequestWithContext(t.Context(), "GET", "/healthz", http.NoBody)
		r.Header[http.CanonicalHeaderKey("X-Request-ID")] = values
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		returned := rec.Header().Get("X-Request-ID")
		if (valid && returned != id) || (!valid && (len(returned) != 32 || strings.Trim(returned, "0123456789abcdef") != "")) {
			t.Fatal("unsafe or missing response request ID")
		}
	}
}

func TestOperationsAuditAndMetrics(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	ops := newOperations(slog.New(slog.NewJSONHandler(&logs, nil)), "management")
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	policy, err := auth.NewPolicy("Acme", "")
	if err != nil {
		t.Fatal(err)
	}

	router := ops.middleware(newRouter(&authConfig{verifier: routerVerifier{}, policy: policy}, handler.NewEnvironmentHandlerInNamespace(cl, "management"), handler.NewTemplateHandlerInNamespace(cl, "management"), func(context.Context) error { return nil }))

	for i, tt := range []struct {
		method, path, body, token, result string
		status                            int
	}{
		{"POST", "/v1/environments", `{"name":"safe-name","template":"template","env":{"private":"body-secret"},"source":{"repo":"source-secret"},"ttl":"1h"}`, "Acme", "created", 201},
		{"POST", "/v1/environments", `{"name":"safe-name","template":"template"}`, "Acme", "updated_or_unchanged", 200},
		{"POST", "/v1/environments", `{"name":"BAD-name","template":"template"}`, "Acme", "rejected", 400},
		{"DELETE", "/v1/environments/safe-name", "", "Other", "rejected", 403},
		{"DELETE", "/v1/environments/safe-name", "", "invalid", "rejected", 401},
		{"DELETE", "/v1/environments/safe-name", "", "Acme", "deletion_accepted", 204},
		{"DELETE", "/v1/environments/safe-name", "", "Acme", "rejected", 404},
		{"DELETE", "/v1/environments/unsafe%0Aname", "", "Acme", "rejected", 404},
	} {
		r := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, strings.NewReader(tt.body))
		r.Header.Set("Authorization", "Bearer "+tt.token)
		r.Header.Set("X-Request-ID", "request-"+strconv.Itoa(i))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		if rec.Code != tt.status || rec.Header().Get("X-Request-ID") != "request-"+strconv.Itoa(i) {
			t.Fatalf("response: %d %v", rec.Code, rec.Header())
		}

		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		if len(lines) != (i+1)*2 {
			t.Fatal("expected exactly one access and audit per mutation")
		}

		var audit map[string]any
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &audit); err != nil {
			t.Fatal(err)
		}
		if audit["msg"] != "audit" || audit["result"] != tt.result || audit["namespace"] != "management" || audit["request_id"] != rec.Header().Get("X-Request-ID") || audit["status"] != float64(tt.status) {
			t.Fatalf("audit: %v", audit)
		}
		if tt.token != "invalid" && (audit["issuer"] != "issuer" || audit["subject"] != "user") {
			t.Fatalf("outer logging lost principal: %v", audit)
		}
		if tt.token == "invalid" && audit["subject"] != "" {
			t.Fatal("unverified identity logged")
		}
	}

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			r := httptest.NewRequestWithContext(t.Context(), "METHOD"+strconv.Itoa(i), "/v1/environments/private-name-"+strconv.Itoa(i)+"?private-query", http.NoBody)
			r.Header.Set("Authorization", "Bearer Acme")
			r.Header.Set("X-Request-ID", "unsafe\nvalue")
			router.ServeHTTP(httptest.NewRecorder(), r)
		})
	}
	wg.Wait()

	rec := httptest.NewRecorder()
	ops.metrics.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", http.NoBody))

	for _, secret := range []string{"body-secret", "source-secret", "private-query", "private-name-", "unsafe\\n", "sensitive", "Bearer", "Acme", "Other"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("leaked %q", secret)
		}
	}
	for _, label := range []string{`route="/v1/environments/{name}"`, `method="OTHER"`, `status="201"`, `le="0.5"`} {
		if !strings.Contains(rec.Body.String(), label) {
			t.Fatalf("missing %s", label)
		}
	}
	if strings.Contains(rec.Body.String(), "safe-name") || strings.Contains(rec.Body.String(), "request-0") || strings.Contains(rec.Body.String(), `subject=`) {
		t.Fatal("unbounded metrics")
	}

	for _, tt := range []struct {
		method, path string
		status       int
	}{{"GET", "/other", 404}, {"POST", "/metrics", 405}, {"HEAD", "/metrics", 200}} {
		r := httptest.NewRecorder()
		ops.metrics.ServeHTTP(r, httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, http.NoBody))
		if r.Code != tt.status {
			t.Fatalf("metrics %s %s: %d", tt.method, tt.path, r.Code)
		}
	}

	for _, token := range []string{"", "Acme"} {
		r := httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", http.NoBody)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)
		want := 401
		if token != "" {
			want = 404
		}
		if rec.Code != want {
			t.Fatal("public metrics auth bypass")
		}
	}
}

func TestResponseWriter(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	w := &responseWriter{ResponseWriter: rec}
	w.WriteHeader(http.StatusCreated)
	w.WriteHeader(http.StatusInternalServerError)
	if _, err := w.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Fatal(err)
	}
	if w.status != 201 || rec.Code != 201 || !rec.Flushed {
		t.Fatal("writer capabilities or status lost")
	}
	if got := auditIdentity("subject\n" + strings.Repeat("x", 300)); !strings.HasPrefix(got, "sha256:") || len(got) != 71 {
		t.Fatal("unbounded identity")
	}
}

func TestOperationsFailures(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"forbidden", "conflict", "serialization", "transport", "panic"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			ops := newOperations(slog.New(slog.NewJSONHandler(&logs, nil)), "management")
			scheme := runtime.NewScheme()
			if err := v1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}

			cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					switch failure {
					case "forbidden":
						return apierrors.NewForbidden(schema.GroupResource{Group: "core.petri.run", Resource: "ephemeralenvironments"}, obj.GetName(), errors.New("private-kube-error"))
					case "conflict":
						return apierrors.NewAlreadyExists(schema.GroupResource{Group: "core.petri.run", Resource: "ephemeralenvironments"}, obj.GetName())
					case "serialization":
						obj.(*v1alpha1.EphemeralEnvironment).Spec.Values = map[string]string{"private": strings.Repeat("x", httpjson.MaxResponseBytes)}
					case "panic":
						panic("private-panic-payload")
					}
					return c.Create(ctx, obj, opts...)
				},
			}).Build()

			router := ops.middleware(newRouter(&authConfig{verifier: auth.NewTokenVerifier("private-test-token"), policy: auth.NewDevelopmentPolicy()}, handler.NewEnvironmentHandlerInNamespace(cl, "management"), nil, func(context.Context) error { return nil }))
			req := httptest.NewRequestWithContext(t.Context(), "POST", "/v1/environments", strings.NewReader(`{"name":"safe-name","template":"template"}`))
			req.Header.Set("Authorization", "Bearer private-test-token")
			rec := httptest.NewRecorder()

			var writer http.ResponseWriter = rec
			if failure == "transport" {
				writer = failingWriter{rec}
			}

			func() {
				defer func() {
					p := recover()
					if (failure == "panic" && !errors.Is(p.(error), http.ErrAbortHandler)) || (failure != "panic" && p != nil) {
						t.Errorf("unexpected panic: %v", p)
					}
				}()
				router.ServeHTTP(writer, req)
			}()

			want, result := 500, "failed"
			switch failure {
			case "conflict":
				want, result = 409, "rejected"
			case "serialization":
				result = "created"
			case "transport":
				want, result = 201, "created"
			}

			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 2 || rec.Code != want {
				t.Fatalf("status %d, logs %s", rec.Code, logs.String())
			}

			var audit map[string]any
			if err := json.Unmarshal([]byte(lines[1]), &audit); err != nil {
				t.Fatal(err)
			}
			if audit["status"] != float64(want) || audit["result"] != result || audit["subject"] != "static-token" {
				t.Fatalf("incorrect audit: %v", audit)
			}

			responseFailed, _ := audit["response_failed"].(bool)
			if (failure == "transport" || failure == "panic") && !responseFailed {
				t.Fatal("missing response failure")
			}
			if strings.Contains(logs.String(), "private-") || strings.Contains(rec.Body.String(), "private-") {
				t.Fatal("secret leaked")
			}
		})
	}
}

type failingWriter struct{ http.ResponseWriter }

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("private-transport-error") }

func TestServeDrainsBothListeners(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	addresses := make(chan string, 2)
	entered, stopped := make(chan struct{}, 2), make(chan struct{}, 2)
	completed := make(chan error, 2)
	release := make(chan struct{})
	defer close(release)

	servers := make([]*http.Server, 2)
	for i := range servers {
		servers[i] = &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second,
			BaseContext: func(l net.Listener) context.Context { addresses <- l.Addr().String(); return context.Background() },
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
				}
				w.WriteHeader(http.StatusNoContent)
			}),
		}
		servers[i].RegisterOnShutdown(func() { stopped <- struct{}{} })
	}

	result := make(chan error, 1)
	go func() { result <- serve(ctx, servers...) }()

	for range 2 {
		select {
		case address := <-addresses:
			go func() {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address, http.NoBody)
				if err != nil {
					completed <- err
					return
				}
				response, err := http.DefaultClient.Do(req)
				if err == nil {
					_ = response.Body.Close()
					if response.StatusCode != http.StatusNoContent {
						err = errors.New("in-flight response lost")
					}
				}
				completed <- err
			}()
		case <-time.After(time.Second):
			t.Fatal("listener did not start")
		}
	}

	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("request did not enter")
		}
	}

	cancel()
	for range 2 {
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("both listeners must stop accepting before draining")
		}
	}
	select {
	case <-result:
		t.Fatal("shutdown did not drain requests")
	default:
	}

	// Release both in-flight handlers, then require clean shutdown.
	release <- struct{}{}
	release <- struct{}{}
	for range 2 {
		select {
		case err := <-completed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("request did not complete")
		}
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish")
	}
}

func TestServeLifecycle(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	first, second := &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second}, &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second}
	started := make(chan struct{}, 2)
	for _, server := range []*http.Server{first, second} {
		server.BaseContext = func(net.Listener) context.Context { started <- struct{}{}; return context.Background() }
	}

	stopped := make(chan struct{}, 2)
	first.RegisterOnShutdown(func() { stopped <- struct{}{} })
	second.RegisterOnShutdown(func() { stopped <- struct{}{} })

	result := make(chan error, 1)
	go func() { result <- serve(ctx, first, second) }()

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("listener not started")
		}
	}

	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}

	for range 2 {
		select {
		case <-stopped:
		case <-time.After(time.Second):
			t.Fatal("listener not shut down")
		}
	}

	occupied, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()
	if err := serve(t.Context(), &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second}, &http.Server{Addr: occupied.Addr().String(), ReadHeaderTimeout: time.Second}); err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatal("bind failure was not fatal")
	}

	broken := &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second, BaseContext: func(l net.Listener) context.Context {
		_ = l.Close()
		return context.Background()
	}}
	go func() {
		result <- serve(t.Context(), broken, &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second})
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("runtime listener failure was not fatal")
		}
	case <-time.After(time.Second):
		t.Fatal("listener failure did not stop both servers")
	}
}

func TestServeShutdownDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	address := make(chan string, 1)
	entered, exited := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second,
		BaseContext: func(l net.Listener) context.Context { address <- l.Addr().String(); return context.Background() },
		Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
			close(exited)
		}),
	}

	result, requestDone := make(chan error, 1), make(chan struct{})
	go func() { result <- serve(ctx, srv) }()

	var host string
	select {
	case host = <-address:
	case <-time.After(time.Second):
		t.Fatal("listener did not start")
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+host, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		response, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = response.Body.Close()
		}
		close(requestDone)
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not enter")
	}

	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("shutdown timeout must exit nonzero")
		}
	case <-time.After(7 * time.Second):
		t.Fatal("shutdown unbounded")
	}

	for _, done := range []chan struct{}{exited, requestDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("forced close did not cancel active request")
		}
	}
}
