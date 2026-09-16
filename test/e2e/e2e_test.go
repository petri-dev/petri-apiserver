//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/petri-dev/petri-apiserver/test/utils"
)

func checkPublicEnvironment(body, name, phase string) error {
	var dto map[string]any
	if err := json.Unmarshal([]byte(body), &dto); err != nil {
		return err
	}
	for field := range dto {
		if !slices.Contains([]string{"name", "template", "source", "values", "env", "ttl", "phase", "url"}, field) {
			return fmt.Errorf("private environment field %q", field)
		}
	}
	if dto["name"] != name || dto["template"] != "e2e-template" {
		return fmt.Errorf("unexpected DTO: %s", body)
	}
	if _, ok := dto["phase"].(string); !ok {
		return fmt.Errorf("missing public phase: %s", body)
	}
	if phase != "" && dto["phase"] != phase {
		return fmt.Errorf("expected phase %s: %s", phase, body)
	}
	return nil
}

const (
	token     = "e2e-test-token"
	namespace = "default"
	curlPod   = "curl-e2e"
)

func curlAPIServer(method, path, body string) (int, string, error) {
	return curlAPIServerWithToken(method, path, body, token)
}

func curlAPIServerWithToken(method, path, body, bearerToken string) (int, string, error) {
	curlArgs := []string{
		"--connect-timeout", "5", "--max-time", "15",
		"-sS", "-o", "/dev/stdout", "-w", "\n%{http_code}",
		"-X", method,
		"-H", "Content-Type: application/json",
	}
	if bearerToken != "" {
		curlArgs = append(curlArgs, "-H", "Authorization: Bearer "+bearerToken)
	}
	if body != "" {
		curlArgs = append(curlArgs, "-d", body)
	}

	svcURL := fmt.Sprintf("http://petri-apiserver.%s.svc.cluster.local:8080%s", namespace, path)
	args := append([]string{"exec", curlPod, "-n", namespace, "--", "curl"}, curlArgs...)
	out, err := utils.Run(exec.Command("kubectl", append(args, svcURL)...))
	if err != nil {
		return 0, out, err
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	code, err := strconv.Atoi(lines[len(lines)-1])
	if err != nil {
		return 0, out, fmt.Errorf("parse curl status from %q: %w", out, err)
	}
	return code, strings.Join(lines[:len(lines)-1], "\n"), nil
}

func request(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	code, responseBody, err := curlAPIServer(method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return code, responseBody
}

func poll(timeout, interval time.Duration, check func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := check(); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s: %w", timeout, err)
		}
		time.Sleep(interval)
	}
}

