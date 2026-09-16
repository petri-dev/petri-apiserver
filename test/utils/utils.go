package utils

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const defaultKindBinary = "kind"

func Run(cmd *exec.Cmd) (string, error) {
	dir, err := GetProjectDir()
	if err != nil {
		return "", err
	}
	return RunInDir(cmd, dir)
}

func RunInDir(cmd *exec.Cmd, dir string) (string, error) {
	cmd.Dir = dir
	command := strings.Join(cmd.Args, " ")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("%q failed with error %q: %w", command, string(output), err)
	}

	return string(output), nil
}

func LoadImageToKindClusterWithName(ctx context.Context, name string) error {
	cluster := os.Getenv("KIND_CLUSTER")
	if cluster == "" {
		return errors.New("KIND_CLUSTER must name the disposable test cluster")
	}

	kindBinary := defaultKindBinary
	if v, ok := os.LookupEnv("KIND"); ok {
		kindBinary = v
	}

	arch, err := Run(exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Arch}}"))
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "petri-image-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	archive := filepath.Join(dir, "image.tar")
	if _, saveErr := Run(
		exec.CommandContext(
			ctx,
			"docker",
			"image",
			"save",
			"--platform",
			"linux/"+strings.TrimSpace(arch),
			"-o",
			archive,
			name,
		),
	); saveErr != nil {
		return saveErr
	}

	cmd := exec.CommandContext(ctx, kindBinary, "load", "image-archive", archive, "--name", cluster)
	_, err = Run(cmd)
	return err
}

func RandomSuffix() string {
	const (
		chars         = "abcdefghijklmnopqrstuvwxyz0123456789"
		randomSuffLen = 6
	)
	b := make([]byte, randomSuffLen)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	return findProjectDir(wd)
}

func findProjectDir(start string) (string, error) {
	for dir := filepath.Clean(start); ; dir = filepath.Dir(dir) {
		_, statErr := os.Stat(filepath.Join(dir, "go.mod"))
		if statErr == nil {
			return dir, nil
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return "", fmt.Errorf("failed to inspect project directory %q: %w", dir, statErr)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("project root not found from %q: go.mod is missing", start)
		}
	}
}
