package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/petri-dev/petri-apiserver/internal/auth"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	authModeToken = "token"

	kubeClientTimeout    = 15 * time.Second
	readHeaderTimeout    = 5 * time.Second
	apiReadTimeout       = 15 * time.Second
	apiWriteTimeout      = 30 * time.Second
	idleTimeout          = 60 * time.Second
	metricsReadTimeout   = 10 * time.Second
	gracefulShutdownTime = 5 * time.Second
	readinessTimeout     = 2 * time.Second
	metricsScrapeTimeout = 5 * time.Second
	metricsMaxInflight   = 2
)

type serverConfig struct {
	namespace   string
	addr        string
	metricsAddr string
	security    *authConfig
}

type authConfig struct {
	verifier auth.Verifier
	policy   auth.Policy
}

func loadConfig(ctx context.Context) (*serverConfig, error) {
	cfg := &serverConfig{namespace: os.Getenv("PETRI_APISERVER_NAMESPACE")}
	if cfg.namespace == "" {
		cfg.namespace = "default"
	}
	if len(validation.IsDNS1123Label(cfg.namespace)) != 0 {
		return nil, errors.New("PETRI_APISERVER_NAMESPACE must be a DNS label")
	}

	var err error
	cfg.addr, err = listenerAddrFromEnv("PETRI_APISERVER_ADDR", ":8080")
	if err != nil {
		return nil, err
	}

	cfg.metricsAddr, err = listenerAddrFromEnv("PETRI_APISERVER_METRICS_ADDR", ":9090")
	if err != nil {
		return nil, err
	}

	cfg.security, err = authFromEnv(ctx)
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

func listenerAddrFromEnv(key, fallback string) (string, error) {
	addr := os.Getenv(key)
	if addr == "" {
		addr = fallback
	}

	if _, _, err := net.SplitHostPort(addr); err != nil {
		return "", fmt.Errorf("%s must be host:port", key)
	}

	return addr, nil
}

func authFromEnv(ctx context.Context) (*authConfig, error) {
	mode := os.Getenv("PETRI_APISERVER_AUTH_MODE")
	token := os.Getenv("PETRI_APISERVER_TOKEN")
	allowToken := os.Getenv("PETRI_APISERVER_ALLOW_INSECURE_TOKEN")
	issuer := os.Getenv("PETRI_APISERVER_OIDC_ISSUER")
	audience := os.Getenv("PETRI_APISERVER_OIDC_AUDIENCE")
	claims := auth.ClaimNames{
		Groups:       os.Getenv("PETRI_APISERVER_OIDC_GROUPS_CLAIM"),
		Organization: os.Getenv("PETRI_APISERVER_OIDC_ORGANIZATION_CLAIM"),
		Repository:   os.Getenv("PETRI_APISERVER_OIDC_REPOSITORY_CLAIM"),
	}

	if allowToken != "" && allowToken != "true" && allowToken != "false" {
		return nil, errors.New("PETRI_APISERVER_ALLOW_INSECURE_TOKEN must be true or false")
	}

	switch mode {
	case authModeToken:
		return buildTokenConfig(token, allowToken, issuer, audience, claims)
	case "", "oidc":
		if token != "" || allowToken == "true" {
			return nil, errors.New("OIDC mode cannot include static token credentials or opt-in")
		}

		return buildOIDCConfig(ctx, issuer, audience, claims)
	default:
		return nil, errors.New("unsupported PETRI_APISERVER_AUTH_MODE")
	}
}

func buildTokenConfig(token, allowToken, issuer, audience string, claims auth.ClaimNames) (*authConfig, error) {
	_, organizationsSet := os.LookupEnv("PETRI_APISERVER_ALLOWED_ORGANIZATIONS")
	_, repositoriesSet := os.LookupEnv("PETRI_APISERVER_ALLOWED_REPOSITORIES")
	if organizationsSet || repositoriesSet {
		return nil, errors.New(
			"token mode cannot include PETRI_APISERVER_ALLOWED_ORGANIZATIONS or PETRI_APISERVER_ALLOWED_REPOSITORIES; unset both; development access is namespace-wide",
		)
	}

	if allowToken != "true" || strings.TrimSpace(token) == "" {
		return nil, errors.New(
			"token mode requires PETRI_APISERVER_ALLOW_INSECURE_TOKEN=true and a nonempty PETRI_APISERVER_TOKEN; development only",
		)
	}

	if issuer != "" || audience != "" || claims != (auth.ClaimNames{}) {
		return nil, errors.New("token mode cannot include OIDC configuration")
	}

	return &authConfig{verifier: auth.NewTokenVerifier(token), policy: auth.NewDevelopmentPolicy()}, nil
}

func buildOIDCConfig(ctx context.Context, issuer, audience string, claims auth.ClaimNames) (*authConfig, error) {
	if err := validateOIDCIssuer(issuer, audience); err != nil {
		return nil, err
	}
	for _, name := range []string{claims.Groups, claims.Organization, claims.Repository} {
		if name != strings.TrimSpace(name) {
			return nil, errors.New("OIDC claim names must not have surrounding whitespace")
		}
	}

	policy, err := auth.NewPolicy(
		os.Getenv("PETRI_APISERVER_ALLOWED_ORGANIZATIONS"),
		os.Getenv("PETRI_APISERVER_ALLOWED_REPOSITORIES"),
	)
	if err != nil {
		return nil, fmt.Errorf("build policy: %w", err)
	}

	if validateErr := policy.ValidateClaims(claims); validateErr != nil {
		return nil, fmt.Errorf("validate policy claims: %w", validateErr)
	}
	verifier, err := auth.NewOIDCVerifier(ctx, issuer, audience, claims)
	if err != nil {
		return nil, fmt.Errorf("create OIDC verifier: %w", err)
	}

	return &authConfig{verifier: verifier, policy: policy}, nil
}

func validateOIDCIssuer(issuer, audience string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		strings.TrimSpace(audience) == "" {
		return errors.New(
			"OIDC requires an HTTPS issuer without credentials, query or fragment and a nonempty audience",
		)
	}

	return nil
}
