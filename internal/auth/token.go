package auth

import (
	"context"
	"crypto/subtle"
	"errors"
)

const (
	devIssuer  = "urn:petri:development"
	devSubject = "static-token"

	envAllowedOrganizations = "PETRI_APISERVER_ALLOWED_ORGANIZATIONS"
	envAllowedRepositories  = "PETRI_APISERVER_ALLOWED_REPOSITORIES"
)

type TokenVerifier struct {
	token []byte
}

func NewTokenVerifier(token string) *TokenVerifier {
	return &TokenVerifier{token: []byte(token)}
}

func (v *TokenVerifier) Verify(_ context.Context, rawToken string) (Principal, error) {
	if len(v.token) == 0 || subtle.ConstantTimeCompare(v.token, []byte(rawToken)) != 1 {
		return Principal{}, errors.New("invalid token")
	}

	return Principal{Issuer: devIssuer, Subject: devSubject}, nil
}
