package auth

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/petri-dev/petri-apiserver/internal/httpjson"
	"github.com/petri-dev/petri-apiserver/internal/requeststate"
)

type Verifier interface {
	Verify(ctx context.Context, rawToken string) (Principal, error)
}

type Principal struct {
	Issuer       string
	Subject      string
	Groups       []string
	Organization string
	Repository   string
}

type principalKey struct{}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	p.Groups = slices.Clone(p.Groups)
	return p, ok
}

func Middleware(v Verifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			httpjson.Error(w, http.StatusUnauthorized, "unauthenticated", "missing authorization header")
			return
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
			httpjson.Error(w, http.StatusUnauthorized, "unauthenticated", "invalid authorization header format")
			return
		}

		rawToken := parts[1]
		principal, err := v.Verify(r.Context(), rawToken)
		if err != nil || principal.Issuer == "" || principal.Subject == "" {
			httpjson.Error(w, http.StatusUnauthorized, "unauthenticated", "authentication failed")
			return
		}

		if state := requeststate.FromContext(r.Context()); state != nil {
			state.Issuer, state.Subject = principal.Issuer, principal.Subject
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
	})
}
