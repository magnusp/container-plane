package cplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// CommandRunner executes one Docker command and returns its combined output.
// It is a field rather than a package function so tests can assert the exact
// argv without a Docker daemon anywhere in sight.
type CommandRunner func(ctx context.Context, name string, args []string, env []string) ([]byte, error)

func runCommand(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.Bytes(), nil
}

// NetworkEnvVar is the variable the compose file uses to join the shared
// network. The tool passes the resolved name on every compose call. A compose
// file opts in by declaring an external network named ${CPLANE_NETWORK}.
const NetworkEnvVar = "CPLANE_NETWORK"

// ExecConfig is the boundary between deployment inputs and Docker. Every value
// here comes from a flag or an environment variable; no file is read.
type ExecConfig struct {
	DockerBin     string
	Project       string
	Service       string
	ComposeFile   string
	EnvFile       string
	ImageVar      string
	Network       string
	NetworkCreate bool
	DesiredImage  string
	Probe         []string
	Drain         time.Duration
	Run           CommandRunner
}

// ExecRuntime implements Runtime by shelling out to `docker` and
// `docker compose`. No Docker client library: the CLI is the interface, and
// that is also what an operator can reproduce by hand.
type ExecRuntime struct {
	DockerBin     string
	Project       string
	Service       string
	ComposeFile   string
	EnvFile       string
	ImageVar      string
	Network       string
	NetworkCreate bool
	DesiredImage  string
	Probe         []string
	Drain         time.Duration

	Run CommandRunner
}

// NewExecRuntime builds a runtime from deployment inputs.
func NewExecRuntime(cfg ExecConfig) ExecRuntime {
	return ExecRuntime{
		DockerBin:     cfg.DockerBin,
		Project:       cfg.Project,
		Service:       cfg.Service,
		ComposeFile:   cfg.ComposeFile,
		EnvFile:       cfg.EnvFile,
		ImageVar:      cfg.ImageVar,
		Network:       cfg.Network,
		NetworkCreate: cfg.NetworkCreate,
		DesiredImage:  cfg.DesiredImage,
		Probe:         append([]string(nil), cfg.Probe...),
		Drain:         cfg.Drain,
		Run:           cfg.Run,
	}
}

func (r ExecRuntime) withDefaults() ExecRuntime {
	if r.DockerBin == "" {
		r.DockerBin = "docker"
	}
	if r.ImageVar == "" {
		r.ImageVar = "IMAGE"
	}
	if r.ComposeFile == "" {
		r.ComposeFile = "compose.yaml"
	}
	if r.Run == nil {
		r.Run = runCommand
	}
	return r
}

func (r ExecRuntime) compose(args []string) []string {
	full := []string{"compose"}
	if r.Project != "" {
		full = append(full, "-p", r.Project)
	}
	if r.ComposeFile != "" {
		full = append(full, "-f", r.ComposeFile)
	}
	// Compose interpolates ${VAR} from this file, not from a service's env_file.
	if r.EnvFile != "" {
		full = append(full, "--env-file", r.EnvFile)
	}
	return append(full, args...)
}

// composeEnv carries the image on every compose call. A compose file that
// guards its image with ${VAR:?} aborts stop, ps, and pull without it, so the
// variable cannot be set only on the start call.
func (r ExecRuntime) composeEnv(image string) []string {
	var env []string
	if r.ImageVar != "" && image != "" {
		env = append(env, r.ImageVar+"="+image)
	}
	if r.Network != "" {
		env = append(env, NetworkEnvVar+"="+r.Network)
	}
	return env
}

func (r ExecRuntime) runCompose(ctx context.Context, args []string, image string) ([]byte, error) {
	r = r.withDefaults()
	return r.Run(ctx, r.DockerBin, r.compose(args), r.composeEnv(image))
}

// Pull fetches the desired image before anything is stopped.
func (r ExecRuntime) Pull(ctx context.Context, image string) error {
	if _, err := r.runCompose(ctx, []string{"pull", r.Service}, image); err != nil {
		return fmt.Errorf("%w: %w", ErrPull, err)
	}
	return nil
}

