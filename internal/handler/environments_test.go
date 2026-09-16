package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/petri-dev/petri-apiserver/internal/httpjson"
	"github.com/petri-dev/petri-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func serve(t *testing.T, h http.Handler, method, path, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != status {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, rec.Code, status, rec.Body.String())
	}
	if status != http.StatusNoContent && rec.Header().Get("Content-Type") != "application/json" {
		t.Fatal("missing JSON content type")
	}
	if status >= 400 {
		var envelope struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal(
			rec.Body.Bytes(),
			&envelope,
		); err != nil || envelope.Error.Code == "" ||
			envelope.Error.Message == "" {
			t.Fatalf("invalid error: %s", rec.Body.String())
		}
	}
	return rec
}

func TestUpsertAndDTO(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	h := NewEnvironmentHandlerInNamespace(cl, "managed")
	path := "/v1/environments"

	serve(
		t,
		h,
		"POST",
		path,
		`{"name":"pr-42","template":"tmpl","source":{"repo":"repo","branch":"main","sha":"old"},"values":{"image.tag":"old"},"env":{"FEATURE":"true"},"ttl":"24h"}`,
		201,
	)
	serve(t, h, "POST", path, `{"name":"pr-42","template":"new","source":{"sha":"new"}}`, 200)

	var ee v1alpha1.EphemeralEnvironment
	key := client.ObjectKey{Name: "pr-42", Namespace: "managed"}
	if err := cl.Get(t.Context(), key, &ee); err != nil {
		t.Fatal(err)
	}
	if ee.Spec.Template != "new" || ee.Spec.Source.Repo != "repo" || ee.Spec.Source.Branch != "main" ||
		ee.Spec.Source.SHA != "new" ||
		ee.Spec.Env["FEATURE"].Value != "true" ||
		ee.Spec.TTL != "24h" ||
		ee.Spec.Values["image.tag"] != "old" {
		t.Fatalf("lost omitted fields: %+v", ee.Spec)
	}

	serve(t, h, "POST", path, `{"name":"pr-42","template":"new","values":{},"env":{}}`, 200)
	if err := cl.Get(t.Context(), key, &ee); err != nil {
		t.Fatal(err)
	}
	if len(ee.Spec.Values) != 0 || len(ee.Spec.Env) != 0 || ee.Spec.TTL != "24h" {
		t.Fatalf("maps not cleared: %+v", ee.Spec)
	}

	ee.Annotations = map[string]string{"private": "hidden-metadata"}
	ee.Spec.Env = map[string]v1alpha1.EnvValue{
		"PLAIN": {Value: "visible"},
		"SECRET": {
			Value:        "hidden-value",
			SecretKeyRef: &v1alpha1.EnvSecretRef{Component: "hidden-component", Key: "hidden-key"},
		},
	}
	if err := cl.Update(t.Context(), &ee); err != nil {
		t.Fatal(err)
	}

	for _, method := range []string{"GET", "POST"} {
		p, body := path+"/pr-42", ""
		if method == "POST" {
			p, body = path, `{"name":"pr-42","template":"new"}`
		}
		rec := serve(t, h, method, p, body, 200)

		var dto map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
			t.Fatal(err)
		}
		for field := range dto {
			if !slices.Contains([]string{"name", "template", "source", "values", "env", "ttl", "phase", "url"}, field) {
				t.Fatalf("private field %s", field)
			}
		}
		if strings.Contains(rec.Body.String(), "hidden") || strings.Contains(rec.Body.String(), "SECRET") ||
			!strings.Contains(rec.Body.String(), "visible") {
			t.Fatalf("bad env projection: %s", rec.Body.String())
		}
	}

	serve(t, h, "GET", path+"?namespace=other", "", 400)
	serve(t, h, "DELETE", path+"/pr-42", "", 204)
	serve(t, h, "DELETE", path+"/pr-42", "", 404)
	serve(t, h, "GET", path+"/pr-42", "", 404)
}

