#!/usr/bin/env bash
# Proves this checkout's modules still build, test and validate inside quack, before anything is tagged.
# Usage: tools/quack-compat.sh [quack-dir]   (no arg: shallow-clones fagerbergj/quack at $QUACK_REF, default main)
set -euo pipefail

ext=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
step=setup
trap '[ $? -eq 0 ] || echo "::error::quack-compat failed at \"$step\" against quack ${QUACK_REF:-main}: this change breaks quack. Fix the module, or if quack must change with it, rerun with QUACK_REF=<quack branch>." >&2; rm -rf "$tmp"' EXIT
run() { step=$*; echo "== $step"; "$@"; }

quack=${1:-$tmp/quack}
[ -n "${1:-}" ] || run git clone -q --depth 1 --branch "${QUACK_REF:-main}" https://github.com/fagerbergj/quack "$quack"
cd "$quack"

# GOWORK outside quack's tree: that checkout's own go commands stay on its go.mod pins.
export GOWORK=$tmp/go.work
mods=("$ext"/*/go.mod)
run go work init . "${mods[@]%/go.mod}"
run go build ./...
# vet type-checks every quack test file against these modules; the tests run only the packages that import them.
run go vet ./...
pkgs=$(go list -f '{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}' ./... | awk '/quack-extensions\//{print $1}')
run go test -count=1 $pkgs

run go build -o "$tmp/quack-bin" ./cmd/quack
# Placeholders only: validate parses and cross-checks config, it never dials these.
set -a
. ./.env.example
QUACK_MEDIA_MODEL=${QUACK_MEDIA_MODEL:-qwen3-omni-30b}
QUACK_IMAGE_MODEL=${QUACK_IMAGE_MODEL:-qwen3-vl-32b}
set +a
run "$tmp/quack-bin" server validate config/quack.yaml
echo "quack-compat: OK against quack ${QUACK_REF:-main}"
