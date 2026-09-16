//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/petri-dev/petri-apiserver/test/utils"
	corev1 "k8s.io/api/core/v1"
)

func TestE2E(t *testing.T) {
	projectDir, err := utils.GetProjectDir()
	if err != nil {
		t.Fatal(err)
	}
	version, err := utils.Run(
		exec.Command("go", "list", "-m", "-f", "{{.Version}}", "github.com/petri-dev/petri-operator"),
	)
	if err != nil {
		t.Fatal(err)
	}
	tag := strings.TrimSpace(version)
	cluster := os.Getenv("KIND_CLUSTER")
	if cluster == "" {
		cluster = "petri-api-e2e-" + utils.RandomSuffix()
	}
	t.Setenv("KIND_CLUSTER", cluster)
	kind := os.Getenv("KIND")
	if kind == "" {
		kind = "kind"
	}
	t.Setenv("KIND", kind)
	clusters, err := utils.Run(exec.Command(kind, "get", "clusters"))
	if err != nil {
		t.Fatal(err)
	}

	for _, existing := range strings.Fields(clusters) {
		if existing == cluster {
			t.Fatalf("refusing to use or delete existing cluster %q", cluster)
		}
	}
	// note: never read or merge the user's kubeconfig, even when running go test directly.
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	t.Setenv("KUBECONFIG", kubeconfig)
	t.Setenv("KUBECTL_KUBERC", "false")
	t.Cleanup(func() {
		if _, err := utils.Run(exec.Command(kind, "delete", "cluster", "--name", cluster)); err != nil {
			t.Errorf("delete disposable cluster: %v", err)
		}
	})
	t.Logf("Disposable cluster: %s; operator: %s", cluster, tag)
	runCommand(t, "create disposable Kind cluster", exec.Command(kind, "create", "cluster", "--name", cluster,
		"--image", "kindest/node:v1.36.1", "--kubeconfig", kubeconfig, "--wait", "120s"))
	current, err := utils.Run(exec.Command("kubectl", "config", "current-context"))
	if err != nil || strings.TrimSpace(current) != "kind-"+cluster {
		t.Fatalf("unexpected disposable context %q: %v", current, err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			logFailureDiagnostics(t, "deployment/petri-apiserver")
			for _, args := range [][]string{
				{"logs", "-n", "petri-system", "deployment/petri-controller-manager", "--all-containers", "--tail=200"},
				{"get", "ephemeralenvironments", "-A", "-o", "yaml"},
				{"get", "pods,jobs,deployments", "-A", "-o", "wide"},
				{"get", "events", "-A", "--sort-by=.lastTimestamp"},
			} {
				out, err := utils.Run(exec.Command("kubectl", args...))
				t.Logf("%v:\n%s\n%v", args, out, err)
			}
			var namespaces corev1.NamespaceList
			if err := getObject("namespaces", "", namespace, &namespaces); err == nil {
				for _, ns := range namespaces.Items {
					if ns.Labels["petri.run/managed"] == "true" {
						out, err := utils.Run(
							exec.Command(
								"kubectl",
								"logs",
								"-n",
								ns.Name,
								"-l",
								"petri.run/managed=true",
								"--all-containers",
								"--tail=100",
							),
						)
						t.Logf("Deployer logs in %s:\n%s\n%v", ns.Name, out, err)
					}
				}
			}
		}
	})

	apiserverImage := "petri-apiserver:" + cluster
	operatorImage := "ghcr.io/petri-dev/petri-operator:" + tag
	deployerImage := "ghcr.io/petri-dev/petri-deployer:" + tag
	fixtureImage := "petri-deployer-fixture:" + cluster

	runCommand(t, "build the apiserver image",
		exec.Command("docker", "build", "-t", apiserverImage, "."))
	runCommand(t, "pull operator image", exec.Command("docker", "pull", operatorImage))
	runCommand(t, "pull deployer image", exec.Command("docker", "pull", deployerImage))
	runCommand(t, "add local chart to deployer", exec.Command("docker", "build", "-t", fixtureImage,
		"--build-arg", "DEPLOYER_IMAGE="+deployerImage,
		"-f", filepath.Join(projectDir, "test", "e2e", "testdata", "Dockerfile.deployer"),
		filepath.Join(projectDir, "test", "e2e", "testdata", "chart")))
	runCommand(t, "pull pause workload", exec.Command("docker", "pull", "registry.k8s.io/pause:3.9"))
	for _, imageTag := range []string{"e2e-test", "e2e-updated"} {
		runCommand(
			t,
			"tag local workload",
			exec.Command("docker", "tag", "registry.k8s.io/pause:3.9", "petri-workload:"+imageTag),
		)
	}
	runCommand(t, "pull pinned API client", exec.Command("docker", "pull", "curlimages/curl:8.12.1"))

	for _, image := range []string{apiserverImage, operatorImage, fixtureImage, "petri-workload:e2e-test", "petri-workload:e2e-updated", "curlimages/curl:8.12.1"} {
		if err := utils.LoadImageToKindClusterWithName(t.Context(), image); err != nil {
			t.Fatalf("load %s: %v", image, err)
		}
	}
	runCommand(t, "install operator and release CRDs", exec.Command("helm", "upgrade", "--install", "petri",
		"oci://ghcr.io/petri-dev/charts/petri", "--version", strings.TrimPrefix(tag, "v"),
		"--namespace", "petri-system", "--create-namespace",
		"--kube-context", "kind-"+cluster,
		"--set", "operator.image.reference="+operatorImage, "--set", "operator.image.pullPolicy=Never",
		"--set", "deployer.image.reference="+fixtureImage, "--wait", "--timeout", "120s"))

	runCommand(t, "wait for CRDs to be established", exec.Command("kubectl", "wait", "--for=condition=Established",
		"crd/ephemeralenvironments.core.petri.run",
		"crd/environmenttemplates.core.petri.run",
		"--timeout=60s"))

	runCommand(t, "deploy the apiserver",
		exec.Command("kubectl", "apply", "-k", "test/e2e/testdata/apiserver"))

	patchJSON := fmt.Sprintf(
		`{"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"apiserver","image":%q,"imagePullPolicy":"Never"}]}}}}`,
		apiserverImage,
	)
	runCommand(t, "use the test apiserver image", exec.Command("kubectl", "patch", "deployment", "petri-apiserver",
		"--type=strategic", "-p", patchJSON))
	runCommand(t, "wait for the apiserver deployment", exec.Command("kubectl", "rollout", "status",
		"deployment/petri-apiserver", "--timeout=120s"))

	waitForAPIServerPod(t)

	runE2ETests(t)
}

func runCommand(t *testing.T, action string, cmd *exec.Cmd) {
	t.Helper()
	t.Log(action)
	if _, err := utils.Run(cmd); err != nil {
		t.Fatalf("%s: %v", action, err)
	}
}
