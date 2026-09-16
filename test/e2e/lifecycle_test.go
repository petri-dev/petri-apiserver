//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/petri-dev/petri-apiserver/test/utils"
	"github.com/petri-dev/petri-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

func getObject(resource, name, ns string, into any) error {
	args := []string{"get", resource, "-n", ns, "-o", "json"}
	if name != "" {
		args = append(args, name)
	}
	out, err := utils.Run(exec.Command("kubectl", args...))
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(out), into)
}

type workloadState struct {
	env        v1alpha1.EphemeralEnvironment
	deployment appsv1.Deployment
	job        batchv1.Job
}

func healthyWorkload(name, tag string, replicas int32, previous workloadState) (workloadState, error) {
	var state workloadState

	code, body, err := curlAPIServer("GET", "/v1/environments/"+name, "")
	if err != nil {
		return state, err
	}
	if code != 200 {
		return state, fmt.Errorf("GET environment: %d %s", code, body)
	}
	if err := checkPublicEnvironment(body, name, "Ready"); err != nil {
		return state, err
	}

	if err := getObject("ephemeralenvironment", name, namespace, &state.env); err != nil {
		return state, err
	}
	ee := state.env
	if ee.Status.Phase != v1alpha1.EnvironmentPhaseReady || ee.Status.ObservedGeneration != ee.Generation ||
		len(ee.Status.Components) != 1 || ee.Status.Components[0].Phase != v1alpha1.ComponentPhaseReady ||
		!slices.Contains(
			ee.Finalizers,
			"petri.run/cleanup",
		) || !strings.HasPrefix(ee.Status.TargetNamespace, "petri-env-") {
		return state, fmt.Errorf("environment not converged: %s", body)
	}

	var ns corev1.Namespace
	if err := getObject("namespace", ee.Status.TargetNamespace, namespace, &ns); err != nil {
		return state, err
	}
	if ns.Labels["petri.run/managed"] != "true" || ns.Labels["petri.run/environment-uid"] != string(ee.UID) {
		return state, fmt.Errorf("namespace ownership mismatch: %v", ns.Labels)
	}

	if err := getObject("deployment", name+"-svc", ns.Name, &state.deployment); err != nil {
		return state, err
	}
	d := state.deployment
	if d.Spec.Replicas == nil || *d.Spec.Replicas != replicas || d.Status.ObservedGeneration != d.Generation ||
		d.Status.UpdatedReplicas != replicas || d.Status.ReadyReplicas != replicas || d.Status.AvailableReplicas != replicas ||
		d.Status.Replicas != replicas || len(d.Spec.Template.Spec.Containers) != 1 ||
		d.Spec.Template.Spec.Containers[0].Image != "petri-workload:"+tag {
		return state, fmt.Errorf("workload has not rolled out: spec=%+v status=%+v", d.Spec, d.Status)
	}
	if err := getObject("job", "petri-deploy-"+name+"-svc", ns.Name, &state.job); err != nil {
		return state, err
	}
	if state.job.Status.Succeeded != 1 {
		return state, fmt.Errorf("deploy job not successful: %+v", state.job.Status)
	}

	if previous.env.UID != "" && (ee.UID != previous.env.UID || ns.Name != previous.env.Status.TargetNamespace ||
		ee.Generation <= previous.env.Generation || d.Generation <= previous.deployment.Generation || state.job.UID == previous.job.UID) {
		return state, fmt.Errorf("update did not replace deploy Job and advance generations in the original namespace")
	}
	return state, nil
}

