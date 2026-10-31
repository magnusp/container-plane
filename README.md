# cplane

`cplane` deploys one Docker Compose service behind a readiness gate. It pulls
the image, joins or creates a shared network, captures the image the running
container was created from, starts the new image, and waits for the service to
become ready. If the wait fails, it starts the captured image again and exits
non-zero. It writes no state file, history, or transition log.

It is deliberately small. The job is the part a shell wrapper gets wrong:
telling a pull failure, a compose error, and a health timeout apart, and
restoring the image that was actually running before the deploy.

## The compose contract

`cplane` drives one service in one compose file. The file provides three
things:

- The service the caller names with `--service`.
- An image that interpolates a variable, by default `${IMAGE}`.
- Optionally, an external network named `${CPLANE_NETWORK}`.

```yaml
services:
  app:
    image: ${IMAGE:?set IMAGE to the image to deploy}
    networks:
      - shared
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/"]
      interval: 5s
      timeout: 3s
      retries: 3
      start_period: 20s

networks:
  shared:
    external: true
    name: ${CPLANE_NETWORK:?set CPLANE_NETWORK to the shared network}
```

`cplane` passes the image variable on every compose call, not just `up`. A file
that guards its image with `${IMAGE:?}` would otherwise abort `ps`, `stop`, and
`pull`. The network variable is passed the same way.

Use `--pull=false` when the image exists only on the local daemon, for example
an image built in the same CI job.

Infrastructure is separate. A database, cache, or proxy is deployed by another
process or another `cplane` invocation, and joins the same external network.
`cplane` only ensures the network exists; it does not start or gate other
services. If the application cannot reach its infrastructure, the readiness gate
fails and the previous image is restored.

## Readiness

Docker's healthcheck is the verdict of record when the compose file declares
one. `starting` counts as booting until the health budget expires. `unhealthy`
fails the deploy. `healthy` settles it.

If the compose file has no healthcheck, pass a probe that runs inside the
container:

```bash
cplane deploy \
  --image ghcr.io/example/app:v1 \
  --service app \
  --probe 'wget -q -O /dev/null http://127.0.0.1:8080/health'
```

The probe is split on whitespace and executed as `docker exec <container>
<probe...>`. It does not use a shell, so an image without `/bin/sh` still works.
A non-zero probe means "not ready yet"; a failure to run the Docker CLI is an
error.

Running the probe inside the container is what lets a CI runner gate a deploy
over a remote Docker context. The runner needs no route to a published port,
and the compose file needs no loopback publish.

When a declared healthcheck is stuck in `starting` past the health budget, the
probe decides, if one is configured. A healthcheck that cannot run at all, for
example because its binary is missing from the image, therefore does not roll
back a serving application forever.

## Networks

`--network` names the shared network. `cplane` inspects it and creates it when
missing, unless `--network-create=false` is set. The resolved name is passed to
compose as `CPLANE_NETWORK`, so the compose file can declare:

```yaml
networks:
  shared:
    external: true
    name: ${CPLANE_NETWORK:?set CPLANE_NETWORK}
```

Without `--network`, no network is created and no `CPLANE_NETWORK` variable is
passed. The compose file can then declare its own network normally. If the
network needs a specific driver, subnet, or labels, create it before the deploy;
`cplane` joins it and leaves it alone.

## Build and test

The Go toolchain is pinned in `mise.toml` and in `go.mod`. Trust the config
once:

```bash
mise trust
mise run check       # go vet and go test -race
mise run hermetic    # build and test an isolated copy
mise run e2e         # Docker deploy and rollback test
mise run build       # bin/cplane for the host
mise run dist        # linux and darwin, amd64 and arm64
```

`mise run e2e` sets `CPLANE_E2E=1`, which the script requires. It builds a tiny
BusyBox HTTP service, deploys it, then deploys an image that never listens and
asserts the previous image is restored with exit status 1. It needs a reachable
Docker daemon and network access for the first pull.

The normal `go test ./...` suite has no Docker dependency.

## Usage

```
cplane deploy --image <ref> --service <name> [flags]
cplane version
```

