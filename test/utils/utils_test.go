package utils

import "testing"

func TestLoadImageRequiresExplicitCluster(t *testing.T) {
	t.Setenv("KIND_CLUSTER", "")
	if err := LoadImageToKindClusterWithName(t.Context(), "unused"); err == nil {
		t.Fatal("expected refusal before invoking Docker or Kind")
	}
}

func TestFindProjectDirFailsOutsideRepository(t *testing.T) {
	t.Parallel()

	if _, err := findProjectDir(t.TempDir()); err == nil {
		t.Fatal("expected an error when go.mod is not found")
	}
}
