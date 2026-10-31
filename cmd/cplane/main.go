package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/magnusp/container-plane"
)

// Build identity. Stamped by GoReleaser through -ldflags; see
// .goreleaser.yaml. An operator holding a broken host needs to know which build
// of the deploy gate produced the failure.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `cplane - deploy one Docker Compose service behind a health gate

Usage:
  cplane deploy --image <ref> --service <name> [flags]
  cplane version

deploy pulls the image, joins or creates the shared network, captures the
running container's image, starts the new image, and waits for readiness. If
the health budget is spent, it starts the captured image again and exits 1.
Nothing is written to disk.

Every flag also has an environment variable:

  --image            CPLANE_IMAGE
  --project          CPLANE_PROJECT
  --service          CPLANE_SERVICE
  --compose-file     CPLANE_COMPOSE_FILE     (default compose.yaml)
  --env-file         CPLANE_ENV_FILE
  --image-var        CPLANE_IMAGE_VAR        (default IMAGE)
  --network          CPLANE_NETWORK
  --network-create   CPLANE_NETWORK_CREATE   (default true)
  --pull             CPLANE_PULL             (default true)
  --drain            CPLANE_DRAIN            (default 30s)
  --health-budget    CPLANE_HEALTH_BUDGET    (default 60s)
  --health-interval  CPLANE_HEALTH_INTERVAL  (default 2s)
  --health-timeout   CPLANE_HEALTH_TIMEOUT   (default 3s)
  --health-retries   CPLANE_HEALTH_RETRIES   (default 3)
  --probe            CPLANE_PROBE
  --docker           CPLANE_DOCKER           (default docker)

Exit status:
  0  the deploy settled
  1  the deploy failed and no previous image was captured, or it was restored
  2  bad usage or a configuration the tool could not run
  3  the image pull failed; nothing was stopped
  4  a Docker or Docker Compose operation failed
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "cplane:", err)
		os.Exit(exitCode(err))
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("%w: a command is required", cplane.ErrUsage)
	}

	command, rest := args[0], args[1:]
	switch command {
	case "deploy":
		return runDeploy(ctx, rest, stdout, stderr)
	case "version", "--version":
		fmt.Fprintf(stdout, "cplane %s\ncommit %s\nbuilt  %s\n", version, commit, date)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprint(stderr, usage)
		return fmt.Errorf("%w: unknown command %q", cplane.ErrUsage, command)
	}
}

type deployEnv struct {
	image          string
	project        string
	service        string
	composeFile    string
	envFile        string
	imageVar       string
	network        string
	networkCreate  bool
	pull           bool
	drain          time.Duration
	healthBudget   time.Duration
	healthInterval time.Duration
	healthTimeout  time.Duration
	healthRetries  int
	probe          string
	docker         string
}

func readEnv() (deployEnv, error) {
	var env deployEnv
	var err error

	env.image = envString("CPLANE_IMAGE", "")
	env.project = envString("CPLANE_PROJECT", "")
	env.service = envString("CPLANE_SERVICE", "")
	env.composeFile = envString("CPLANE_COMPOSE_FILE", "compose.yaml")
	env.envFile = envString("CPLANE_ENV_FILE", "")
	env.imageVar = envString("CPLANE_IMAGE_VAR", "IMAGE")
	env.network = envString("CPLANE_NETWORK", "")
	env.probe = envString("CPLANE_PROBE", "")
	env.docker = envString("CPLANE_DOCKER", "docker")

	if env.networkCreate, err = envBool("CPLANE_NETWORK_CREATE", true); err != nil {
		return deployEnv{}, err
	}
	if env.pull, err = envBool("CPLANE_PULL", true); err != nil {
		return deployEnv{}, err
	}
	if env.drain, err = envDuration("CPLANE_DRAIN", 30*time.Second); err != nil {
		return deployEnv{}, err
	}
	if env.healthBudget, err = envDuration("CPLANE_HEALTH_BUDGET", 60*time.Second); err != nil {
		return deployEnv{}, err
	}
	if env.healthInterval, err = envDuration("CPLANE_HEALTH_INTERVAL", 2*time.Second); err != nil {
		return deployEnv{}, err
	}
	if env.healthTimeout, err = envDuration("CPLANE_HEALTH_TIMEOUT", 3*time.Second); err != nil {
		return deployEnv{}, err
	}
	if env.healthRetries, err = envInt("CPLANE_HEALTH_RETRIES", 3); err != nil {
		return deployEnv{}, err
	}
	return env, nil
}

