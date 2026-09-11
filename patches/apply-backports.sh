#!/usr/bin/env bash
# Apply backported generic UML fixes cherry-picked from the
# linux-um-arm64 series (https://github.com/zalexdev/linux-um-arm64,
# branch um-arm64, base Linux 7.2-rc4 of the uml tree).
#
# The series targets a much newer base, so each fix is applied
# best-effort: `git apply --3way`, and a patch whose context has drifted
# too far is skipped with a ::warning instead of failing the build.
# None of these fixes are critical — unlike the bundled patches, losing
# one on a future 6.18.x sublevel must not stop the build.
#
# IMPORTANT: run AFTER apply-smp.sh and the bundled patches loop.
# um-backport-04 (stub reaper) builds on the SMP backport's threading
# changes in os-Linux/skas/process.c.
#
# Usage: apply-backports.sh <patches-dir>  (run from inside the kernel tree)
set -uo pipefail

PATCH_DIR="${1:?patches dir required}"
KVER_FILE="Makefile"
LOG=/tmp/backport-apply.err

read_kver() {
    local v p
    v=$(sed -n 's/^VERSION = //p' "$KVER_FILE")
    p=$(sed -n 's/^PATCHLEVEL = //p' "$KVER_FILE")
    echo "${v}.${p}"
}
KVER=$(read_kver)

shopt -s nullglob
patches=("$PATCH_DIR"/um-backport/*.patch)
if [ ${#patches[@]} -eq 0 ]; then
    echo "[backport] no patches in $PATCH_DIR/um-backport, nothing to do"
    exit 0
fi

ok=0; skip=0
for p in "${patches[@]}"; do
    name=$(basename "$p")
    if git apply --check --3way "$p" 2>"$LOG" && git apply --3way "$p" 2>>"$LOG"; then
        echo "[backport] applied $name"
        ok=$((ok + 1))
    else
        echo "::warning::backport $name did not apply on ${KVER}, skipping"
        echo "[backport] $name FAILED:" >&2
        tail -5 "$LOG" >&2 || true
        skip=$((skip + 1))
    fi
done

echo "[backport] ${KVER}: applied=${ok} skipped=${skip}"
[ "$skip" -eq 0 ] || echo "[backport] note: skipped fixes may need a rebase against this sublevel"
exit 0
