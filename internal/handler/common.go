package handler

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/petri-dev/petri-apiserver/internal/httpjson"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultListLimit = 100
	maxListLimit     = 500
	maxContinueLen   = 8192

	pathEnvironments = "/v1/environments"
	pathTemplates    = "/v1/templates"
)

type page[T any] struct {
	Items    []T    `json:"items"`
	Continue string `json:"continue,omitempty"`
}

func invalid(w http.ResponseWriter, message string) {
	httpjson.Error(w, http.StatusBadRequest, "invalid_request", message)
}

func kubeError(w http.ResponseWriter, err error) {
	switch {
	case apierrors.IsNotFound(err):
		httpjson.NotFound(w)
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		httpjson.Error(w, http.StatusConflict, "conflict", "environment was modified concurrently")
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err), apierrors.IsResourceExpired(err), apierrors.IsGone(err):
		invalid(w, "request rejected; check fields or restart pagination")
	default:
		httpjson.Error(w, http.StatusInternalServerError, "internal_error", "Kubernetes operation failed")
	}
}

func listOptions(r *http.Request, namespace string) (*client.ListOptions, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errors.New("invalid query encoding")
	}

	opts := &client.ListOptions{Namespace: namespace, Limit: defaultListLimit}
	for key, values := range q {
		if len(values) != 1 {
			return nil, errors.New("query parameters must not be repeated")
		}
		err = applyListParam(opts, key, values[0])
		if err != nil {
			return nil, err
		}
	}

	return opts, nil
}

func applyListParam(opts *client.ListOptions, key, value string) error {
	switch key {
	case "limit":
		return applyLimitParam(opts, value)
	case "continue":
		if value == "" || len(value) > maxContinueLen {
			return errors.New("continue must be 1..8192 bytes")
		}
		opts.Continue = value
	default:
		return errors.New("unknown query parameter")
	}
	return nil
}

func applyLimitParam(opts *client.ListOptions, value string) error {
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return errors.New("limit must be an integer from 1 to 500")
	}
	limit, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil || limit < 1 || limit > maxListLimit {
		return errors.New("limit must be an integer from 1 to 500")
	}
	opts.Limit = limit
	return nil
}
