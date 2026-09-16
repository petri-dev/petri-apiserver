package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/petri-dev/petri-apiserver/internal/httpjson"
	"github.com/petri-dev/petri-apiserver/internal/requeststate"
	"github.com/petri-dev/petri-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type EnvironmentRequest struct {
	Name     string            `json:"name"`
	Template string            `json:"template"`
	Source   *sourcePatch      `json:"source,omitempty"`
	Values   map[string]string `json:"values,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	TTL      *string           `json:"ttl,omitempty"`
}

type sourcePatch struct {
	Repo   *string `json:"repo,omitempty"`
	Branch *string `json:"branch,omitempty"`
	SHA    *string `json:"sha,omitempty"`
}

type Source struct {
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
	SHA    string `json:"sha,omitempty"`
}

type Environment struct {
	Name     string            `json:"name"`
	Template string            `json:"template"`
	Source   *Source           `json:"source,omitempty"`
	Values   map[string]string `json:"values,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	TTL      string            `json:"ttl,omitempty"`
	Phase    string            `json:"phase"`
	URL      string            `json:"url,omitempty"`
}

const maxEnvironmentRequestBody = 1 << 20

const maxMapEntries = 128

type EnvironmentHandler struct {
	client    client.Client
	namespace string
}

func NewEnvironmentHandlerInNamespace(cl client.Client, namespace string) http.Handler {
	return &EnvironmentHandler{client: cl, namespace: namespace}
}

func (h *EnvironmentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.URL.EscapedPath() != path {
		httpjson.NotFound(w)
		return
	}

	if path == pathEnvironments {
		switch r.Method {
		case http.MethodPost:
			if r.URL.RawQuery != "" {
				invalid(w, "query parameters are not supported")
				return
			}
			h.handlePost(w, r)
		case http.MethodGet, http.MethodHead:
			h.handleList(w, r)
		default:
			httpjson.MethodNotAllowed(w, "GET, HEAD, POST")
		}
		return
	}

	h.routeEnvironmentItem(w, r, path)
}

func (h *EnvironmentHandler) routeEnvironmentItem(w http.ResponseWriter, r *http.Request, path string) {
	name, ok := strings.CutPrefix(path, "/v1/environments/")
	if !ok || name == "" || strings.Contains(name, "/") {
		httpjson.NotFound(w)
		return
	}
	if len(validation.IsDNS1123Label(name)) > 0 {
		invalid(w, "invalid environment name")
		return
	}
	if r.URL.RawQuery != "" {
		invalid(w, "query parameters are not supported")
		return
	}

	r.SetPathValue("name", name)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.handleGet(w, r)
	case http.MethodDelete:
		h.handleDelete(w, r)
	default:
		httpjson.MethodNotAllowed(w, "DELETE, GET, HEAD")
	}
}

func (h *EnvironmentHandler) handlePost(w http.ResponseWriter, r *http.Request) {
	req, ok := parseEnvironmentRequest(w, r)
	if !ok {
		return
	}

	if state := requeststate.FromContext(r.Context()); state != nil {
		state.Name = req.Name
	}

	ee := &v1alpha1.EphemeralEnvironment{ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: h.namespace}}
	result, err := controllerutil.CreateOrUpdate(r.Context(), h.client, ee, applyEnvironmentSpec(req, ee))
	if err != nil {
		kubeError(w, err)
		return
	}

	code := http.StatusOK
	if result == controllerutil.OperationResultCreated {
		code = http.StatusCreated
	}
	if state := requeststate.FromContext(r.Context()); state != nil {
		state.Result = "updated_or_unchanged"
		if code == http.StatusCreated {
			state.Result = "created"
		}
	}

	httpjson.Write(w, code, publicEnvironment(ee))
}

func parseEnvironmentRequest(w http.ResponseWriter, r *http.Request) (EnvironmentRequest, bool) {
	limited := http.MaxBytesReader(w, r.Body, maxEnvironmentRequestBody)
	defer limited.Close()
	body, err := io.ReadAll(limited)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			httpjson.Error(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 1 MiB")
		} else {
			invalid(w, "could not read request body")
		}
		return EnvironmentRequest{}, false
	}

	var object map[string]any
	if json.Unmarshal(body, &object) != nil || object == nil || containsNull(object) {
		invalid(w, "request body must be one JSON object without null values")
		return EnvironmentRequest{}, false
	}

	var req EnvironmentRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil {
		invalid(w, "invalid JSON fields or types; unknown fields are not allowed")
		return EnvironmentRequest{}, false
	}

	err = validateEnvironmentRequest(req)
	if err != nil {
		invalid(w, err.Error())
		return EnvironmentRequest{}, false
	}
	return req, true
}

