package main

import (
	"os"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv("PETRI_APISERVER_AUTH_MODE", "token")
	t.Setenv("PETRI_APISERVER_ALLOW_INSECURE_TOKEN", "true")
	t.Setenv("PETRI_APISERVER_TOKEN", "test-token")
	for _, suffix := range []string{
		"NAMESPACE", "ADDR", "METRICS_ADDR", "OIDC_ISSUER", "OIDC_AUDIENCE",
		"OIDC_GROUPS_CLAIM", "OIDC_ORGANIZATION_CLAIM", "OIDC_REPOSITORY_CLAIM",
		"ALLOWED_ORGANIZATIONS", "ALLOWED_REPOSITORIES",
	} {
		key := "PETRI_APISERVER_" + suffix
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := loadConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.namespace != "default" || cfg.addr != ":8080" || cfg.metricsAddr != ":9090" {
		t.Fatal("incorrect configuration defaults")
	}
	if _, err := cfg.security.verifier.Verify(t.Context(), "test-token"); err != nil {
		t.Fatal("configuration did not initialize authentication")
	}

	t.Setenv("PETRI_APISERVER_NAMESPACE", "managed")
	t.Setenv("PETRI_APISERVER_ADDR", "127.0.0.1:8181")
	t.Setenv("PETRI_APISERVER_METRICS_ADDR", "[::1]:9191")
	cfg, err = loadConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.namespace != "managed" || cfg.addr != "127.0.0.1:8181" || cfg.metricsAddr != "[::1]:9191" {
		t.Fatal("configuration ignored environment overrides")
	}

	for _, suffix := range []string{"NAMESPACE", "ADDR", "METRICS_ADDR", "AUTH_MODE"} {
		t.Run(suffix, func(t *testing.T) {
			key := "PETRI_APISERVER_" + suffix
			t.Setenv(key, "INVALID")
			cfg, err := loadConfig(t.Context())
			if cfg != nil || err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("expected configuration error naming %s, got %v", key, err)
			}
		})
	}
}