func waitHealthy(t *testing.T, name, tag string, replicas int32, previous workloadState) workloadState {
	t.Helper()
	var state workloadState
	if err := poll(4*time.Minute, 2*time.Second, func() error {
		var err error
		state, err = healthyWorkload(name, tag, replicas, previous)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return state
}

func deleteEnvironment(t *testing.T, name, targetNS string) {
	t.Helper()
	code, body := request(t, "DELETE", "/v1/environments/"+name, "")
	if code != 204 {
		t.Fatalf("delete: %d %s", code, body)
	}

	if err := poll(3*time.Minute, 2*time.Second, func() error {
		for _, resource := range []struct{ kind, name string }{{"ephemeralenvironment", name}, {"namespace", targetNS}} {
			out, err := utils.Run(exec.Command("kubectl", "get", resource.kind, resource.name,
				"-n", namespace, "--ignore-not-found", "-o", "name"))
			if err != nil {
				return err
			}
			if strings.TrimSpace(out) != "" {
				return fmt.Errorf("cleanup incomplete: %s", out)
			}
		}
		code, body, err := curlAPIServer("GET", "/v1/environments/"+name, "")
		if err != nil {
			return err
		}
		if code != 404 {
			return fmt.Errorf("deleted API object: %d %s", code, body)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func testLifecycle(t *testing.T) {
	runCommand(
		t,
		"apply local lifecycle template",
		exec.Command("kubectl", "apply", "-f", "test/e2e/testdata/template.yaml"),
	)

	first, second, broken := "first-"+utils.RandomSuffix(), "second-"+utils.RandomSuffix(), "broken-"+utils.RandomSuffix()
	upsert := func(name, tag, replicas string, want int) {
		t.Helper()
		body := fmt.Sprintf(
			`{"name":%q,"template":"e2e-template","values":{"image.tag":%q,"replicaCount":%q}}`,
			name,
			tag,
			replicas,
		)
		code, response := request(t, "POST", "/v1/environments", body)
		if code != want {
			t.Fatalf("upsert %s: expected %d, got %d %s", name, want, code, response)
		}
		if err := checkPublicEnvironment(response, name, ""); err != nil {
			t.Fatal(err)
		}
	}

	t.Log("create two environments through API and require healthy workloads")
	upsert(first, "e2e-test", "1", 201)
	upsert(second, "e2e-test", "1", 201)

	t.Log("paginate real Kubernetes lists through the public API")
	seen := map[string]bool{}
	continuation := ""
	for range 20 {
		path := "/v1/environments?limit=1"
		if continuation != "" {
			path += "&continue=" + url.QueryEscape(continuation)
		}
		code, body := request(t, "GET", path, "")
		if code != 200 {
			t.Fatalf("pagination: %d %s", code, body)
		}
		var page struct {
			Items    []json.RawMessage `json:"items"`
			Continue string            `json:"continue"`
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 1 {
			t.Fatal("Kubernetes list exceeded limit")
		}
		for _, item := range page.Items {
			var dto struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(item, &dto); err != nil {
				t.Fatal(err)
			}
			if err := checkPublicEnvironment(string(item), dto.Name, ""); err != nil {
				t.Fatal(err)
			}
			if seen[dto.Name] {
				t.Fatal("pagination repeated an environment")
			}
			seen[dto.Name] = true
		}
		continuation = page.Continue
		if continuation == "" {
			break
		}
	}
	if continuation != "" || !seen[first] || !seen[second] {
		t.Fatalf("incomplete pagination: %v", seen)
	}

	a := waitHealthy(t, first, "e2e-test", 1, workloadState{})
	b := waitHealthy(t, second, "e2e-test", 1, workloadState{})
	if a.env.Status.TargetNamespace == b.env.Status.TargetNamespace {
		t.Fatal("two environments share a workload namespace")
	}

	assertSecondUnchanged := func() {
		t.Helper()
		current, err := healthyWorkload(second, "e2e-test", 1, workloadState{})
		if err != nil {
			t.Fatal(err)
		}
		if current.env.UID != b.env.UID || current.env.Generation != b.env.Generation ||
			current.env.Status.TargetNamespace != b.env.Status.TargetNamespace || current.job.UID != b.job.UID ||
			current.deployment.UID != b.deployment.UID || current.deployment.Generation != b.deployment.Generation {
			t.Fatal("mutation of first environment changed the second")
		}
	}

	t.Log("update image reference and replica values; require new deploy Job and completed rollout")
	upsert(first, "e2e-updated", "2", 200)
	updated := waitHealthy(t, first, "e2e-updated", 2, a)
	assertSecondUnchanged()

	t.Log("delete finalized environment; require CR and workload namespace disappearance")
	deleteEnvironment(t, first, updated.env.Status.TargetNamespace)
	assertSecondUnchanged()

	t.Log("invalid Helm replica value must fail real deploy Jobs and surface terminal API failure")
	upsert(broken, "e2e-test", "invalid", 201)
	var failed v1alpha1.EphemeralEnvironment
	if err := poll(10*time.Minute, 3*time.Second, func() error {
		code, body, err := curlAPIServer("GET", "/v1/environments/"+broken, "")
		if err != nil {
			return err
		}
		if code != 200 {
			return fmt.Errorf("get failed environment: %d %s", code, body)
		}
		if err := checkPublicEnvironment(body, broken, "Failed"); err != nil {
			return err
		}
		if err := getObject("ephemeralenvironment", broken, namespace, &failed); err != nil {
			return err
		}
		condition := meta.FindStatusCondition(failed.Status.Conditions, "Ready")
		if failed.Status.Phase != v1alpha1.EnvironmentPhaseFailed || failed.Status.ObservedGeneration != failed.Generation ||
			condition == nil || condition.Status != "False" || condition.Reason != "DeployFailed" ||
			condition.ObservedGeneration != failed.Generation || !strings.Contains(condition.Message, "replicas") ||
			len(failed.Status.Components) != 1 ||
			failed.Status.Components[0].Phase != v1alpha1.ComponentPhaseFailed ||
			failed.Status.Components[0].DeployRetries < 5 {
			return fmt.Errorf("failure not yet surfaced: %s", body)
		}
		var job batchv1.Job
		if err := getObject("job", "petri-deploy-"+broken+"-svc", failed.Status.TargetNamespace, &job); err != nil {
			return err
		}
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				return nil
			}
		}
		return fmt.Errorf("expected terminal failed deploy Job: %+v", job.Status)
	}); err != nil {
		t.Fatal(err)
	}

	assertSecondUnchanged()
	deleteEnvironment(t, broken, failed.Status.TargetNamespace)
	deleteEnvironment(t, second, b.env.Status.TargetNamespace)
}
