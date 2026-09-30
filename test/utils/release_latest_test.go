package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestReleaseLatest(t *testing.T) {
	t.Parallel()
	root, err := GetProjectDir()
	if err != nil {
		t.Fatal(err)
	}
	image := "ghcr.io/petri-dev/petri-apiserver"
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, scenario := range []string{"stable", "older run", "prerelease", "foreign image", "wrong digest", "tag failure", "public verification failure", "lookup failure"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			fake := `#!/bin/sh
set -eu
printf '%s %s\n' "${0##*/}" "$*" >> "$CALLS"
case "${0##*/}:$1" in
  gh:release)
    case "$2" in
      view) printf '%s\n' "$PRERELEASE" ;;
      download)
        test "$3" = v2.0.0
        printf '%s\n' "$IMAGE_REF" ;;
      *) exit 1 ;;
    esac ;;
  gh:api)
    test "$2" = repos/petri-dev/petri-apiserver/releases/latest
    test "$LOOKUP_FAILURE" = false
    printf '%s\n' v2.0.0 ;;
  crane:auth)
    IFS= read -r token || true
    test "$token" = test-token
    printf '%s' "$DOCKER_CONFIG" > "$CONFIG_PATH" ;;
  crane:tag)
    test "$2" = "$IMAGE_REF"
    test "$3" = latest
    test "$TAG_FAILURE" = false ;;
  crane:digest)
    case "$2" in
      *:v2.0.0) printf '%s\n' "$REMOTE_DIGEST" ;;
      *:latest)
        test "${DOCKER_CONFIG##*/}" = anonymous
        test ! -e "$DOCKER_CONFIG/config.json"
        printf '%s\n' "$LATEST_DIGEST" ;;
      *) exit 1 ;;
    esac ;;
  *) exit 1 ;;
esac
`
			for _, name := range []string{"gh", "crane"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(fake), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ref, remote, latest, tag := image+"@"+digest, digest, digest, "v2.0.0"
			if scenario == "foreign image" {
				ref = "ghcr.io/other/image@" + digest
			}
			if scenario == "wrong digest" {
				remote = "sha256:" + strings.Repeat("b", 64)
			}
			if scenario == "public verification failure" {
				latest = "sha256:" + strings.Repeat("b", 64)
			}
			if scenario == "older run" {
				tag = "v1.5.0"
			}
			cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(root, "hack", "release-latest.sh"))
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GITHUB_REPOSITORY=petri-dev/petri-apiserver", "GITHUB_REF_NAME="+tag,
				"GITHUB_ACTOR=test-user", "GH_TOKEN=test-token", "CALLS="+filepath.Join(dir, "calls"),
				"CONFIG_PATH="+filepath.Join(dir, "config-path"), "IMAGE_REF="+ref,
				"REMOTE_DIGEST="+remote, "LATEST_DIGEST="+latest,
				"PRERELEASE="+strconv.FormatBool(scenario == "prerelease"),
				"TAG_FAILURE="+strconv.FormatBool(scenario == "tag failure"),
				"LOOKUP_FAILURE="+strconv.FormatBool(scenario == "lookup failure"))
			out, err := cmd.CombinedOutput()
			success := scenario == "stable" || scenario == "older run" || scenario == "prerelease"
			if (err == nil) != success {
				t.Fatalf("unexpected latest result: %v: %s", err, out)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			wrote := strings.Contains(string(calls), "crane tag ")
			wantWrite := scenario == "stable" || scenario == "older run" || scenario == "tag failure" || scenario == "public verification failure"
			if wrote != wantWrite {
				t.Fatalf("unexpected registry mutation: %s", calls)
			}
			if scenario == "prerelease" && strings.Count(string(calls), "\n") != 1 {
				t.Fatalf("prerelease must not access the registry: %s", calls)
			}
			if success && wrote && !strings.Contains(string(calls), "crane digest "+image+":latest") {
				t.Fatalf("latest was not verified: %s", calls)
			}
			if config, err := os.ReadFile(filepath.Join(dir, "config-path")); err == nil {
				if _, err := os.Stat(string(config)); !os.IsNotExist(err) {
					t.Fatal("temporary registry credentials were not removed")
				}
			}
		})
	}
}