func runDeploy(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	env, err := readEnv()
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(stderr)

	image := fs.String("image", env.image, "image reference to deploy")
	project := fs.String("project", env.project, "compose project name")
	service := fs.String("service", env.service, "compose service to deploy")
	composeFile := fs.String("compose-file", env.composeFile, "compose file")
	envFile := fs.String("env-file", env.envFile, "compose interpolation env file")
	imageVar := fs.String("image-var", env.imageVar, "compose variable that holds the image")
	network := fs.String("network", env.network, "shared network to join or create")
	networkCreate := fs.Bool("network-create", env.networkCreate, "create the shared network when missing")
	pull := fs.Bool("pull", env.pull, "pull the image before deploying")
	drain := fs.Duration("drain", env.drain, "grace period for the stop call")
	budget := fs.Duration("health-budget", env.healthBudget, "time a boot may take before it is failed")
	interval := fs.Duration("health-interval", env.healthInterval, "delay between readiness probes")
	timeout := fs.Duration("health-timeout", env.healthTimeout, "timeout for one readiness probe")
	retries := fs.Int("health-retries", env.healthRetries, "readiness probe attempts per pass")
	probe := fs.String("probe", env.probe, "readiness command run inside the container")
	docker := fs.String("docker", env.docker, "docker binary")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %w", cplane.ErrUsage, err)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: unexpected argument %q", cplane.ErrUsage, fs.Arg(0))
	}
	if *image == "" {
		return fmt.Errorf("%w: --image or CPLANE_IMAGE is required", cplane.ErrUsage)
	}
	if *service == "" {
		return fmt.Errorf("%w: --service or CPLANE_SERVICE is required", cplane.ErrUsage)
	}
	if *project == "" {
		*project = defaultProject(*composeFile)
	}

	desired := cplane.Deployment{
		Image: *image,
		Health: cplane.Health{
			Interval: *interval,
			Timeout:  *timeout,
			Retries:  *retries,
			Budget:   *budget,
		},
	}
	if err := desired.Validate(); err != nil {
		return err
	}

	runtime := cplane.NewExecRuntime(cplane.ExecConfig{
		DockerBin:     *docker,
		Project:       *project,
		Service:       *service,
		ComposeFile:   *composeFile,
		EnvFile:       *envFile,
		ImageVar:      *imageVar,
		Network:       *network,
		NetworkCreate: *networkCreate,
		DesiredImage:  *image,
		Probe:         strings.Fields(*probe),
		Drain:         *drain,
	})

	// Capture the rollback target before the pull. A pull can move a mutable
	// tag, and a reference that resolves to the new image is not a rollback
	// target. The reconciler keeps the same capture as a fallback for direct
	// library use.
	if err := capturePrevious(ctx, runtime, &desired); err != nil {
		return err
	}

	if *pull {
		if err := runtime.Pull(ctx, *image); err != nil {
			return err
		}
	}
	if err := runtime.EnsureNetwork(ctx); err != nil {
		return err
	}

	outcome, err := (&cplane.Reconciler{
		Desired:   desired,
		Runtime:   runtime,
		Inspector: runtime,
		Log:       slog.Default(),
	}).Run(ctx)

	switch {
	case errors.Is(err, cplane.ErrRolledBack):
		fmt.Fprintf(stdout, "deploy of %s failed: %s\n", *image, outcome.Decision.Reason)
		fmt.Fprintf(stdout, "restored %s\n", outcome.Decision.Image)
		return err
	case err != nil:
		return err
	case !outcome.Settled:
		return fmt.Errorf("%w: deploy of %s did not settle", cplane.ErrDeploy, *image)
	}
	fmt.Fprintf(stdout, "deployed %s\n", *image)
	return nil
}

func capturePrevious(ctx context.Context, runtime cplane.ExecRuntime, desired *cplane.Deployment) error {
	state, err := runtime.Inspect(ctx)
	if err != nil {
		return fmt.Errorf("%w: inspect the running container: %w", cplane.ErrCompose, err)
	}
	if !state.Exists {
		return nil
	}
	desired.PreviousImage = state.ImageRef
	if desired.PreviousImage == "" {
		desired.PreviousImage = state.Digest
	}
	return nil
}

func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, cplane.ErrUsage):
		return 2
	case errors.Is(err, cplane.ErrPull):
		return 3
	case errors.Is(err, cplane.ErrCompose):
		return 4
	case errors.Is(err, cplane.ErrDeploy), errors.Is(err, cplane.ErrRolledBack):
		return 1
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return 1
	default:
		return 2
	}
}

func defaultProject(composeFile string) string {
	if abs, err := filepath.Abs(composeFile); err == nil {
		base := filepath.Base(filepath.Dir(abs))
		if base != "" && base != "." && base != string(filepath.Separator) {
			return base
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return filepath.Base(cwd)
	}
	return ""
}

func envString(key, def string) string {
	if value, ok := os.LookupEnv(key); ok && value != "" {
		return value
	}
	return def
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return def, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%w: %s=%q is not a duration: %w", cplane.ErrUsage, key, value, err)
	}
	return parsed, nil
}

func envInt(key string, def int) (int, error) {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return def, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%w: %s=%q is not an integer: %w", cplane.ErrUsage, key, value, err)
	}
	return parsed, nil
}

func envBool(key string, def bool) (bool, error) {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return def, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%w: %s=%q is not a boolean: %w", cplane.ErrUsage, key, value, err)
	}
	return parsed, nil
}
