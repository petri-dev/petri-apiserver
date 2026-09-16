package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/petri-dev/petri-apiserver/internal/auth"
)

func TestListenerAddressConfiguration(t *testing.T) {
	t.Setenv("PETRI_APISERVER_ADDR", "")
	t.Setenv("PETRI_APISERVER_METRICS_ADDR", "")
	for key, fallback := range map[string]string{
		"PETRI_APISERVER_ADDR":         ":8080",
		"PETRI_APISERVER_METRICS_ADDR": ":9090",
	} {
		for _, value := range []string{"", "127.0.0.1:8181", "[::1]:8181", "invalid-address"} {
			t.Setenv(key, value)
			addr, err := listenerAddrFromEnv(key, fallback)
			if value == "invalid-address" {
				if err == nil || err.Error() != key+" must be host:port" {
					t.Fatalf("%s: expected sanitized configuration error, got %v", key, err)
				}
				continue
			}
			want := value
			if want == "" {
				want = fallback
			}
			if err != nil || addr != want {
				t.Fatalf("%s: address=%q, error=%v; want %q", key, addr, err, want)
			}
		}
	}
}

func TestAuthConfiguration(t *testing.T) {
	for _, key := range []string{"PETRI_APISERVER_ALLOWED_ORGANIZATIONS", "PETRI_APISERVER_ALLOWED_REPOSITORIES"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	keys := []string{"AUTH_MODE", "TOKEN", "ALLOW_INSECURE_TOKEN", "OIDC_ISSUER", "OIDC_AUDIENCE", "OIDC_GROUPS_CLAIM", "OIDC_ORGANIZATION_CLAIM", "OIDC_REPOSITORY_CLAIM"}
	for _, tt := range []struct {
		name  string
		env   map[string]string
		valid bool
	}{
		{"default requires OIDC", nil, false},
		{"unknown mode", map[string]string{"AUTH_MODE": "proxy"}, false},
		{"token requires opt-in", map[string]string{"AUTH_MODE": "token", "TOKEN": "secret"}, false},
		{"token requires secret", map[string]string{"AUTH_MODE": "token", "ALLOW_INSECURE_TOKEN": "true"}, false},
		{"token whitespace", map[string]string{"AUTH_MODE": "token", "ALLOW_INSECURE_TOKEN": "true", "TOKEN": " "}, false},
		{"token explicit false", map[string]string{"AUTH_MODE": "token", "ALLOW_INSECURE_TOKEN": "false", "TOKEN": "secret"}, false},
		{"invalid opt-in", map[string]string{"ALLOW_INSECURE_TOKEN": "yes"}, false},
		{"development token", map[string]string{"AUTH_MODE": "token", "ALLOW_INSECURE_TOKEN": "true", "TOKEN": "secret"}, true},
		{"mixed issuer", map[string]string{"AUTH_MODE": "token", "ALLOW_INSECURE_TOKEN": "true", "TOKEN": "secret", "OIDC_ISSUER": "https://issuer.example"}, false},
		{"mixed claims", map[string]string{"AUTH_MODE": "token", "ALLOW_INSECURE_TOKEN": "true", "TOKEN": "secret", "OIDC_GROUPS_CLAIM": "groups"}, false},
		{"no token fallback", map[string]string{"TOKEN": "secret"}, false},
		{"OIDC rejects opt-in", map[string]string{"AUTH_MODE": "oidc", "ALLOW_INSECURE_TOKEN": "true"}, false},
		{"HTTP issuer", map[string]string{"OIDC_ISSUER": "http://issuer.example", "OIDC_AUDIENCE": "petri"}, false},
		{"issuer credentials", map[string]string{"OIDC_ISSUER": "https://user:secret@issuer.example", "OIDC_AUDIENCE": "petri"}, false},
		{"issuer query", map[string]string{"OIDC_ISSUER": "https://issuer.example?secret=1", "OIDC_AUDIENCE": "petri"}, false},
		{"missing audience", map[string]string{"OIDC_ISSUER": "https://issuer.example"}, false},
		{"claim whitespace", map[string]string{"OIDC_ISSUER": "https://issuer.example", "OIDC_AUDIENCE": "petri", "OIDC_GROUPS_CLAIM": " groups "}, false},
		{"OIDC discovery fails without fallback", map[string]string{"OIDC_ISSUER": "https://issuer.example", "OIDC_AUDIENCE": "petri", "OIDC_ORGANIZATION_CLAIM": "company"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range keys {
				t.Setenv("PETRI_APISERVER_"+key, tt.env[key])
			}
			if tt.name == "OIDC discovery fails without fallback" {
				t.Setenv("PETRI_APISERVER_ALLOWED_ORGANIZATIONS", "Acme")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel() // Valid OIDC configuration must reach discovery, without external traffic.
			v, err := authFromEnv(ctx)
			if (err == nil) != tt.valid {
				t.Fatalf("verifier %T, error %v", v, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("configuration leaked credentials")
			}
			if tt.name == "OIDC discovery fails without fallback" && !strings.Contains(err.Error(), "OIDC discovery failed") {
				t.Fatalf("did not reach discovery: %v", err)
			}
			if tt.valid {
				if _, err := v.verifier.Verify(t.Context(), "secret"); err != nil {
					t.Fatal(err)
				}
				called := false
				h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					called = true
					w.WriteHeader(http.StatusNoContent)
				})
				req := httptest.NewRequestWithContext(t.Context(), "GET", "/v1/environments", http.NoBody)
				req.Header.Set("Authorization", "Bearer secret")
				rec := httptest.NewRecorder()
				newRouter(v, h, h, func(context.Context) error { return nil }).ServeHTTP(rec, req)
				if rec.Code != http.StatusNoContent || !called {
					t.Fatal("startup configuration did not wire development authorization")
				}
			} else if v != nil {
				t.Fatal("invalid configuration returned a verifier")
			}
		})
	}
}

func TestPolicyConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name, organizations, repositories, organizationClaim, repositoryClaim, mode, want string
	}{
		{name: "empty", want: "requires a nonempty"},
		{name: "whitespace empty", organizations: " \t", want: "requires a nonempty"},
		{name: "organization mapping required", organizations: "Acme", want: "requires PETRI_APISERVER_OIDC_ORGANIZATION_CLAIM"},
		{name: "repository mapping required", repositories: "Acme/service", want: "requires PETRI_APISERVER_OIDC_REPOSITORY_CLAIM"},
		{name: "both mappings required", organizations: "Acme", repositories: "Other/service", organizationClaim: "company", want: "requires PETRI_APISERVER_OIDC_REPOSITORY_CLAIM"},
		{name: "organization only", organizations: " Acme, Other ", organizationClaim: "company", want: "OIDC discovery failed"},
		{name: "repository only", repositories: "Acme/service", repositoryClaim: "project", want: "OIDC discovery failed"},
		{name: "both", organizations: "Acme", repositories: "Other/service", organizationClaim: "company", repositoryClaim: "project", want: "OIDC discovery failed"},
		{name: "malformed organization", organizations: "Acme/*", organizationClaim: "company", want: "invalid entry"},
		{name: "malformed repository", repositories: "service", repositoryClaim: "project", want: "invalid entry"},
		{name: "static organization", mode: "token", organizations: "Acme", want: "token mode cannot include PETRI_APISERVER_ALLOWED"},
		{name: "static repository", mode: "token", repositories: "Acme/service", want: "token mode cannot include PETRI_APISERVER_ALLOWED"},
		{name: "static empty policy variables", mode: "token", want: "token mode cannot include PETRI_APISERVER_ALLOWED"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range map[string]string{
				"AUTH_MODE": tt.mode, "TOKEN": "", "ALLOW_INSECURE_TOKEN": "", "OIDC_GROUPS_CLAIM": "",
				"OIDC_ISSUER": "https://issuer.example", "OIDC_AUDIENCE": "petri",
				"ALLOWED_ORGANIZATIONS": tt.organizations, "ALLOWED_REPOSITORIES": tt.repositories,
				"OIDC_ORGANIZATION_CLAIM": tt.organizationClaim, "OIDC_REPOSITORY_CLAIM": tt.repositoryClaim,
			} {
				t.Setenv("PETRI_APISERVER_"+key, value)
			}
			if tt.mode == "token" {
				t.Setenv("PETRI_APISERVER_TOKEN", "secret")
				t.Setenv("PETRI_APISERVER_ALLOW_INSECURE_TOKEN", "true")
				t.Setenv("PETRI_APISERVER_OIDC_ISSUER", "")
				t.Setenv("PETRI_APISERVER_OIDC_AUDIENCE", "")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			cfg, err := authFromEnv(ctx)
			if cfg != nil || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q, got config %v, error %v", tt.want, cfg, err)
			}
		})
	}
}

