package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const oidcHTTPTimeout = 15 * time.Second

type OIDCVerifier struct {
	verifier *oidc.IDTokenVerifier
	issuer   string
	claims   ClaimNames
}

type ClaimNames struct {
	Groups       string
	Organization string
	Repository   string
}

func NewOIDCVerifier(ctx context.Context, issuerURL, audience string, claims ClaimNames) (*OIDCVerifier, error) {
	if issuerURL == "" || audience == "" {
		return nil, errors.New("OIDC issuer and audience are required")
	}

	ctx = oidc.ClientContext(ctx, &http.Client{Timeout: oidcHTTPTimeout})
	discoveryCtx, cancel := context.WithTimeout(ctx, oidcHTTPTimeout)
	defer cancel()

	provider, err := oidc.NewProvider(discoveryCtx, issuerURL)
	if err != nil {
		return nil, errors.New("OIDC discovery failed")
	}

	if jwksErr := validateJWKSURL(provider, issuerURL); jwksErr != nil {
		return nil, jwksErr
	}

	return &OIDCVerifier{
		verifier: provider.VerifierContext(ctx, &oidc.Config{ClientID: audience}),
		issuer:   issuerURL,
		claims:   claims,
	}, nil
}

func validateJWKSURL(provider *oidc.Provider, issuerURL string) error {
	var metadata struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return errors.New("invalid OIDC discovery metadata")
	}

	u, err := url.Parse(metadata.JWKSURL)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" ||
		(u.Scheme != "https" && (u.Scheme != "http" || !strings.HasPrefix(issuerURL, "http://"))) {
		return errors.New("OIDC discovery requires a valid JWKS URL with secure transport for HTTPS issuers")
	}
	return nil
}

func (v *OIDCVerifier) Verify(ctx context.Context, rawToken string) (Principal, error) {
	token, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return Principal{}, errors.New("token verification failed")
	}
	if token.Issuer != v.issuer || token.Subject == "" || token.Expiry.IsZero() {
		return Principal{}, errors.New("required identity claims are invalid")
	}

	var rawClaims map[string]any
	if claimsErr := token.Claims(&rawClaims); claimsErr != nil {
		return Principal{}, errors.New("invalid identity claims")
	}
	if stdErr := validateStandardClaims(rawClaims); stdErr != nil {
		return Principal{}, stdErr
	}

	p := Principal{Issuer: token.Issuer, Subject: token.Subject}
	if customErr := v.extractCustomClaims(rawClaims, &p); customErr != nil {
		return Principal{}, customErr
	}
	return p, nil
}

func validateStandardClaims(claims map[string]any) error {
	if _, ok := claims["exp"].(float64); !ok {
		return errors.New("expiry must be a numeric date")
	}
	audiences, ok := claims["aud"].([]any)
	if !ok {
		return nil
	}
	for _, audience := range audiences {
		if _, isStr := audience.(string); !isStr {
			return errors.New("invalid audience claim")
		}
	}
	return nil
}

func (v *OIDCVerifier) extractCustomClaims(claims map[string]any, p *Principal) error {
	if err := v.extractGroups(claims, p); err != nil {
		return err
	}

	for _, field := range []struct {
		name string
		dest *string
	}{
		{v.claims.Organization, &p.Organization},
		{v.claims.Repository, &p.Repository},
	} {
		if field.name == "" {
			continue
		}
		raw, exists := claims[field.name]
		if !exists {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return errors.New("invalid selected claim type")
		}
		*field.dest = value
	}

	return nil
}

func (v *OIDCVerifier) extractGroups(claims map[string]any, p *Principal) error {
	if v.claims.Groups == "" {
		return nil
	}

	raw, exists := claims[v.claims.Groups]
	if !exists {
		return nil
	}

	groups, ok := raw.([]any)
	if !ok {
		return errors.New("invalid groups claim")
	}
	p.Groups = make([]string, 0, len(groups))
	for _, group := range groups {
		name, isStr := group.(string)
		if !isStr {
			return errors.New("invalid groups claim")
		}
		p.Groups = append(p.Groups, name)
	}

	return nil
}