func TestValidationAndRouting(t *testing.T) {
	t.Parallel()
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Error("invalid request reached Kubernetes")
			return errors.New("unexpected")
		},
	}).Build()
	h := NewEnvironmentHandlerInNamespace(cl, "managed")

	valid := `{"name":"pr-42","template":"tmpl"}`
	for _, body := range []string{
		`null`, `[]`, `1`, `"text"`, `{}`, valid + `{}`, valid + `x`,
		`{"name":"BAD","template":"tmpl"}`, `{"name":"ok","template":"BAD"}`, `{"name":"ok"}`,
		`{"name":"ok","template":"tmpl","namespace":"other"}`, `{"name":"ok","template":"tmpl","source":{"namespace":"other"}}`,
		`{"name":"ok","template":"tmpl","env":{"X":null}}`, `{"name":"ok","template":"tmpl","values":null}`,
		`{"name":"ok","template":"tmpl","ttl":null}`, `{"name":"ok","template":"tmpl","source":null}`,
		`{"name":"ok","template":"tmpl","values":{"x":1}}`, `{"name":"ok","template":"tmpl","env":{"x":{"secretKeyRef":{}}}}`,
		`{"name":"ok","template":"tmpl","ttl":"-1s"}`, `{"name":"ok","template":"tmpl","ttl":"NaN"}`,
		`{"name":"ok","template":"tmpl","ttl":"Inf"}`, `{"name":"ok","template":"tmpl","ttl":1}`,
		`{"name":"ok","template":"tmpl","ttl":"9999999999999999999999h"}`,
		`{"name":"ok","template":"tmpl","values":{"":"x"}}`,
	} {
		serve(t, h, "POST", "/v1/environments", body, 400)
	}

	for _, body := range []string{valid + strings.Repeat(" ", maxEnvironmentRequestBody), `{"` + strings.Repeat("x", maxEnvironmentRequestBody)} {
		rec := serve(t, h, "POST", "/v1/environments", body, 413)
		if !strings.Contains(rec.Body.String(), `"code":"request_too_large"`) {
			t.Fatal(rec.Body.String())
		}
	}

	for _, path := range []string{"/environments", "/v1/environments/", "/v1/environments/a/b", "/v1/environments/a/", "/v1/environments/%61", "/v1/environments/a%2fb", "/v1/environments//a"} {
		serve(t, h, "GET", path, "", 404)
	}
	serve(t, h, "GET", "/v1/environments/INVALID", "", 400)
	serve(t, h, "PUT", "/v1/environments", "", 405)
	serve(t, h, "PATCH", "/v1/environments/ok", "", 405)
	serve(t, h, "POST", "/v1/environments?namespace=other", valid, 400)
}

func TestFieldBounds(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"values", "env", "repo", "branch", "sha", "key", "entries"} {
		for _, extra := range []int{0, 1} {
			body := map[string]any{"name": "ok", "template": "tmpl"}
			switch field {
			case "values", "env":
				body[field] = map[string]string{"key": strings.Repeat("x", 4096+extra)}
			case "key":
				body["values"] = map[string]string{strings.Repeat("x", 253+extra): "v"}
			case "entries":
				m := map[string]string{}
				for i := range 128 + extra {
					m[strings.Repeat("x", i+1)] = "v"
				}
				body["env"] = m
			default:
				limit := map[string]int{"repo": 2048, "branch": 256, "sha": 128}[field]
				body["source"] = map[string]string{field: strings.Repeat("x", limit+extra)}
			}

			data, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}

			cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
			status := 201
			if extra == 1 {
				status = 400
			}
			serve(t, NewEnvironmentHandlerInNamespace(cl, "managed"), "POST", "/v1/environments", string(data), status)
		}
	}

	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	h := NewEnvironmentHandlerInNamespace(cl, "managed")
	valid := `{"name":"ok","template":"tmpl","ttl":"0s"}`
	serve(t, h, "POST", "/v1/environments", valid+strings.Repeat(" ", maxEnvironmentRequestBody-len(valid)), 201)
	serve(t, h, "POST", "/v1/environments", `{"name":"ok","template":"tmpl","ttl":""}`, 200)
}

