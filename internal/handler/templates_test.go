package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/petri-dev/petri-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestListPagination(t *testing.T) {
	t.Parallel()
	for _, templates := range []bool{false, true} {
		for _, query := range []string{"", "?limit=1", "?limit=500&continue=opaque%2B%2F%3D"} {
			called := false
			cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
				List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
					called = true
					opts := (&client.ListOptions{}).ApplyOptions(options)
					wantLimit, wantContinue := int64(100), ""
					if query == "?limit=1" {
						wantLimit = 1
					}
					if strings.Contains(query, "500") {
						wantLimit, wantContinue = 500, "opaque+/="
					}
					if opts.Namespace != "managed" || opts.Limit != wantLimit || opts.Continue != wantContinue || opts.LabelSelector != nil || opts.FieldSelector != nil {
						t.Fatalf("wrong options: %+v", opts)
					}
					switch v := list.(type) {
					case *v1alpha1.EnvironmentTemplateList:
						v.Continue = "next+/="
						v.Items = []v1alpha1.EnvironmentTemplate{{ObjectMeta: metav1.ObjectMeta{Name: "tmpl", Annotations: map[string]string{"private": "hidden"}}, Spec: v1alpha1.EnvironmentTemplateSpec{Components: []v1alpha1.ComponentSpec{{Name: "svc", Helm: &v1alpha1.HelmSpec{Repo: "hidden"}}}}}}
					case *v1alpha1.EphemeralEnvironmentList:
						v.Continue = "next+/="
						v.Items = []v1alpha1.EphemeralEnvironment{{ObjectMeta: metav1.ObjectMeta{Name: "env", Namespace: "managed"}, Spec: v1alpha1.EphemeralEnvironmentSpec{Template: "tmpl"}}}
					}
					return nil
				},
			}).Build()

			h := NewEnvironmentHandlerInNamespace(cl, "managed")
			path := "/v1/environments"
			if templates {
				h, path = NewTemplateHandlerInNamespace(cl, "managed"), "/v1/templates"
			}

			rec := serve(t, h, "GET", path+query, "", 200)
			var result page[map[string]any]
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if !called || len(result.Items) != 1 || result.Continue != "next+/=" || strings.Contains(rec.Body.String(), "hidden") || strings.Contains(rec.Body.String(), "metadata") {
				t.Fatal(rec.Body.String())
			}
			if templates && len(result.Items[0]) != 2 {
				t.Fatal("unexpected template fields")
			}

			for _, invalid := range []string{"limit=", "limit=0", "limit=501", "limit=-1", "limit=%2B1", "limit=1.0", "limit=1e2", "limit=99999999999999999999", "limit=1&limit=2", "namespace=other", "labelSelector=x", "continue=", "continue=a&continue=b", "continue=" + strings.Repeat("x", 8193), "limit=%xx", "limit=1;continue=x"} {
				called = false
				serve(t, h, "GET", path+"?"+invalid, "", 400)
				if called {
					t.Fatal("invalid query reached client")
				}
			}
		}
	}
}

func TestEmptyListsAndTemplateRouting(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	for path, h := range map[string]http.Handler{"/v1/environments": NewEnvironmentHandlerInNamespace(cl, "managed"), "/v1/templates": NewTemplateHandlerInNamespace(cl, "managed")} {
		if rec := serve(t, h, "GET", path, "", 200); rec.Body.String() != "{\"items\":[]}\n" {
			t.Fatal(rec.Body.String())
		}
	}

	h := NewTemplateHandlerInNamespace(cl, "managed")
	for _, path := range []string{"/templates", "/v1/templates/", "/v1/templates/private", "/v1/%74emplates"} {
		serve(t, h, "GET", path, "", 404)
	}
	serve(t, h, "POST", "/v1/templates", "", 405)
}
