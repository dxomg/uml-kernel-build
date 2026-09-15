#!/bin/sh
# gVisor's module zips intermittently ship pkg/tcpip/stack/bridge_test.go
# declaring `package bridge_test` — illegal next to the directory's
# `package stack`, so any `go build`/`go vet` that loads pkg/tcpip/stack
# dies with "found packages stack and bridge". gVisor builds with Bazel
# and never notices. The file is a test file: dropping it from the local
# module cache fixes every consumer without touching go.sum (the zip
# hash is only verified at download time).
#
# Run after any `go get`/`go mod download` that may pull a fresh gVisor
# (the CI float step, `go mod tidy` after a bump, ...). Idempotent: a
# cache without the stray file is a no-op.
set -e

command -v go >/dev/null 2>&1 || exit 0
cache="$(go env GOMODCACHE 2>/dev/null)"
[ -n "$cache" ] || exit 0

removed=0
for d in "$cache"/gvisor.dev/gvisor@*; do
    [ -d "$d" ] || continue
    f="$d/pkg/tcpip/stack/bridge_test.go"
    if [ -f "$f" ]; then
        # Cache entries are read-only; unlink needs a writable directory.
        chmod u+w "$(dirname "$f")" 2>/dev/null || true
        rm -f "$f"
        echo "removed stray $f"
        removed=$((removed + 1))
    fi
done

[ "$removed" -eq 0 ] && echo "no stray gvisor bridge_test.go in cache — nothing to do"
exit 0