func TestConcurrentUpserts(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		builder := fake.NewClientBuilder().WithScheme(newScheme(t))
		if existing {
			builder.WithObjects(
				&v1alpha1.EphemeralEnvironment{
					ObjectMeta: metav1.ObjectMeta{Name: "ok", Namespace: "managed"},
					Spec:       v1alpha1.EphemeralEnvironmentSpec{Template: "old"},
				},
			)
		}

		arrived, release := make(chan struct{}, 2), make(chan struct{})
		cl := builder.WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := cl.Get(ctx, key, obj, opts...)
			arrived <- struct{}{}
			<-release
			return err
		}}).
			Build()
		h := NewEnvironmentHandlerInNamespace(cl, "managed")

		results := make(chan int, 2)
		for _, template := range []string{"first", "second"} {
			go func() {
				r := httptest.NewRequestWithContext(
					t.Context(),
					"POST",
					"/v1/environments",
					strings.NewReader(`{"name":"ok","template":"`+template+`"}`),
				)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				results <- w.Code
			}()
		}

		<-arrived
		<-arrived
		close(release)
		a, b := <-results, <-results

		want := 201
		if existing {
			want = 200
		}
		if (a != want || b != 409) && (b != want || a != 409) {
			t.Fatalf("concurrent existing=%t: %d %d", existing, a, b)
		}
	}
}

func TestKubernetesErrorRedaction(t *testing.T) {
	t.Parallel()
	resource := schema.GroupResource{Group: "private.group", Resource: "private-resource"}
	for _, tt := range []struct {
		err    error
		status int
		code   string
	}{
		{apierrors.NewNotFound(resource, "private-name"), 404, "not_found"},
		{apierrors.NewConflict(resource, "private-name", errors.New("private-detail")), 409, "conflict"},
		{apierrors.NewBadRequest("private-detail"), 400, "invalid_request"},
		{apierrors.NewInvalid(schema.GroupKind{Group: "private", Kind: "private"}, "private", nil), 400, "invalid_request"},
		{apierrors.NewResourceExpired("private-token"), 400, "invalid_request"},
		{apierrors.NewForbidden(resource, "private-name", errors.New("private-detail")), 500, "internal_error"},
		{apierrors.NewServiceUnavailable("private-detail"), 500, "internal_error"},
		{errors.New("private-detail"), 500, "internal_error"},
	} {
		cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return tt.err
			},
			List:   func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return tt.err },
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error { return tt.err },
		}).Build()
		h := NewEnvironmentHandlerInNamespace(cl, "managed")

		for _, request := range []struct{ method, path, body string }{{"GET", "/v1/environments/ok", ""}, {"DELETE", "/v1/environments/ok", ""}, {"GET", "/v1/environments", ""}} {
			rec := serve(t, h, request.method, request.path, request.body, tt.status)
			if strings.Contains(rec.Body.String(), "private") ||
				!strings.Contains(rec.Body.String(), `"code":"`+tt.code+`"`) {
				t.Fatal(rec.Body.String())
			}
		}
	}
}

func TestNamespaceAndResponseBounds(t *testing.T) {
	t.Parallel()
	private := &v1alpha1.EphemeralEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "other"},
		Spec:       v1alpha1.EphemeralEnvironmentSpec{Template: "tmpl"},
	}
	large := &v1alpha1.EphemeralEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "large", Namespace: "managed"},
		Spec: v1alpha1.EphemeralEnvironmentSpec{
			Template: "tmpl",
			Values:   map[string]string{"large": strings.Repeat("x", httpjson.MaxResponseBytes)},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(private, large).Build()
	h := NewEnvironmentHandlerInNamespace(cl, "managed")

	serve(t, h, "GET", "/v1/environments/private", "", 404)
	serve(t, h, "DELETE", "/v1/environments/private", "", 404)
	for _, path := range []string{"/v1/environments/large", "/v1/environments"} {
		rec := serve(t, h, "GET", path, "", 500)
		if rec.Body.Len() > 256 || !strings.Contains(rec.Body.String(), `"code":"internal_error"`) {
			t.Fatal("partial oversized response")
		}
	}

	if err := cl.Delete(t.Context(), large); err != nil {
		t.Fatal(err)
	}
	if rec := serve(t, h, "GET", "/v1/environments", "", 200); rec.Body.String() != "{\"items\":[]}\n" {
		t.Fatalf("namespace leaked: %s", rec.Body.String())
	}
}
