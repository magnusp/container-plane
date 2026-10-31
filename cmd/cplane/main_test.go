package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magnusp/container-plane"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "success", err: nil, want: 0},
		{name: "usage", err: cplane.ErrUsage, want: 2},
		{name: "pull", err: cplane.ErrPull, want: 3},
		{name: "compose", err: cplane.ErrCompose, want: 4},
		{name: "deploy", err: cplane.ErrDeploy, want: 1},
		{name: "rolled back", err: cplane.ErrRolledBack, want: 1},
		{name: "cancelled", err: context.Canceled, want: 1},
		{name: "unknown", err: errors.New("boom"), want: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := exitCode(test.err); got != test.want {
				t.Fatalf("exitCode(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"version"}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stdout.String(), "cplane") {
		t.Fatalf("stdout = %q, want the build identity", stdout.String())
	}
}

func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"help"}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stdout.String(), "cplane deploy") {
		t.Fatalf("stdout = %q, want usage", stdout.String())
	}
}

func TestRunWithoutACommandIsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(t.Context(), nil, &stdout, &stderr)
	if !errors.Is(err, cplane.ErrUsage) {
		t.Fatalf("run error = %v, want ErrUsage", err)
	}
}

func TestRunRejectsAnUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{"swarm"}, &stdout, &stderr)
	if !errors.Is(err, cplane.ErrUsage) {
		t.Fatalf("run error = %v, want ErrUsage", err)
	}
}

func TestDeployRequiresAnImage(t *testing.T) {
	t.Setenv("CPLANE_IMAGE", "")
	t.Setenv("CPLANE_SERVICE", "app")
	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{"deploy"}, &stdout, &stderr)
	if !errors.Is(err, cplane.ErrUsage) {
		t.Fatalf("run error = %v, want ErrUsage", err)
	}
}

func TestDeployRequiresAService(t *testing.T) {
	t.Setenv("CPLANE_IMAGE", "app:v1")
	t.Setenv("CPLANE_SERVICE", "")
	var stdout, stderr bytes.Buffer
	err := run(t.Context(), []string{"deploy"}, &stdout, &stderr)
	if !errors.Is(err, cplane.ErrUsage) {
		t.Fatalf("run error = %v, want ErrUsage", err)
	}
}

func TestDeployHelpReturnsWithoutRunning(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"deploy", "--help"}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestReadEnvRejectsABadDuration(t *testing.T) {
	t.Setenv("CPLANE_HEALTH_BUDGET", "soon")
	_, err := readEnv()
	if !errors.Is(err, cplane.ErrUsage) {
		t.Fatalf("readEnv error = %v, want ErrUsage", err)
	}
}

func TestReadEnvRejectsABadBoolean(t *testing.T) {
	t.Setenv("CPLANE_PULL", "maybe")
	_, err := readEnv()
	if !errors.Is(err, cplane.ErrUsage) {
		t.Fatalf("readEnv error = %v, want ErrUsage", err)
	}
}

func TestReadEnvDefaults(t *testing.T) {
	for _, key := range []string{
		"CPLANE_IMAGE", "CPLANE_PROJECT", "CPLANE_SERVICE", "CPLANE_COMPOSE_FILE",
		"CPLANE_ENV_FILE", "CPLANE_IMAGE_VAR", "CPLANE_NETWORK", "CPLANE_NETWORK_CREATE",
		"CPLANE_PULL", "CPLANE_DRAIN", "CPLANE_HEALTH_BUDGET", "CPLANE_HEALTH_INTERVAL",
		"CPLANE_HEALTH_TIMEOUT", "CPLANE_HEALTH_RETRIES", "CPLANE_PROBE", "CPLANE_DOCKER",
	} {
		t.Setenv(key, "")
	}
	env, err := readEnv()
	if err != nil {
		t.Fatalf("readEnv: %v", err)
	}
	if env.composeFile != "compose.yaml" || env.imageVar != "IMAGE" || env.docker != "docker" {
		t.Fatalf("env = %+v, want neutral defaults", env)
	}
	if !env.pull || !env.networkCreate {
		t.Fatalf("env = %+v, want pull and network creation on by default", env)
	}
}

func TestCapturePreviousReadsTheRunningContainer(t *testing.T) {
	runtime := cplane.NewExecRuntime(cplane.ExecConfig{
		Project:      "demo",
		Service:      "app",
		ComposeFile:  "compose.yaml",
		DesiredImage: "app:v2",
		Run: func(_ context.Context, name string, args []string, _ []string) ([]byte, error) {
			key := name + " " + strings.Join(args, " ")
			if strings.Contains(key, " ps -q app") {
				return []byte("cafe1234\n"), nil
			}
			return []byte(`{"State":{"Running":true},"Image":"sha256:old","Config":{"Image":"app:v1"}}`), nil
		},
	})
	desired := cplane.Deployment{Image: "app:v2"}
	if err := capturePrevious(t.Context(), runtime, &desired); err != nil {
		t.Fatalf("capturePrevious: %v", err)
	}
	if desired.PreviousImage != "app:v1" {
		t.Fatalf("PreviousImage = %q, want app:v1", desired.PreviousImage)
	}
}

func TestCapturePreviousFallsBackToTheImageID(t *testing.T) {
	runtime := cplane.NewExecRuntime(cplane.ExecConfig{
		Project:      "demo",
		Service:      "app",
		ComposeFile:  "compose.yaml",
		DesiredImage: "app:v2",
		Run: func(_ context.Context, name string, args []string, _ []string) ([]byte, error) {
			key := name + " " + strings.Join(args, " ")
			if strings.Contains(key, " ps -q app") {
				return []byte("cafe1234\n"), nil
			}
			return []byte(`{"State":{"Running":true},"Image":"sha256:old","Config":{}}`), nil
		},
	})
	desired := cplane.Deployment{Image: "app:v2"}
	if err := capturePrevious(t.Context(), runtime, &desired); err != nil {
		t.Fatalf("capturePrevious: %v", err)
	}
	if desired.PreviousImage != "sha256:old" {
		t.Fatalf("PreviousImage = %q, want the image ID", desired.PreviousImage)
	}
}

func TestCapturePreviousWithoutAContainer(t *testing.T) {
	runtime := cplane.NewExecRuntime(cplane.ExecConfig{
		Project:      "demo",
		Service:      "app",
		ComposeFile:  "compose.yaml",
		DesiredImage: "app:v2",
		Run: func(context.Context, string, []string, []string) ([]byte, error) {
			return []byte("\n"), nil
		},
	})
	desired := cplane.Deployment{Image: "app:v2"}
	if err := capturePrevious(t.Context(), runtime, &desired); err != nil {
		t.Fatalf("capturePrevious: %v", err)
	}
	if desired.PreviousImage != "" {
		t.Fatalf("PreviousImage = %q, want empty", desired.PreviousImage)
	}
}

func TestDefaultProjectUsesTheComposeFileDirectory(t *testing.T) {
	dir := t.TempDir()
	if got := defaultProject(filepath.Join(dir, "compose.yaml")); got != filepath.Base(dir) {
		t.Fatalf("defaultProject = %q, want %q", got, filepath.Base(dir))
	}
}
