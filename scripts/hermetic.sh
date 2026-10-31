#!/usr/bin/env bash
# Prove the module can stand alone: copy it somewhere empty, then build and
# test the copy with no other repository file present.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

tar -C "$root" \
  --exclude=.git \
  --exclude=bin \
  --exclude=dist \
  -cf - . | tar -C "$tmp" -xf -
cd "$tmp"

# A module file must not reach outside the module. The extraction target is a
# repository containing only this module, so a parent path or a sibling
# directory is a build that would fail after the move.
if grep -RInE '\.\./|(^|[^[:alnum:]_])(deployment|core|frontend|docs)/' \
  --include='*.go' --include='go.mod' --include='go.sum' --include='*.toml' .; then
  echo "hermetic: a module file references a path outside the module" >&2
  exit 1
fi

# The tool must not carry vendor-specific names from its origin. This check
# also catches a stale example or release target.
if grep -RIniE 'retellify|mediaboken' \
  --exclude-dir=.git --exclude='hermetic.sh' .; then
  echo "hermetic: a vendor-specific reference remains" >&2
  exit 1
fi

go build ./...
go test ./...

echo "hermetic: the module builds and tests in isolation"