type routerVerifier struct{}

func (routerVerifier) Verify(_ context.Context, raw string) (auth.Principal, error) {
	if raw == "invalid" {
		return auth.Principal{}, errors.New("sensitive verifier error")
	}
	return auth.Principal{Issuer: "issuer", Subject: "user", Organization: raw}, nil
}

func TestRouterAuthorization(t *testing.T) {
	t.Parallel()
	policy, err := auth.NewPolicy("Acme", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, security := range []*authConfig{
		{verifier: routerVerifier{}, policy: policy},
		{verifier: auth.NewTokenVerifier("Acme"), policy: auth.NewDevelopmentPolicy()},
	} {
		for _, path := range []string{"/v1/environments", "/v1/environments/", "/v1/environments/shared", "/v1/environments/shared/", "/v1/templates", "/v1/templates/", "/v1/templates/shared", "/v1/templates/shared/", "/v1/%65nvironments", "/v1/environments%2fshared", "/healthz/", "/%68ealthz", "/v1/../healthz", "/unknown", "/environments", "/templates", "/healthz", "/readyz"} {
			for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
				for _, token := range []string{"", "invalid", "Other", "Acme"} {
					called := false
					downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						called = true
						if _, ok := auth.PrincipalFromContext(r.Context()); !ok {
							t.Error("handler missing principal")
						}
						w.WriteHeader(http.StatusNoContent)
					})
					router := newRouter(security, downstream, downstream, func(context.Context) error { return nil })
					req := httptest.NewRequestWithContext(t.Context(), method, path, http.NoBody)
					if token != "" {
						req.Header.Set("Authorization", "Bearer "+token)
					}
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, req)
					want, wantCalled := http.StatusUnauthorized, false
					switch token {
					case "Acme":
						want, wantCalled = http.StatusNoContent, true
						if req.URL.EscapedPath() != req.URL.Path || (!strings.HasPrefix(path, "/v1/environments") && !strings.HasPrefix(path, "/v1/templates")) {
							want, wantCalled = http.StatusNotFound, false
						}
					case "Other":
						if _, static := security.verifier.(*auth.TokenVerifier); !static {
							want = http.StatusForbidden
						}
					}
					if path == "/healthz" || path == "/readyz" {
						want, wantCalled = http.StatusMethodNotAllowed, false
						if method == "GET" {
							want = http.StatusOK
						}
					}
					if rec.Code != want || called != wantCalled || strings.Contains(rec.Body.String(), "sensitive") {
						t.Fatalf("%T %s %s token %q: status %d, called %t; want %d, %t", security.verifier, method, path, token, rec.Code, called, want, wantCalled)
					}
					if want >= 400 {
						var response struct {
							Error struct{ Code, Message string }
						}
						codes := map[int]string{401: "unauthenticated", 403: "forbidden", 404: "not_found", 405: "method_not_allowed"}
						if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.Error.Code != codes[want] || response.Error.Message == "" || rec.Header().Get("Content-Type") != "application/json" {
							t.Fatalf("invalid error envelope: %s", rec.Body.String())
						}
					}
				}
			}
		}
	}
}