func validateEnvironmentRequest(req EnvironmentRequest) error {
	if req.Name == "" {
		return errors.New("name is required")
	}
	if len(validation.IsDNS1123Label(req.Name)) > 0 {
		return errors.New("invalid name")
	}

	if req.Template == "" {
		return errors.New("template is required")
	}
	if len(validation.IsDNS1123Subdomain(req.Template)) > 0 {
		return errors.New("invalid template name")
	}

	if err := validateTTL(req.TTL); err != nil {
		return err
	}

	if err := validateMaps(req); err != nil {
		return err
	}

	return validateSource(req.Source)
}

func validateTTL(ttl *string) error {
	if ttl == nil || *ttl == "" {
		return nil
	}
	d, ttlErr := time.ParseDuration(*ttl)
	if ttlErr != nil || d < 0 || len(*ttl) > 64 {
		return errors.New("ttl must be a non-negative duration of at most 64 bytes")
	}
	return nil
}

func validateMaps(req EnvironmentRequest) error {
	for _, m := range []map[string]string{req.Values, req.Env} {
		if len(m) > maxMapEntries {
			return errors.New("maps may contain at most 128 entries")
		}
		for k, v := range m {
			if len(k) == 0 || len(k) > 253 || len(v) > 4096 {
				return errors.New("map keys must be 1..253 bytes and values at most 4096 bytes")
			}
		}
	}
	return nil
}

func validateSource(src *sourcePatch) error {
	if src == nil {
		return nil
	}
	for _, field := range []struct {
		value *string
		max   int
	}{{src.Repo, 2048}, {src.Branch, 256}, {src.SHA, 128}} {
		if field.value != nil && len(*field.value) > field.max {
			return errors.New("source exceeds repo/branch/sha byte limits (2048/256/128)")
		}
	}
	return nil
}

func applyEnvironmentSpec(req EnvironmentRequest, ee *v1alpha1.EphemeralEnvironment) func() error {
	return func() error {
		ee.Spec.Template = req.Template
		applySourcePatch(req.Source, &ee.Spec.Source)
		if req.Values != nil {
			ee.Spec.Values = req.Values
		}
		if req.Env != nil {
			ee.Spec.Env = make(map[string]v1alpha1.EnvValue, len(req.Env))
			for k, v := range req.Env {
				ee.Spec.Env[k] = v1alpha1.EnvValue{Value: v}
			}
		}
		if req.TTL != nil {
			ee.Spec.TTL = *req.TTL
		}
		return nil
	}
}

func applySourcePatch(src *sourcePatch, spec *v1alpha1.SourceSpec) {
	if src == nil {
		return
	}
	if src.Repo != nil {
		spec.Repo = *src.Repo
	}
	if src.Branch != nil {
		spec.Branch = *src.Branch
	}
	if src.SHA != nil {
		spec.SHA = *src.SHA
	}
}

func containsNull(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case map[string]any:
		for _, item := range v {
			if containsNull(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if containsNull(item) {
				return true
			}
		}
	}
	return false
}

func publicEnvironment(ee *v1alpha1.EphemeralEnvironment) Environment {
	e := Environment{
		Name:     ee.Name,
		Template: ee.Spec.Template,
		Values:   ee.Spec.Values,
		TTL:      ee.Spec.TTL,
		Phase:    string(ee.Status.Phase),
		URL:      ee.Status.URL,
	}
	if ee.Spec.Source != (v1alpha1.SourceSpec{}) {
		e.Source = &Source{Repo: ee.Spec.Source.Repo, Branch: ee.Spec.Source.Branch, SHA: ee.Spec.Source.SHA}
	}

	for k, v := range ee.Spec.Env {
		if v.SecretKeyRef == nil {
			if e.Env == nil {
				e.Env = make(map[string]string)
			}
			e.Env[k] = v.Value
		}
	}

	return e
}

func (h *EnvironmentHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	ee := &v1alpha1.EphemeralEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: r.PathValue("name"), Namespace: h.namespace},
	}

	if err := h.client.Delete(r.Context(), ee); err != nil {
		kubeError(w, err)
		return
	}

	if state := requeststate.FromContext(r.Context()); state != nil {
		state.Result = "deletion_accepted"
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *EnvironmentHandler) handleList(w http.ResponseWriter, r *http.Request) {
	opts, err := listOptions(r, h.namespace)
	if err != nil {
		invalid(w, err.Error())
		return
	}

	var list v1alpha1.EphemeralEnvironmentList
	if listErr := h.client.List(r.Context(), &list, opts); listErr != nil {
		kubeError(w, listErr)
		return
	}

	items := make([]Environment, 0, len(list.Items))
	for i := range list.Items {
		items = append(items, publicEnvironment(&list.Items[i]))
	}

	httpjson.Write(w, http.StatusOK, page[Environment]{Items: items, Continue: list.Continue})
}

func (h *EnvironmentHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	ee := &v1alpha1.EphemeralEnvironment{}
	if err := h.client.Get(
		r.Context(),
		client.ObjectKey{Name: r.PathValue("name"), Namespace: h.namespace},
		ee,
	); err != nil {
		kubeError(w, err)
		return
	}

	httpjson.Write(w, http.StatusOK, publicEnvironment(ee))
}
