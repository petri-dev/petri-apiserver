package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/petri-dev/petri-apiserver/internal/auth"
	"github.com/petri-dev/petri-apiserver/internal/handler"
	"github.com/petri-dev/petri-apiserver/internal/httpjson"
	"github.com/petri-dev/petri-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	pathEnvironments = "/v1/environments"
	pathTemplates    = "/v1/templates"
	pathHealthz      = "/healthz"
	pathReadyz       = "/readyz"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("server stopped", "reason", err.Error())
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	config, err := loadConfig(ctx)
	if err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	if schemeErr := v1alpha1.AddToScheme(scheme); schemeErr != nil {
		return errors.New("could not initialize Kubernetes scheme")
	}

	cfg, err := ctrl.GetConfig()
	if err != nil {
		return errors.New("could not load Kubernetes configuration")
	}
	cfg.Timeout = kubeClientTimeout

	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return errors.New("could not initialize Kubernetes client")
	}

	dependencies, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return errors.New("could not initialize readiness client")
	}

	envHandler := handler.NewEnvironmentHandlerInNamespace(cl, config.namespace)
	tmplHandler := handler.NewTemplateHandlerInNamespace(cl, config.namespace)
	ops := newOperations(slog.Default(), config.namespace)

	srv := &http.Server{
		Addr: config.addr,
		Handler: ops.middleware(
			newRouter(config.security, envHandler, tmplHandler, dependencyCheck(dependencies, config.namespace)),
		),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       apiReadTimeout,
		WriteTimeout:      apiWriteTimeout,
		IdleTimeout:       idleTimeout,
	}
	metrics := &http.Server{
		Addr:              config.metricsAddr,
		Handler:           ops.metrics,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       metricsReadTimeout,
		WriteTimeout:      metricsReadTimeout,
		IdleTimeout:       idleTimeout,
	}

	return serve(ctx, srv, metrics)
}

func serve(ctx context.Context, servers ...*http.Server) error {
	listeners := make([]net.Listener, 0, len(servers))
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()

	for _, server := range servers {
		listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", server.Addr)
		if err != nil {
			return errors.New("could not bind HTTP listener")
		}
		listeners = append(listeners, listener)
	}

	results := make(chan error, len(servers))
	for i, server := range servers {
		go func() { results <- server.Serve(listeners[i]) }()
	}

	slog.Info("server started", "listeners", len(servers), "log_format", "json")

	remaining := len(servers)
	var result error
	select {
	case <-ctx.Done():
	case <-results:
		remaining--
		result = errors.New("HTTP listener stopped unexpectedly")
	}
	slog.Info("server stopping")

	return shutdownAll(servers, results, remaining, result)
}

func shutdownAll(servers []*http.Server, results <-chan error, remaining int, result error) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTime)
	defer cancel()

	shutdowns := make(chan error, len(servers))
	for _, server := range servers {
		go func() {
			err := server.Shutdown(shutdownCtx)
			if err != nil {
				_ = server.Close()
			}
			shutdowns <- err
		}()
	}
	for range servers {
		if err := <-shutdowns; err != nil {
			result = errors.New("HTTP shutdown deadline exceeded")
		}
	}
	for range remaining {
		<-results
	}

	return result
}

func newRouter(
	security *authConfig,
	envHandler, tmplHandler http.Handler,
	ready func(context.Context) error,
) http.Handler {
	protected := auth.Middleware(
		security.verifier,
		security.policy.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.EscapedPath() != r.URL.Path {
				httpjson.NotFound(w)
				return
			}
			switch {
			case r.URL.Path == pathEnvironments || strings.HasPrefix(r.URL.Path, pathEnvironments+"/"):
				envHandler.ServeHTTP(w, r)
			case r.URL.Path == pathTemplates || strings.HasPrefix(r.URL.Path, pathTemplates+"/"):
				tmplHandler.ServeHTTP(w, r)
			default:
				httpjson.NotFound(w)
			}
		})),
	)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == r.URL.Path && (r.URL.Path == pathHealthz || r.URL.Path == pathReadyz) {
			serveHealthCheck(w, r, ready)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

func serveHealthCheck(w http.ResponseWriter, r *http.Request, ready func(context.Context) error) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		httpjson.MethodNotAllowed(w, "GET, HEAD")
		return
	}
	if r.URL.Path == pathReadyz && ready(r.Context()) != nil {
		httpjson.Error(w, http.StatusServiceUnavailable, "internal_error", "dependencies unavailable")
		return
	}
	w.WriteHeader(http.StatusOK)
}
