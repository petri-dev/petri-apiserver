//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/petri-dev/petri-apiserver/test/load"
)

func testOperationalLoad(t *testing.T) {
	if os.Getenv("PETRI_KIND_SOAK") != "true" {
		t.Skip("set PETRI_KIND_SOAK=true for disposable load and pod restart")
	}

	runCommand(
		t,
		"apply disposable template for read load",
		exec.Command("kubectl", "apply", "-f", "test/e2e/testdata/template.yaml"),
	)
	for _, phase := range []string{"baseline", "after restart"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "kubectl", "--request-timeout=10s", "-n", namespace,
				"port-forward", "--address=127.0.0.1", "service/petri-apiserver", ":8080")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal("port-forward pipe unavailable")
			}
			if err := cmd.Start(); err != nil {
				t.Fatal("port-forward failed to start")
			}
			defer func() { cancel(); _ = cmd.Wait() }()

			ready := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					var port int
					if strings.HasPrefix(scanner.Text(), "Forwarding from 127.0.0.1:") {
						if _, err := fmt.Sscanf(scanner.Text(), "Forwarding from 127.0.0.1:%d", &port); err == nil {
							select {
							case ready <- fmt.Sprintf("http://127.0.0.1:%d", port):
							default:
							}
						}
					}
				}
			}()

			var target string
			select {
			case target = <-ready:
			case <-time.After(10 * time.Second):
				t.Fatal("port-forward readiness timeout")
			}

			r, err := load.Run(
				ctx,
				load.Config{
					Target:      target,
					Token:       token,
					Rate:        10,
					Concurrency: 4,
					MaxRequests: 300,
					Duration:    30 * time.Second,
				},
			)
			if err != nil {
				t.Fatal(err)
			}

			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			t.Log(string(data))
			if !r.Healthy() {
				t.Fatal("disposable API load failed")
			}
		})

		if phase == "baseline" {
			old := waitForAPIServerPod(t)
			start := time.Now()

			runCommand(
				t,
				"delete only disposable API pod",
				exec.Command("kubectl", "delete", "pod", old, "-n", namespace, "--timeout=60s"),
			)
			runCommand(
				t,
				"wait for API replacement readiness",
				exec.Command(
					"kubectl",
					"rollout",
					"status",
					"deployment/petri-apiserver",
					"-n",
					namespace,
					"--timeout=120s",
				),
			)
			if replacement := waitForAPIServerPod(t); replacement == old {
				t.Fatal("API pod was not replaced")
			}

			code, _ := request(t, "GET", "/readyz", "")
			if code != 200 {
				t.Fatalf("readiness not restored: %d", code)
			}
			t.Logf(
				"pod replacement and readiness recovery wall time: %s; not an outage duration measurement",
				time.Since(start),
			)
		}
	}
}
