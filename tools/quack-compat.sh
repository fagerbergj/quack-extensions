#!/usr/bin/env bash
# Proves this checkout's modules still build, test and validate inside quack, before anything is tagged.
# Usage: tools/quack-compat.sh [quack-dir]   (no arg: shallow-clones fagerbergj/quack at $QUACK_REF, default main)
set -euo pipefail

ext=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
step=setup
infra=
on_exit() {
	local rc=$?
	rm -rf "$tmp"
	[ "$rc" -eq 0 ] && return
	if [ -n "$infra" ]; then
		echo "::error::quack-compat could not run \"$step\" (network, or a bad QUACK_REF/quack dir). This is not a verdict on the change; rerun." >&2
	else
		echo "::error::quack-compat failed at \"$step\" against quack ${quack_desc:-?}: this change breaks quack. Fix the module, or if quack must change with it, see \"Checking a change against quack\" in README.md." >&2
	fi
}
trap on_exit EXIT
run() { step=$*; echo "== $step"; "$@"; }

quack=${1:-$tmp/quack}
infra=1
[ -n "${1:-}" ] || run git clone -q --depth 1 --branch "${QUACK_REF:-main}" https://github.com/fagerbergj/quack "$quack"
cd "$quack"
quack_desc="$(git rev-parse --abbrev-ref HEAD)@$(git rev-parse --short HEAD) ($PWD)"
echo "== quack: $quack_desc"

# GOWORK outside quack's tree: that checkout's own go commands stay on its go.mod pins.
export GOWORK=$tmp/go.work
mods=("$ext"/*/go.mod)
run go work init . "${mods[@]%/go.mod}"
run go mod download
infra=
run go build ./...
# vet type-checks every quack test file against these modules.
run go vet ./...

step="list quack packages depending on quack-extensions"
pkgs=$(go list -f '{{.ImportPath}} {{join .Deps " "}} {{join .TestImports " "}} {{join .XTestImports " "}}' ./... | awk '/quack-extensions\//{print $1}')
[ -n "$pkgs" ] || { echo "no quack package depends on quack-extensions; the go.work is wrong" >&2; exit 1; }
# shellcheck disable=SC2086 # one import path per word
run go test -count=1 $pkgs

run go build -o "$tmp/quack-bin" ./cmd/quack
# Placeholder env for quack's shipped config, read as KEY=VALUE data rather than sourced as shell.
while IFS='=' read -r k v; do
	if [[ $k =~ ^[A-Z_][A-Z0-9_]*$ ]]; then export "$k=$v"; fi
done <.env.example
export QUACK_MEDIA_MODEL=${QUACK_MEDIA_MODEL:-qwen3-omni-30b} QUACK_IMAGE_MODEL=${QUACK_IMAGE_MODEL:-qwen3-vl-32b}
run "$tmp/quack-bin" server validate config/quack.yaml

export QUACK_COMPAT_TMP=$tmp QUACK_COMPAT_GITHUB_KEY=$tmp/github.pem QUACK_COMPAT_SECRET=placeholder
run openssl genrsa -out "$QUACK_COMPAT_GITHUB_KEY" 2048
step="quack server validate tools/quack-compat.config.yaml"
echo "== $step"
accepted=$("$tmp/quack-bin" server validate --json "$ext/tools/quack-compat.config.yaml" | jq -r '.extensions[]?')
echo "accepted: ${accepted//$'\n'/ }"
# Every extension module needs a fixture block, or its Factory goes unchecked.
for m in "${mods[@]%/go.mod}"; do
	name=${m##*/}
	[ "$name" = sdk ] && continue
	step="fixture covers $name"
	grep -qx "$name" <<<"$accepted" || { echo "$name: no accepted extensions.$name block in tools/quack-compat.config.yaml" >&2; exit 1; }
done
echo "quack-compat: OK against quack $quack_desc"