// EnsureNetwork joins an existing network or creates it when it is missing.
// Infrastructure (databases, caches, proxies) is deployed separately; this only
// makes the shared network exist.
func (r ExecRuntime) EnsureNetwork(ctx context.Context) error {
	r = r.withDefaults()
	if r.Network == "" {
		return nil
	}
	if _, err := r.Run(ctx, r.DockerBin, []string{"network", "inspect", r.Network}, nil); err == nil {
		return nil
	}
	if !r.NetworkCreate {
		return fmt.Errorf("%w: network %s does not exist and network creation is disabled", ErrCompose, r.Network)
	}
	if _, err := r.Run(ctx, r.DockerBin, []string{"network", "create", r.Network}, nil); err != nil {
		return fmt.Errorf("%w: create network %s: %w", ErrCompose, r.Network, err)
	}
	return nil
}

type containerJSON struct {
	State struct {
		Running   bool   `json:"Running"`
		StartedAt string `json:"StartedAt"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Image  string `json:"Image"`
	Config struct {
		Image string `json:"Image"`
	} `json:"Config"`
}

// Inspect reports the single app container. A compose service with no container
// yet is not an error; it is PhaseAbsent.
func (r ExecRuntime) Inspect(ctx context.Context) (ContainerState, error) {
	r = r.withDefaults()
	id, err := r.containerID(ctx)
	if err != nil || id == "" {
		return ContainerState{}, err
	}
	out, err := r.Run(ctx, r.DockerBin, []string{"inspect", "--format", "{{json .}}", id}, nil)
	if err != nil {
		return ContainerState{}, err
	}
	var payload containerJSON
	if err := json.Unmarshal(bytes.TrimSpace(out), &payload); err != nil {
		return ContainerState{}, fmt.Errorf("parse docker inspect %s: %w", id, err)
	}
	state := ContainerState{
		Exists:   true,
		Running:  payload.State.Running,
		Digest:   payload.Image,
		ImageRef: payload.Config.Image,
	}
	if payload.State.Health != nil {
		state.Health = payload.State.Health.Status
	}
	if started, err := time.Parse(time.RFC3339Nano, payload.State.StartedAt); err == nil {
		state.StartedAt = started
	}
	return state, nil
}

func (r ExecRuntime) containerID(ctx context.Context) (string, error) {
	out, err := r.runCompose(ctx, []string{"ps", "-q", r.Service}, r.DesiredImage)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if i := strings.IndexByte(id, '\n'); i >= 0 {
		id = id[:i]
	}
	return strings.TrimSpace(id), nil
}

func (r ExecRuntime) ImageDigest(ctx context.Context, image string) (string, error) {
	r = r.withDefaults()
	out, err := r.Run(ctx, r.DockerBin, []string{"image", "inspect", "--format", "{{.Id}}", image}, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (r ExecRuntime) Stop(ctx context.Context) error {
	r = r.withDefaults()
	args := []string{"stop"}
	if r.Drain > 0 {
		args = append(args, "--timeout", strconv.Itoa(int(r.Drain.Seconds())))
	}
	args = append(args, r.Service)
	_, err := r.runCompose(ctx, args, r.DesiredImage)
	return err
}

func (r ExecRuntime) Start(ctx context.Context, image string) error {
	r = r.withDefaults()
	_, err := r.runCompose(ctx, []string{"up", "-d", "--no-deps", r.Service}, image)
	return err
}

// Ready runs the probe inside the running container, so a CI runner needs no
// published loopback port and no route to the service. A non-zero probe is a
// "not ready yet" answer; only the Docker CLI failing to run is an error.
func (r ExecRuntime) Ready(ctx context.Context) (bool, error) {
	r = r.withDefaults()
	if len(r.Probe) == 0 {
		return false, ErrNoProbe
	}
	id, err := r.containerID(ctx)
	if err != nil {
		return false, err
	}
	if id == "" {
		return false, nil
	}
	args := append([]string{"exec", id}, r.Probe...)
	if _, err := r.Run(ctx, r.DockerBin, args, nil); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
