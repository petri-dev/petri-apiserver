package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type failingVerifier struct{}

type principalVerifier struct{ principal Principal }

func (v principalVerifier) Verify(context.Context, string) (Principal, error) {
	return v.principal, nil
}

func (failingVerifier) Verify(context.Context, string) (Principal, error) {
	return Principal{}, errors.New("sensitive verifier details")
}

func TestTokenVerifier(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{name: "valid token", token: "my-secret-token"},
		{name: "invalid token", token: "wrong-token", wantErr: true},
		{name: "empty token", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewTokenVerifier("my-secret-token").Verify(context.Background(), tt.token)
			if (err != nil) != tt.wantErr {
				t.Fatalf("expected error %t, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestMiddleware(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		verifier   Verifier
		header     string
		wantStatus int
		wantCalled bool
		redact     string
	}{
		{name: "missing header", verifier: NewTokenVerifier("token"), wantStatus: http.StatusUnauthorized},
		{name: "invalid format", verifier: NewTokenVerifier("token"), header: "Basic dXNlcjpwYXNz", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", verifier: NewTokenVerifier("correct-token"), header: "Bearer wrong-token", wantStatus: http.StatusUnauthorized},
		{name: "valid token", verifier: NewTokenVerifier("correct-token"), header: "Bearer correct-token", wantStatus: http.StatusOK, wantCalled: true},
		{name: "bearer case insensitive", verifier: NewTokenVerifier("token"), header: "BEARER token", wantStatus: http.StatusOK, wantCalled: true},
		{name: "does not expose verifier error", verifier: failingVerifier{}, header: "Bearer token", wantStatus: http.StatusUnauthorized, redact: "sensitive verifier details"},
		{name: "empty principal", verifier: principalVerifier{}, header: "Bearer token", wantStatus: http.StatusUnauthorized},
		{name: "missing subject", verifier: principalVerifier{Principal{Issuer: "issuer", Organization: "Acme"}}, header: "Bearer token", wantStatus: http.StatusUnauthorized},
		{name: "missing issuer", verifier: principalVerifier{Principal{Subject: "user", Repository: "Acme/service"}}, header: "Bearer token", wantStatus: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			called := false
			handler := Middleware(tt.verifier, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p, ok := PrincipalFromContext(r.Context())
				if !ok || p.Issuer != "urn:petri:development" || p.Subject != "static-token" {
					t.Error("missing development principal")
				}
				called = true
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
			req.Header.Set("Authorization", tt.header)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("expected %d, got %d", tt.wantStatus, rec.Code)
			}
			if called != tt.wantCalled {
				t.Fatalf("expected next handler called %t, got %t", tt.wantCalled, called)
			}
			if tt.redact != "" && strings.Contains(rec.Body.String(), tt.redact) {
				t.Fatal("verifier error was exposed in the response")
			}
		})
	}
}
