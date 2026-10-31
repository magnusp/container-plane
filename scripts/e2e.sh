#!/usr/bin/env bash
# End-to-end deploy and rollback test. It needs a reachable Docker daemon and
# network access for the first image pull. The normal unit suite does not run
# it; set CPLANE_E2E=1 to opt in.
set -euo pipefail

if [[ "${CPLANE_E2E:-}" != "1" ]]; then
  echo "e2e: set CPLANE_E2E=1 to run the Docker end-to-end test"
  exit 0
fi

command -v docker >/dev/null || {
  echo "e2e: docker is required" >&2
  exit 1
}
docker info >/dev/null 2>&1 || {
  echo "e2e: the Docker daemon is not reachable" >&2
  exit 1
}

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
project="cplane-e2e-$$"
network="${project}-net"
healthy_image="busybox:1.36.1"
bad_image="${project}-bad:local"

cleanup() {
  IMAGE="$healthy_image" CPLANE_NETWORK="$network" \
    docker compose -p "$project" -f "$tmp/compose.yaml" down -v >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  docker image rm -f "$bad_image" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

cat > "$tmp/compose.yaml" <<'YAML'
services:
  app:
    image: ${IMAGE:?set IMAGE}
    command: ["sh", "-c", "mkdir -p /www && echo ok > /www/index.html && httpd -f -p 8080 -h /www"]
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/"]
      interval: 1s
      timeout: 1s
      retries: 3
      start_period: 2s
    networks:
      - shared
networks:
  shared:
    external: true
    name: ${CPLANE_NETWORK:?set CPLANE_NETWORK}
YAML

# An image that starts and stays running but never listens on the health port.
cat > "$tmp/Dockerfile.bad" <<'DOCKER'
FROM busybox:1.36.1
ENTRYPOINT ["sh", "-c", "sleep 3600"]
DOCKER

cd "$root"
go build -o "$tmp/cplane" ./cmd/cplane
docker build -q -t "$bad_image" -f "$tmp/Dockerfile.bad" "$tmp" >/dev/null

deploy() {
  "$tmp/cplane" deploy \
    --project "$project" \
    --service app \
    --compose-file "$tmp/compose.yaml" \
    --image "$1" \
    --network "$network" \
    --drain 1s \
    --health-budget "${2:-30s}" \
    "${@:3}"
}

# First deploy: create the network, pull the image, and settle healthy.
deploy "$healthy_image"

# Second deploy: an image that never listens. It must fail the health budget,
# restore the previous image, and exit 1.
set +e
deploy "$bad_image" 5s --pull=false
status=$?
set -e

if [[ "$status" -ne 1 ]]; then
  echo "e2e: expected exit 1 after rollback, got $status" >&2
  exit 1
fi

running=$(IMAGE="$healthy_image" CPLANE_NETWORK="$network" \
  docker compose -p "$project" -f "$tmp/compose.yaml" ps -q app | head -n1)
if [[ -z "$running" ]]; then
  echo "e2e: no container is running after the rollback" >&2
  exit 1
fi

actual=$(docker inspect --format '{{.Config.Image}}' "$running")
if [[ "$actual" != "$healthy_image" ]]; then
  echo "e2e: running $actual after rollback, want $healthy_image" >&2
  exit 1
fi

echo "e2e: passed"