func waitForAPIServerPod(t *testing.T) string {
	t.Helper()
	var podName string
	err := poll(2*time.Minute, 5*time.Second, func() error {
		out, err := utils.Run(exec.Command("kubectl", "get", "pods",
			"-l", "app=petri-apiserver",
			"-o", "go-template={{ range .items }}{{ if not .metadata.deletionTimestamp }}{{ .metadata.name }}\n{{ end }}{{ end }}",
			"-n", namespace))
		if err != nil {
			return err
		}
		pods := strings.Fields(out)
		if len(pods) != 1 {
			return fmt.Errorf("expected one apiserver pod, got %d: %q", len(pods), out)
		}

		phase, err := utils.Run(exec.Command("kubectl", "get", "pod", pods[0],
			"-n", namespace, "-o", "jsonpath={.status.phase}"))
		if err != nil {
			return err
		}
		if strings.TrimSpace(phase) != "Running" {
			return fmt.Errorf("pod %s is in phase %q", pods[0], phase)
		}
		podName = pods[0]
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return podName
}

func logFailureDiagnostics(t *testing.T, podName string) {
	t.Helper()
	if out, err := utils.Run(exec.Command("kubectl", "logs", podName, "-n", namespace)); err == nil {
		t.Logf("Apiserver logs:\n%s", out)
	} else {
		t.Logf("Unable to fetch apiserver logs: %v", err)
	}
	if out, err := utils.Run(exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")); err == nil {
		t.Logf("Kubernetes events:\n%s", out)
	} else {
		t.Logf("Unable to fetch Kubernetes events: %v", err)
	}
}

func runE2ETests(t *testing.T) {
	runCommand(t, "start the in-cluster curl client", exec.Command("kubectl", "run", curlPod,
		"-n", namespace, "--image=curlimages/curl:8.12.1", "--image-pull-policy=Never", "--restart=Never", "--command", "--", "sleep", "3600"))
	t.Cleanup(func() {
		_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", curlPod, "-n", namespace, "--ignore-not-found"))
	})
	runCommand(t, "wait for the curl client", exec.Command("kubectl", "wait", "--for=condition=Ready", "pod/"+curlPod,
		"-n", namespace, "--timeout=120s"))

	t.Run("Health probes", func(t *testing.T) {
		t.Run("healthz returns 200", func(t *testing.T) {
			if err := poll(30*time.Second, 2*time.Second, func() error {
				code, _, err := curlAPIServerWithToken("GET", "/healthz", "", "")
				if err != nil {
					return err
				}
				if code != 200 {
					return fmt.Errorf("got status %d", code)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})

		t.Run("readyz returns 200", func(t *testing.T) {
			if err := poll(30*time.Second, 2*time.Second, func() error {
				code, _, err := curlAPIServerWithToken("GET", "/readyz", "", "")
				if err != nil {
					return err
				}
				if code != 200 {
					return fmt.Errorf("got status %d", code)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	})

	t.Run("Authentication", func(t *testing.T) {
		t.Run("missing token returns 401", func(t *testing.T) {
			code, _, err := curlAPIServerWithToken("GET", "/v1/environments", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if code != 401 {
				t.Fatalf("expected status 401, got %d", code)
			}
		})

		t.Run("wrong token returns 401", func(t *testing.T) {
			code, _, err := curlAPIServerWithToken("GET", "/v1/environments", "", "wrong-token")
			if err != nil {
				t.Fatal(err)
			}
			if code != 401 {
				t.Fatalf("expected status 401, got %d", code)
			}
		})
	})

	t.Run("Versioned contract", func(t *testing.T) {
		for _, tt := range []struct {
			method, path, bearer, code string
			status                     int
		}{
			{"GET", "/environments", token, "not_found", 404},
			{"GET", "/templates", token, "not_found", 404},
			{"GET", "/v1/unknown", token, "not_found", 404},
			{"GET", "/v1/environments/", token, "not_found", 404},
			{"GET", "/v1/environments%2fprivate", token, "not_found", 404},
			{"GET", "/v1/environments/", "", "unauthenticated", 401},
			{"GET", "/v1/%65nvironments", "", "unauthenticated", 401},
			{"PUT", "/v1/environments", token, "method_not_allowed", 405},
			{"POST", "/v1/templates", token, "method_not_allowed", 405},
			{"GET", "/v1/environments?namespace=other", token, "invalid_request", 400},
			{"GET", "/v1/templates?limit=501", token, "invalid_request", 400},
			{"GET", "/v1/environments?continue=not-a-token", token, "invalid_request", 400},
		} {
			code, body, err := curlAPIServerWithToken(tt.method, tt.path, "", tt.bearer)
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				Error struct{ Code, Message string }
			}
			if err := json.Unmarshal([]byte(body), &response); err != nil {
				t.Fatal(err)
			}
			if code != tt.status || response.Error.Code != tt.code || response.Error.Message == "" {
				t.Fatalf("%s %s: %d %s", tt.method, tt.path, code, body)
			}
		}
	})

	t.Run("Environment CRUD", func(t *testing.T) {
		envName := fmt.Sprintf("e2e-%s", utils.RandomSuffix())
		t.Cleanup(func() {
			_, _ = utils.Run(exec.Command("kubectl", "delete", "ephemeralenvironment", envName,
				"-n", namespace, "--ignore-not-found", "--wait=false"))
			_, _ = utils.Run(exec.Command("kubectl", "delete", "environmenttemplate", "e2e-template",
				"-n", namespace, "--ignore-not-found"))
		})

		runCommand(t, "apply the EnvironmentTemplate fixture",
			exec.Command("kubectl", "apply", "-f", "test/e2e/testdata/template.yaml"))

		if !t.Run("list templates", func(t *testing.T) {
			if err := poll(30*time.Second, 2*time.Second, func() error {
				code, body, err := curlAPIServer("GET", "/v1/templates", "")
				if err != nil {
					return err
				}
				if code != 200 {
					return fmt.Errorf("expected status 200, got %d", code)
				}

				var response struct {
					Items []map[string]any `json:"items"`
				}
				if err := json.Unmarshal([]byte(body), &response); err != nil {
					return err
				}
				for _, item := range response.Items {
					if len(item) != 2 || item["components"] == nil {
						return fmt.Errorf("unexpected template DTO: %s", body)
					}
					if item["name"] == "e2e-template" {
						return nil
					}
				}
				return fmt.Errorf("e2e-template not found in %s", body)
			}); err != nil {
				t.Fatal(err)
			}
		}) {
			t.FailNow()
		}

		if !t.Run("create environment", func(t *testing.T) {
			body := fmt.Sprintf(`{
				"name": %q,
				"template": "e2e-template",
				"source": {
					"repo": "https://github.com/petri-dev/petri-apiserver",
					"branch": "main",
					"sha": "abc1234"
				},
				"values": {"image.tag": "e2e-test"},
				"env": {"E2E_VAR": "true"}
			}`, envName)

			code, responseBody := request(t, "POST", "/v1/environments", body)
			if code != 201 {
				t.Fatalf("expected status 201, got %d: %s", code, responseBody)
			}

			if err := checkPublicEnvironment(responseBody, envName, ""); err != nil {
				t.Fatal(err)
			}

			out, err := utils.Run(exec.Command("kubectl", "get", "ephemeralenvironment", envName,
				"-n", namespace, "-o", "jsonpath={.spec.template}"))
			if err != nil {
				t.Fatal(err)
			}
			if out != "e2e-template" {
				t.Fatalf("expected template e2e-template, got %q", out)
			}

			out, err = utils.Run(exec.Command("kubectl", "get", "ephemeralenvironment", envName,
				"-n", namespace, "-o", "jsonpath={.spec.namespace}"))
			if err != nil {
				t.Fatal(err)
			}
			if out != "" {
				t.Fatalf("expected legacy spec.namespace to be empty, got %q", out)
			}
		}) {
			t.FailNow()
		}

		if !t.Run("update environment", func(t *testing.T) {
			body := fmt.Sprintf(`{
				"name": %q,
				"template": "e2e-template",
				"values": {"image.tag": "e2e-updated"}
			}`, envName)

			code, responseBody := request(t, "POST", "/v1/environments", body)
			if code != 200 {
				t.Fatalf("expected status 200, got %d: %s", code, responseBody)
			}
			if err := checkPublicEnvironment(responseBody, envName, ""); err != nil {
				t.Fatal(err)
			}

			out, err := utils.Run(exec.Command("kubectl", "get", "ephemeralenvironment", envName,
				"-n", namespace, "-o", `jsonpath={.spec.values.image\.tag}`))
			if err != nil {
				t.Fatal(err)
			}
			if out != "e2e-updated" {
				t.Fatalf("expected updated image tag, got %q", out)
			}
		}) {
			t.FailNow()
		}

		if !t.Run("get environment", func(t *testing.T) {
			code, body := request(t, "GET", "/v1/environments/"+envName, "")
			if code != 200 {
				t.Fatalf("expected status 200, got %d: %s", code, body)
			}

			if err := checkPublicEnvironment(body, envName, ""); err != nil {
				t.Fatal(err)
			}
		}) {
			t.FailNow()
		}

		if !t.Run("list environments", func(t *testing.T) {
			code, body := request(t, "GET", "/v1/environments", "")
			if code != 200 {
				t.Fatalf("expected status 200, got %d: %s", code, body)
			}

			var response struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal([]byte(body), &response); err != nil {
				t.Fatal(err)
			}
			for _, item := range response.Items {
				if item["name"] == envName {
					return
				}
			}
			t.Fatalf("environment %q not found in %s", envName, body)
		}) {
			t.FailNow()
		}

		if !t.Run("delete environment", func(t *testing.T) {
			code, body := request(t, "DELETE", "/v1/environments/"+envName, "")
			if code != 204 {
				t.Fatalf("expected status 204, got %d: %s", code, body)
			}

			if err := poll(3*time.Minute, 2*time.Second, func() error {
				out, err := utils.Run(exec.Command("kubectl", "get", "ephemeralenvironment", envName,
					"-n", namespace, "--ignore-not-found", "-o", "name"))
				if err != nil {
					return err
				}
				if strings.TrimSpace(out) != "" {
					return fmt.Errorf("environment still exists: %s", out)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}) {
			t.FailNow()
		}

		t.Run("deleted environment returns 404", func(t *testing.T) {
			code, body := request(t, "GET", "/v1/environments/"+envName, "")
			if code != 404 {
				t.Fatalf("expected status 404, got %d: %s", code, body)
			}
		})
	})

	t.Run("Validation", func(t *testing.T) {
		tests := []struct {
			name    string
			body    string
			message string
		}{
			{"missing name", `{"template": "e2e-template"}`, "name"},
			{"invalid DNS name", `{"name": "INVALID_NAME!!!", "template": "e2e-template"}`, "invalid name"},
			{"missing template", `{"name": "valid-name"}`, "template"},
			{"caller-controlled namespace", `{"name": "valid-name", "template": "e2e-template", "namespace": "other"}`, "unknown field"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				code, body := request(t, "POST", "/v1/environments", test.body)
				if code != 400 {
					t.Fatalf("expected status 400, got %d: %s", code, body)
				}
				var response struct {
					Error struct{ Code, Message string }
				}
				if err := json.Unmarshal([]byte(body), &response); err != nil {
					t.Fatal(err)
				}
				if response.Error.Code != "invalid_request" || !strings.Contains(response.Error.Message, test.message) {
					t.Fatalf("expected invalid_request containing %q, got %s", test.message, body)
				}
			})
		}
	})

	t.Run("RBAC negative checks", func(t *testing.T) {
		tests := []struct {
			name     string
			verb     string
			resource string
			want     string
		}{
			{"cannot create namespaces", "create", "namespaces", "no"},
			{"can get management namespace", "get", "namespaces/default", "yes"},
			{"cannot get other namespace", "get", "namespaces/kube-system", "no"},
			{"cannot list namespaces", "list", "namespaces", "no"},
			{"cannot get secrets", "get", "secrets", "no"},
			{"cannot list secrets", "list", "secrets", "no"},
			{"can create ephemeralenvironments", "create", "ephemeralenvironments.core.petri.run", "yes"},
			{"can list environmenttemplates", "list", "environmenttemplates.core.petri.run", "yes"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				out, err := utils.Run(exec.Command("kubectl", "auth", "can-i", test.verb, test.resource,
					"--as=system:serviceaccount:default:petri-apiserver"))
				if err != nil && test.want == "yes" {
					t.Fatal(err)
				}
				lines := strings.Fields(out)
				if len(lines) == 0 || lines[len(lines)-1] != test.want {
					t.Fatalf("expected %q, got %q", test.want, out)
				}
			})
		}
	})

	t.Run("Bounded operational load", testOperationalLoad)
	t.Run("Deployment rollback", testDeploymentRollback)
	t.Run("Operator lifecycle", testLifecycle)
	t.Run("Operations dependencies and metrics", testOperations)
}

func testDeploymentRollback(t *testing.T) {
	// Verify availability and recovery during a failed two-replica rollout.
	runCommand(t, "enable two-replica safe rollout", exec.Command("kubectl", "patch", "deployment", "petri-apiserver", "--type=strategic", "-p",
		`{"spec":{"replicas":2,"minReadySeconds":10,"strategy":{"type":"RollingUpdate","rollingUpdate":{"maxUnavailable":0,"maxSurge":1}},"template":{"spec":{"terminationGracePeriodSeconds":30,"containers":[{"name":"apiserver","lifecycle":{"preStop":{"sleep":{"seconds":5}}}}]}}}}`))
	runCommand(t, "wait for both replicas", exec.Command("kubectl", "rollout", "status", "deployment/petri-apiserver", "--timeout=120s"))
	revision, err := utils.Run(exec.Command("kubectl", "get", "deployment", "petri-apiserver", "-o", `jsonpath={.metadata.annotations.deployment\.kubernetes\.io/revision}`))
	if err != nil {
		t.Fatal(err)
	}
	image, err := utils.Run(exec.Command("kubectl", "get", "deployment", "petri-apiserver", "-o", "jsonpath={.spec.template.spec.containers[0].image}"))
	if err != nil {
		t.Fatal(err)
	}

	rolledBack := false
	t.Cleanup(func() {
		if !rolledBack {
			runCommand(t, "restore known pod template", exec.Command("kubectl", "rollout", "undo", "deployment/petri-apiserver", "--to-revision="+strings.TrimSpace(revision)))
		}
		runCommand(t, "restore single-replica dev fixture", exec.Command("kubectl", "scale", "deployment/petri-apiserver", "--replicas=1"))
		runCommand(t, "wait for restored dev fixture", exec.Command("kubectl", "rollout", "status", "deployment/petri-apiserver", "--timeout=120s"))
		waitForAPIServerPod(t)
	})

	runCommand(t, "introduce invalid authentication configuration", exec.Command("kubectl", "set", "env", "deployment/petri-apiserver", "PETRI_APISERVER_AUTH_MODE=invalid-rollout-test"))
	if _, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/petri-apiserver", "--timeout=20s")); err == nil {
		t.Fatal("invalid rollout unexpectedly became ready")
	}

	for range 3 {
		code, _ := request(t, "GET", "/v1/templates", "")
		if code != 200 {
			t.Fatalf("old replicas did not maintain Service: %d", code)
		}
	}

	runCommand(t, "rollback to known image and configuration revision", exec.Command("kubectl", "rollout", "undo", "deployment/petri-apiserver", "--to-revision="+strings.TrimSpace(revision)))
	runCommand(t, "wait for rollback", exec.Command("kubectl", "rollout", "status", "deployment/petri-apiserver", "--timeout=120s"))
	rolledBack = true

	restored, err := utils.Run(exec.Command("kubectl", "get", "deployment", "petri-apiserver", "-o", "jsonpath={.spec.template.spec.containers[0].image}"))
	if err != nil || restored != image {
		t.Fatal("rollback did not restore known image reference")
	}

	code, _ := request(t, "GET", "/v1/templates", "")
	if code != 200 {
		t.Fatalf("Service unavailable after rollback: %d", code)
	}
}

// Run last: removing a CRD is safe only after all lifecycle cleanup assertions.
func testOperations(t *testing.T) {
	ip, err := utils.Run(exec.Command("kubectl", "get", "pod", "-l", "app=petri-apiserver", "-n", namespace, "-o", "jsonpath={.items[0].status.podIP}"))
	if err != nil {
		t.Fatal(err)
	}
	probe := func(port, path, want string) string {
		t.Helper()
		out, err := utils.Run(exec.Command("kubectl", "exec", curlPod, "-n", namespace, "--", "curl", "--connect-timeout", "3", "--max-time", "5", "-sS", "-w", "\n%{http_code}", "http://"+strings.TrimSpace(ip)+":"+port+path))
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if err != nil || lines[len(lines)-1] != want {
			t.Fatalf("probe %s: %s %v", path, out, err)
		}
		return out
	}
	probe("8080", "/readyz", "200")
	metrics := probe("9090", "/metrics", "200")

	for _, expected := range []string{"petri_http_requests_total", "petri_http_request_duration_seconds_bucket", `le="0.5"`, `route="/v1/environments/{name}"`} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("metrics missing %s", expected)
		}
	}

	probe("8080", "/metrics", "401")
	probe("9090", "/v1/environments", "404")

	runCommand(t, "remove API list permission", exec.Command("kubectl", "patch", "clusterrole", "petri-apiserver-role", "--type=json", "-p", `[{"op":"remove","path":"/rules/0"}]`))
	t.Cleanup(func() {
		runCommand(t, "restore API permissions", exec.Command("kubectl", "apply", "-f", "config/rbac/role.yaml"))
	})
	probe("8080", "/readyz", "503")
	probe("8080", "/healthz", "200")

	runCommand(t, "restore API list permission", exec.Command("kubectl", "apply", "-f", "config/rbac/role.yaml"))
	probe("8080", "/readyz", "200")

	runCommand(t, "remove required CRD after lifecycle cleanup", exec.Command("kubectl", "delete", "crd", "environmenttemplates.core.petri.run", "--timeout=60s"))
	probe("8080", "/readyz", "503")
	probe("8080", "/healthz", "200")

	logs, err := utils.Run(exec.Command("kubectl", "logs", "deployment/petri-apiserver", "-n", namespace))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, `"msg":"audit"`) || !strings.Contains(logs, `"result":"deletion_accepted"`) || strings.Contains(logs, token) {
		t.Fatal("missing audit events or token leaked")
	}
}
