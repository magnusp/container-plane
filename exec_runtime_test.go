package cplane

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type recordedCommand struct {
	name string
	args []string
	env  []string
}

func commandKey(name string, args []string) string {
	return name + " " + strings.Join(args, " ")
}

type stubRunner struct {
	calls     []recordedCommand
	responses map[string]string
	fail      map[string]error
}

func (s *stubRunner) run(_ context.Context, name string, args []string, env []string) ([]byte, error) {
	key := commandKey(name, args)
	s.calls = append(s.calls, recordedCommand{name: name, args: args, env: env})
	if err := s.fail[key]; err != nil {
		return nil, err
	}
	return []byte(s.responses[key]), nil
}

func (s *stubRunner) last() recordedCommand {
	if len(s.calls) == 0 {
		return recordedCommand{}
	}
	return s.calls[len(s.calls)-1]
}

func testRuntime(runner *stubRunner) ExecRuntime {
	return NewExecRuntime(ExecConfig{
		Project:       "demo",
		Service:       "app",
		ComposeFile:   "/etc/demo/compose.yaml",
		EnvFile:       "/etc/demo/app.env",
		ImageVar:      "APP_IMAGE",
		Network:       "demo-net",
		NetworkCreate: true,
		DesiredImage:  "app:v2",
		Probe:         []string{"wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/"},
		Drain:         15 * time.Second,
		Run:           runner.run,
	})
}

const composeArgsPrefix = "compose -p demo -f /etc/demo/compose.yaml --env-file /etc/demo/app.env"
const composePrefix = "docker " + composeArgsPrefix

func TestInspectReportsNoContainer(t *testing.T) {
	runner := &stubRunner{responses: map[string]string{composePrefix + " ps -q app": "\n"}}
	state, err := testRuntime(runner).Inspect(t.Context())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if state.Exists {
		t.Fatalf("state = %+v, want a container that is not there", state)
	}
}

func TestInspectParsesTheRunningContainer(t *testing.T) {
	started := "2026-03-01T12:00:00.000000000Z"
	runner := &stubRunner{responses: map[string]string{
		composePrefix + " ps -q app":                  "cafe1234\n",
		"docker inspect --format {{json .}} cafe1234": `{"State":{"Running":true,"StartedAt":"` + started + `","Health":{"Status":"starting"}},"Image":"sha256:abc","Config":{"Image":"registry.example/app:v1"}}`,
	}}
	state, err := testRuntime(runner).Inspect(t.Context())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !state.Exists || !state.Running {
		t.Fatalf("state = %+v, want an existing running container", state)
	}
	if state.Digest != "sha256:abc" {
		t.Errorf("Digest = %q, want sha256:abc", state.Digest)
	}
	if state.ImageRef != "registry.example/app:v1" {
		t.Errorf("ImageRef = %q, want the image the container was created from", state.ImageRef)
	}
	if state.Health != "starting" {
		t.Errorf("Health = %q, want starting", state.Health)
	}
	want := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	if !state.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %s, want %s", state.StartedAt, want)
	}
}

func TestInspectWithoutAHealthcheck(t *testing.T) {
	runner := &stubRunner{responses: map[string]string{
		composePrefix + " ps -q app":                  "cafe1234\n",
		"docker inspect --format {{json .}} cafe1234": `{"State":{"Running":true,"StartedAt":"2026-03-01T12:00:00Z"},"Image":"sha256:abc","Config":{"Image":"registry.example/app:v1"}}`,
	}}
	state, err := testRuntime(runner).Inspect(t.Context())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if state.Health != "" {
		t.Errorf("Health = %q, want empty so the probe decides", state.Health)
	}
}

func TestImageDigest(t *testing.T) {
	runner := &stubRunner{responses: map[string]string{"docker image inspect --format {{.Id}} app:v2": "sha256:def\n"}}
	digest, err := testRuntime(runner).ImageDigest(t.Context(), "app:v2")
	if err != nil {
		t.Fatalf("ImageDigest: %v", err)
	}
	if digest != "sha256:def" {
		t.Errorf("digest = %q, want sha256:def", digest)
	}
}

func TestPullRunsBeforeAnyChange(t *testing.T) {
	runner := &stubRunner{}
	if err := testRuntime(runner).Pull(t.Context(), "app:v2"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	call := runner.last()
	if got := strings.Join(call.args, " "); got != composeArgsPrefix+" pull app" {
		t.Fatalf("ran %q, want %q", got, composeArgsPrefix+" pull app")
	}
	if !hasEnv(call.env, "APP_IMAGE=app:v2") || !hasEnv(call.env, NetworkEnvVar+"=demo-net") {
		t.Fatalf("env = %v, want the image and network on the pull", call.env)
	}
}

func TestPullFailureIsItsOwnClass(t *testing.T) {
	runner := &stubRunner{fail: map[string]error{
		composePrefix + " pull app": errors.New("manifest unknown"),
	}}
	err := testRuntime(runner).Pull(t.Context(), "app:v2")
	if !errors.Is(err, ErrPull) {
		t.Fatalf("Pull error = %v, want ErrPull", err)
	}
}

func TestEnsureNetworkCreatesWhenMissing(t *testing.T) {
	runner := &stubRunner{fail: map[string]error{
		"docker network inspect demo-net": errors.New("no such network"),
	}}
	if err := testRuntime(runner).EnsureNetwork(t.Context()); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %d, want inspect then create", len(runner.calls))
	}
	if got := strings.Join(runner.calls[1].args, " "); got != "network create demo-net" {
		t.Fatalf("ran %q, want network create demo-net", got)
	}
}

