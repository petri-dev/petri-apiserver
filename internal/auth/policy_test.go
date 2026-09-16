package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPolicyParsing(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		organizations, repositories string
		valid                       bool
	}{
		{"", "", false}, {" \t", "\n", false},
		{" Acme, Other_Org , Acme ", " Other_Org/service.v2-1 ", true},
		{"Acme", "", true}, {"", "Acme/service", true},
		{"Acme,", "", false}, {",Acme", "", false}, {"Acme,,Other", "", false},
		{"Ac me", "", false}, {"Acme\nOther", "", false}, {"Acme/service", "", false},
		{"*", "", false}, {"Acme?", "", false}, {"[Acme]", "", false}, {".Acme", "", false},
		{"Acmé", "", false}, {"Acme", "*/service", false}, {"", "Acme/*", false},
		{"", "service", false}, {"", "Acme/", false}, {"", "/service", false},
		{"", "Acme/service/extra", false}, {"", "Acme /service", false},
		{"", "Acme/service,", false}, {"", "https://Acme/service", false},
	} {
		_, err := NewPolicy(tt.organizations, tt.repositories)
		if (err == nil) != tt.valid {
			t.Errorf("%q, %q: %v", tt.organizations, tt.repositories, err)
		}
		if err != nil && (strings.Contains(err.Error(), "Acme") || strings.Contains(err.Error(), "service")) {
			t.Error("error echoed policy entries")
		}
	}
}

func TestPolicyFailsClosed(t *testing.T) {
	t.Parallel()
	policy, err := NewPolicy("Acme", "Acme/service")
	if err != nil {
		t.Fatal(err)
	}

	for _, value := range []any{nil, "wrong type", Principal{}, Principal{Issuer: "issuer", Organization: "Acme"}, Principal{Subject: "user", Repository: "Acme/service"}} {
		ctx := t.Context()
		if value != nil {
			ctx = context.WithValue(ctx, principalKey{}, value)
		}
		for _, p := range []Policy{policy, {}, NewDevelopmentPolicy()} {
			rec := httptest.NewRecorder()
			p.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("invalid principal reached handler")
			})).ServeHTTP(rec, httptest.NewRequestWithContext(ctx, "GET", "/", http.NoBody))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("invalid principal: %d", rec.Code)
			}
		}
	}

	for _, p := range []Policy{policy, {}} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), "GET", "/", http.NoBody)
		req.Header.Set("Authorization", "Bearer token")
		Middleware(NewTokenVerifier("token"), p.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("static identity bypassed OIDC policy")
		}))).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("valid token without policy access: %d", rec.Code)
		}
	}

	for _, p := range []Policy{{}, NewDevelopmentPolicy()} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), "GET", "/", http.NoBody)
		req.Header.Set("Authorization", "Bearer token")
		v := principalVerifier{Principal{Issuer: "issuer", Subject: "user", Organization: "Acme", Repository: "Acme/service"}}
		Middleware(v, p.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("OIDC identity bypassed empty or development policy")
		}))).ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected deny-by-default: %d", rec.Code)
		}
	}
}
