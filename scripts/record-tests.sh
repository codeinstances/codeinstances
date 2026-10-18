#!/bin/sh
# Copyright CodeInstances.com 2026
# SPDX-License-Identifier: Apache-2.0
#
# Runs every test, the race detector, the release plan's cases, the
# self-checking demo and the smoke check on release-style binaries, and writes
# the output to test/results/ with a note of the machine they ran on. The
# numbers in the README and on codeinstances.com come from these files.
#
#   sh scripts/record-tests.sh
#
# Needs Go and Python 3.
set -eu
cd "$(dirname "$0")/.."
out=test/results
mkdir -p "$out"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Whether the checkout was clean before anything was recorded.
state="at $(git rev-parse --short HEAD 2>/dev/null || echo 'no commit')$(test -z "$(git status --porcelain 2>/dev/null)" || echo ' plus uncommitted changes')"

header() {
	echo "# $1"
	echo "# recorded $(date -u '+%Y-%m-%d %H:%M UTC') $state, CodeInstances $(sed -n 's/^const Version = "\(.*\)"$/\1/p' codeinstances.go), $(go version | cut -d' ' -f3-)"
	cpu=$(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //' || true)
	test -n "$cpu" || cpu=$(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo 'unknown CPU')
	echo "# $(uname -sr), $cpu, $(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo '?') CPUs"
	echo "# command: $2"
	echo
}

go vet ./...

cmd="go test -count=1 -v ./..."
{ header "Every test, verbose" "$cmd"; $cmd 2>&1; } > "$out/tests.txt"

cmd="go test -race -count=1 ./..."
{ header "Every test with the race detector" "$cmd"; $cmd 2>&1; } > "$out/race.txt"

cmd="python3 -B -m unittest discover -s scripts -p *_test.py -v"
{ header "The release plan's cases" "$cmd"; $cmd 2>&1; } > "$out/release-plan.txt"

# Release-style binaries: no cgo, stripped, as the release workflow builds them.
mkdir "$work/bin"
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$work/bin/codeinstances" ./cmd/codeinstances
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$work/bin/codeinstances-demo" ./examples/demo

# The demo prints progress on stderr and a JSON report on stdout, and fails
# on any failed check. The report carries its own environment and timings.
"$work/bin/codeinstances-demo" -cli "$work/bin/codeinstances" -replays 10 > "$out/demo.json" 2>"$work/demo.log" || { cat "$work/demo.log" >&2; exit 1; }

cmd="python3 scripts/smoke.py BIN"
{ header "The release smoke check on binaries built as the release workflow builds them" "$cmd"; python3 scripts/smoke.py "$work/bin" 2>&1; } > "$out/smoke.txt"

grep -E '^(ok|FAIL|--- FAIL)' "$out/tests.txt" "$out/race.txt" || true
tail -n 1 "$out/release-plan.txt" "$out/smoke.txt"
echo "results written to $out/"
