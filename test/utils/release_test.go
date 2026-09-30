package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseManifest(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is required to render release manifests")
	}
	root, err := GetProjectDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.CopyFS(filepath.Join(dir, "config"), os.DirFS(filepath.Join(root, "config"))); err != nil {
		t.Fatal(err)
	}
	image := "ghcr.io/petri-dev/petri-apiserver@sha256:" + strings.Repeat("a", 64)
	cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(root, "hack", "release-manifests.sh"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "RELEASE_IMAGE="+image)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render manifest: %v: %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "release", "install.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "image: "+image) != 1 || strings.Contains(string(data), ":latest") {
		t.Fatal("release manifest did not pin the deployment to the scanned digest")
	}

	cmd = exec.CommandContext(t.Context(), "bash", filepath.Join(root, "hack", "release-manifests.sh"))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "RELEASE_IMAGE=ghcr.io/petri-dev/petri-apiserver:latest")
	if err := cmd.Run(); err == nil {
		t.Fatal("mutable release image accepted")
	}
}

func TestReleaseSmoke(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required to read release metadata")
	}
	root, err := GetProjectDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"success", "wrong digest", "pull failure", "missing platform", "invalid platform digest"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "release"), 0700); err != nil {
				t.Fatal(err)
			}
			digest := "sha256:" + strings.Repeat("a", 64)
			amd64Digest := "sha256:" + strings.Repeat("b", 64)
			arm64Digest := "sha256:" + strings.Repeat("c", 64)
			fixture := `{"containerimage.digest":"` + digest + `"}`
			fake := `#!/bin/sh
set -eu
test "$DOCKER_CONFIG" != "$ORIGINAL_CONFIG"
test ! -e "$DOCKER_CONFIG/config.json"
printf '%s\n' "$*" >> "$CALLS"
case "$0" in
  */crane)
    if test "$2" = --platform; then
      test "$PLATFORM_FAILURE" != true
      case "$3" in
        linux/amd64) printf '%s\n' "$AMD64_DIGEST" ;;
        linux/arm64) printf '%s\n' "$ARM64_DIGEST" ;;
        *) exit 1 ;;
      esac
    else
      printf '%s\n' "$REMOTE_DIGEST"
    fi ;;
  */docker) test "$PULL_FAILURE" != true ;;
esac
`
			for name, data := range map[string]string{"release/build.json": fixture, "crane": fake, "docker": fake} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0700); err != nil {
					t.Fatal(err)
				}
			}
			remote, fail, platformFail := digest, "false", "false"
			if scenario == "wrong digest" {
				remote = "sha256:" + strings.Repeat("d", 64)
			}
			if scenario == "pull failure" {
				fail = "true"
			}
			if scenario == "missing platform" {
				platformFail = "true"
			}
			if scenario == "invalid platform digest" {
				amd64Digest = "invalid"
			}
			cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(root, "hack", "release-smoke.sh"))
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GITHUB_REPOSITORY=petri-dev/petri-apiserver", "RELEASE_VERSION=v1.2.3",
				"DOCKER_CONFIG="+dir, "ORIGINAL_CONFIG="+dir, "CALLS="+filepath.Join(dir, "calls"),
				"REMOTE_DIGEST="+remote, "PULL_FAILURE="+fail, "PLATFORM_FAILURE="+platformFail,
				"AMD64_DIGEST="+amd64Digest, "ARM64_DIGEST="+arm64Digest)
			out, err := cmd.CombinedOutput()
			if (err == nil) != (scenario == "success") {
				t.Fatalf("unexpected smoke result: %v: %s", err, out)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			image := "ghcr.io/petri-dev/petri-apiserver"
			want := "digest " + image + ":v1.2.3\n"
			if scenario != "wrong digest" {
				want += "digest --platform linux/amd64 " + image + "@" + digest + "\n"
			}
			if scenario == "success" || scenario == "pull failure" {
				want += "pull --platform linux/amd64 " + image + "@" + amd64Digest + "\n"
			}
			if scenario == "success" {
				want += "digest --platform linux/arm64 " + image + "@" + digest + "\n"
				want += "pull --platform linux/arm64 " + image + "@" + arm64Digest + "\n"
			}
			if string(calls) != want {
				t.Fatalf("unexpected registry calls: %s", calls)
			}
		})
	}
}
