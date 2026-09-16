package auth

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/petri-dev/petri-apiserver/internal/httpjson"
)

// Policy grants namespace-wide access, never per-resource ownership. Its zero value denies every request. Construct once before serving requests.
type Policy struct {
	organizations []string
	repositories  []string
	development   bool
}

// NewDevelopmentPolicy is only for explicitly opted-in static token mode.
func NewDevelopmentPolicy() Policy { return Policy{development: true} }

// NewPolicy parses comma-separated lists, trimming whitespace around entries only.
// Each name component uses ASCII letters, digits, dot, underscore or hyphen, starting with a letter or digit. Repositories have exactly two components.
func NewPolicy(organizations, repositories string) (Policy, error) {
	p := Policy{}
	component := `[A-Za-z0-9][A-Za-z0-9._-]*`
	for _, list := range []struct {
		raw, pattern, variable string
		dest                   *[]string
	}{
		{organizations, `^` + component + `$`, envAllowedOrganizations, &p.organizations},
		{repositories, `^` + component + `/` + component + `$`, envAllowedRepositories, &p.repositories},
	} {
		if strings.TrimSpace(list.raw) == "" {
			continue
		}
		valid := regexp.MustCompile(list.pattern)
		for _, entry := range strings.Split(list.raw, ",") {
			entry = strings.TrimSpace(entry)
			if !valid.MatchString(entry) {
				return Policy{}, errors.New(
					list.variable + ": invalid entry; use comma-separated names (repositories: organization/repository), without empty entries or wildcards",
				)
			}
			*list.dest = append(*list.dest, entry)
		}
	}

	if len(p.organizations)+len(p.repositories) == 0 {
		return Policy{}, errors.New(
			"OIDC requires a nonempty " + envAllowedOrganizations + " or " + envAllowedRepositories,
		)
	}

	return p, nil
}

func (p Policy) ValidateClaims(claims ClaimNames) error {
	if len(p.organizations) > 0 && claims.Organization == "" {
		return errors.New(envAllowedOrganizations + " requires PETRI_APISERVER_OIDC_ORGANIZATION_CLAIM")
	}
	if len(p.repositories) > 0 && claims.Repository == "" {
		return errors.New(envAllowedRepositories + " requires PETRI_APISERVER_OIDC_REPOSITORY_CLAIM")
	}
	return nil
}

// Middleware must run after authentication. Missing or invalid principals fail closed.
func (p Policy) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := PrincipalFromContext(r.Context())
		if !ok || principal.Issuer == "" || principal.Subject == "" {
			httpjson.Error(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
			return
		}

		allowed := slices.Contains(p.organizations, principal.Organization) ||
			slices.Contains(p.repositories, principal.Repository)
		if p.development {
			allowed = principal.Issuer == devIssuer && principal.Subject == devSubject
		}
		if !allowed {
			httpjson.Error(w, http.StatusForbidden, "forbidden", "access denied")
			return
		}

		next.ServeHTTP(w, r)
	})
}
