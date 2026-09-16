package handler

import (
	"net/http"

	"github.com/petri-dev/petri-apiserver/internal/httpjson"
	v1alpha1 "github.com/petri-dev/petri-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type TemplateHandler struct {
	client    client.Client
	namespace string
}

func NewTemplateHandlerInNamespace(cl client.Client, namespace string) http.Handler {
	return &TemplateHandler{client: cl, namespace: namespace}
}

func (h *TemplateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != pathTemplates || r.URL.EscapedPath() != r.URL.Path {
		httpjson.NotFound(w)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		httpjson.MethodNotAllowed(w, "GET, HEAD")
		return
	}
	h.handleList(w, r)
}

func (h *TemplateHandler) handleList(w http.ResponseWriter, r *http.Request) {
	opts, err := listOptions(r, h.namespace)
	if err != nil {
		invalid(w, err.Error())
		return
	}

	var list v1alpha1.EnvironmentTemplateList
	if listErr := h.client.List(r.Context(), &list, opts); listErr != nil {
		kubeError(w, listErr)
		return
	}

	type componentSummary struct {
		Name string `json:"name"`
	}
	type templateSummary struct {
		Name       string             `json:"name"`
		Components []componentSummary `json:"components"`
	}

	items := make([]templateSummary, 0, len(list.Items))
	for _, item := range list.Items {
		t := templateSummary{Name: item.Name, Components: make([]componentSummary, 0, len(item.Spec.Components))}
		for _, c := range item.Spec.Components {
			t.Components = append(t.Components, componentSummary{Name: c.Name})
		}
		items = append(items, t)
	}

	httpjson.Write(w, http.StatusOK, page[templateSummary]{Items: items, Continue: list.Continue})
}