Every flag has an environment variable:

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--image` | `CPLANE_IMAGE` | required | Image reference to deploy |
| `--project` | `CPLANE_PROJECT` | compose file's directory name | Compose project name |
| `--service` | `CPLANE_SERVICE` | required | Compose service to deploy |
| `--compose-file` | `CPLANE_COMPOSE_FILE` | `compose.yaml` | Compose file |
| `--env-file` | `CPLANE_ENV_FILE` | compose default | Interpolation env file |
| `--image-var` | `CPLANE_IMAGE_VAR` | `IMAGE` | Compose variable holding the image |
| `--network` | `CPLANE_NETWORK` | none | Shared network to join or create |
| `--network-create` | `CPLANE_NETWORK_CREATE` | `true` | Create the network when missing |
| `--pull` | `CPLANE_PULL` | `true` | Pull the image before deploying |
| `--drain` | `CPLANE_DRAIN` | `30s` | Grace period for `docker compose stop` |
| `--health-budget` | `CPLANE_HEALTH_BUDGET` | `60s` | Time a boot may take before it fails |
| `--health-interval` | `CPLANE_HEALTH_INTERVAL` | `2s` | Delay between probe attempts |
| `--health-timeout` | `CPLANE_HEALTH_TIMEOUT` | `3s` | Timeout for one probe attempt |
| `--health-retries` | `CPLANE_HEALTH_RETRIES` | `3` | Probe attempts per pass |
| `--probe` | `CPLANE_PROBE` | none | Command run inside the container |
| `--docker` | `CPLANE_DOCKER` | `docker` | Docker binary |

The defaults are starting points, not measurements. Set the budget from the
application's observed boot time.

## Deploy model

1. Pull the image. A pull failure exits 3 before anything is stopped.
2. Inspect or create the shared network.
3. Read `.Config.Image` from the running container. That is the rollback target.
4. Stop the service, honouring `--drain`.
5. Start the new image with `docker compose up -d --no-deps`.
6. Observe, decide, and wait in a bounded loop.
7. If readiness is lost or the budget expires, start the captured image again
   and exit 1.
8. If any start or compose operation fails after the stop, attempt the same
   restore and exit 4.

The loop is stop-first: the old container stops before the new one starts. Put a
proxy in front of the service if clients must not see the gap.

There is no history file and no host timer. A stored history can name an image
that has been pruned; the running container cannot. A deploy that fails and
restores leaves the host on the previous image, and a later deploy tries again.

## Exit status

| Code | Meaning |
| --- | --- |
| `0` | The deploy settled |
| `1` | The deploy failed; the previous image was restored, or none was captured |
| `2` | Bad usage or an unreadable configuration |
| `3` | The image pull failed; nothing was stopped |
| `4` | A Docker or Docker Compose operation failed |

The log carries the phase, action, and reason for every pass, so a workflow can
report why a deploy failed without guessing from the exit code alone.

## Release

A release is a bare tag and GoReleaser:

```bash
git tag v0.1.0
git push origin v0.1.0
```

`.github/workflows/release.yml` publishes static binaries for linux and darwin
on amd64 and arm64, named `cplane_<version>_<os>_<arch>`, with a
`checksums.txt`. `.github/workflows/ci.yml` runs vet, the race detector,
`goreleaser check`, the hermetic copy, and the Docker end-to-end test on every
pull request.

A released binary reports its own provenance:

```bash
cplane version
# cplane 0.1.0
# commit 0123456789abcdef0123456789abcdef01234567
# built  2026-11-01T12:00:00Z
```

Validate the release config locally with `goreleaser check`. Produce the
artifacts without publishing with:

```bash
goreleaser release --snapshot --clean --skip=publish,announce,validate
```

## Hermetic check

`scripts/hermetic.sh` copies the module to a temporary directory and runs
`go build ./...` and `go test ./...` there. It fails if a module file references
a parent path or a sibling directory, or if a vendor-specific name remains. The
copy is the check that matters: if the module needs a file outside its own
directory, the copy fails before a new repository does.

## Layout

- `decide.go` is the whole branch logic: a pure function from a deployment plus
  an observation to one `Action`. No Docker, no clock, no environment.
- `observe.go` classifies the running container into a `Phase`. The enum makes
  the reconcile cases an exhaustive switch instead of a chain of conditionals.
- `reconcile.go` drives observe, decide, apply, and the in-run restore, bounded
  by the health budget and interruptible through a context.
- `exec_runtime.go` is the Docker seam, shelling out to `docker` and
  `docker compose`. `fake_runtime.go` is the in-memory equivalent used by tests.
- `cmd/cplane/main.go` owns flags, environment variables, and exit codes.

## Extraction notes

This module was generalized from an earlier single-service deployment gate. The
extraction changed:

- The module path and package name to this repository.
- The state file, YAML dependency, transition log, and history were removed.
- `init`, `logs`, `status`, `rollback`, and `reconcile` were removed. A rollback
  is a deploy of an older tag.
- Deployment values moved from constants to flags and environment variables.
- Readiness moved from a host HTTP request to a probe inside the container.
- Image pull and network creation were added to the deploy.
- The release workflow, tag scheme, and GoReleaser target belong to this
  repository.