func TestEnsureNetworkJoinsAnExistingNetwork(t *testing.T) {
	runner := &stubRunner{}
	if err := testRuntime(runner).EnsureNetwork(t.Context()); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want only the inspect", len(runner.calls))
	}
}

func TestEnsureNetworkCanRefuseToCreate(t *testing.T) {
	runner := &stubRunner{fail: map[string]error{
		"docker network inspect demo-net": errors.New("no such network"),
	}}
	runtime := testRuntime(runner)
	runtime.NetworkCreate = false
	err := runtime.EnsureNetwork(t.Context())
	if !errors.Is(err, ErrCompose) {
		t.Fatalf("EnsureNetwork error = %v, want ErrCompose", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("calls = %d, want no create attempt", len(runner.calls))
	}
}

func TestEnsureNetworkDoesNothingWithoutANetwork(t *testing.T) {
	runner := &stubRunner{}
	runtime := testRuntime(runner)
	runtime.Network = ""
	if err := runtime.EnsureNetwork(t.Context()); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("calls = %v, want none", runner.calls)
	}
}

func TestStopHonoursTheDrain(t *testing.T) {
	runner := &stubRunner{}
	if err := testRuntime(runner).Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	want := composeArgsPrefix + " stop --timeout 15 app"
	if got := strings.Join(runner.last().args, " "); got != want {
		t.Fatalf("ran %q, want %q", got, want)
	}
}

func TestStartPassesTheImageThroughTheEnvironment(t *testing.T) {
	runner := &stubRunner{}
	if err := testRuntime(runner).Start(t.Context(), "app:v2"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	call := runner.last()
	want := composeArgsPrefix + " up -d --no-deps app"
	if got := strings.Join(call.args, " "); got != want {
		t.Fatalf("ran %q, want %q", got, want)
	}
	if !hasEnv(call.env, "APP_IMAGE=app:v2") {
		t.Fatalf("env = %v, want APP_IMAGE=app:v2", call.env)
	}
}

func TestEveryComposeCallCarriesTheImageAndNetwork(t *testing.T) {
	runner := &stubRunner{responses: map[string]string{
		composePrefix + " ps -q app": "\n",
	}}
	runtime := testRuntime(runner)
	if _, err := runtime.Inspect(t.Context()); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if err := runtime.Pull(t.Context(), "app:v2"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if err := runtime.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := runtime.Start(t.Context(), "app:v2"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for _, call := range runner.calls {
		if len(call.args) == 0 || call.args[0] != "compose" {
			continue
		}
		if !hasEnv(call.env, "APP_IMAGE=app:v2") {
			t.Errorf("%v missing APP_IMAGE", call.args)
		}
		if !hasEnv(call.env, NetworkEnvVar+"=demo-net") {
			t.Errorf("%v missing %s", call.args, NetworkEnvVar)
		}
	}
}

func TestReadyRunsTheProbeInsideTheContainer(t *testing.T) {
	runner := &stubRunner{responses: map[string]string{
		composePrefix + " ps -q app":                                       "cafe1234\n",
		"docker exec cafe1234 wget -q -O /dev/null http://127.0.0.1:8080/": "",
	}}
	ready, err := testRuntime(runner).Ready(t.Context())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !ready {
		t.Fatal("Ready = false, want true")
	}
	if got := strings.Join(runner.last().args, " "); got != "exec cafe1234 wget -q -O /dev/null http://127.0.0.1:8080/" {
		t.Fatalf("ran %q, want the probe inside the container", got)
	}
}

func TestReadyTreatsAProbeExitAsNotReady(t *testing.T) {
	exitErr := exec.Command("sh", "-c", "exit 1").Run()
	runner := &stubRunner{
		responses: map[string]string{composePrefix + " ps -q app": "cafe1234\n"},
		fail: map[string]error{
			"docker exec cafe1234 wget -q -O /dev/null http://127.0.0.1:8080/": exitErr,
		},
	}
	ready, err := testRuntime(runner).Ready(t.Context())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if ready {
		t.Fatal("Ready = true, want false for a non-zero probe")
	}
}

func TestReadyNeedsAProbe(t *testing.T) {
	runtime := testRuntime(&stubRunner{})
	runtime.Probe = nil
	if _, err := runtime.Ready(t.Context()); !errors.Is(err, ErrNoProbe) {
		t.Fatalf("Ready error = %v, want ErrNoProbe", err)
	}
}

func TestEnvFileIsOmittedWhenUnset(t *testing.T) {
	runner := &stubRunner{responses: map[string]string{
		"docker compose -p demo -f /etc/demo/compose.yaml ps -q app": "\n",
	}}
	runtime := testRuntime(runner)
	runtime.EnvFile = ""
	if _, err := runtime.Inspect(t.Context()); err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if got := strings.Join(runner.calls[0].args, " "); strings.Contains(got, "--env-file") {
		t.Fatalf("ran %q, want no --env-file when none is configured", got)
	}
}

func hasEnv(env []string, want string) bool {
	for _, value := range env {
		if value == want {
			return true
		}
	}
	return false
}
