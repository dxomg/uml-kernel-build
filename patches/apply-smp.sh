#!/usr/bin/env bash
# Apply UML SMP support to the kernel source tree in $PWD.
#
# Strategy:
#   - Kernel >= 6.19 has native UML SMP (since commit 1e4ee5135d81).
#     Nothing to patch; we just enable CONFIG_SMP via the config fragment.
#   - Kernel 6.18.x gets the full SMP series backported. The series was
#     written on top of 6.18-rc3, so it backports cleanly; we apply the
#     cumulative patch with `git apply --3way` (falls back to fuzz) so it
#     survives minor 6.18.x context drift as stable backports accumulate.
#   - Kernel <= 6.17 / 6.12 LTS: the series base is too far away to port
#     safely. SMP stays off (single-CPU, as upstream).
#
# IMPORTANT: never reset the whole working tree on failure. Other bundled
# patches (memfd, memdrop, ...) may already be applied; a blanket
# `git checkout -- .` would silently drop them.
#
# Usage: apply-smp.sh <patches-dir>  (run from inside the kernel source tree)
set -euo pipefail

PATCH_DIR="${1:?patches dir required}"
KVER_FILE="Makefile"

# Parse VERSION/PATCHLEVEL/SUBLEVEL from the kernel Makefile
read_kver() {
    local v p
    v=$(sed -n 's/^VERSION = //p' "$KVER_FILE")
    p=$(sed -n 's/^PATCHLEVEL = //p' "$KVER_FILE")
    echo "${v}.${p}"
}

KVER=$(read_kver)
KMAJOR="${KVER%%.*}"
KMINOR="${KVER#*.}"

echo "[smp] kernel series: ${KVER}"

# 6.19+ -> native SMP, nothing to backport
if [ "$KMAJOR" -gt 6 ] || { [ "$KMAJOR" -eq 6 ] && [ "$KMINOR" -ge 19 ]; }; then
    echo "[smp] >= 6.19: native UML SMP available, no source patch needed"
    exit 0
fi

# 6.18.x -> backport the series
if [ "$KMAJOR" -eq 6 ] && [ "$KMINOR" -eq 18 ]; then
    CUMUL="${PATCH_DIR}/smp-backport/uml-smp-backport-6.18.patch"
    if [ ! -f "$CUMUL" ]; then
        echo "[smp] WARN: $CUMUL not found, skipping SMP backport"
        exit 0
    fi
    echo "[smp] 6.18.x: applying SMP backport series"
    # Prefer 3-way merge (uses base blobs from git history).
    if git apply --3way --index "$CUMUL" 2>/tmp/smp-apply.err; then
        echo "[smp] applied cleanly via 3-way merge"
        exit 0
    fi

    echo "[smp] 3-way had issues, trying git apply without 3-way"
    if git apply --reject --whitespace=nowarn "$CUMUL" 2>/tmp/smp-apply1.err; then
        echo "[smp] applied via git apply"
        exit 0
    fi

    echo "[smp] plain git apply failed, trying patch(1) --fuzz=3 on current tree"
    # Do NOT reset the tree — preserve already-applied bundled patches.
    if patch -p1 --fuzz=3 --forward --no-backup-if-mismatch < "$CUMUL" \
            >/tmp/smp-apply2.log 2>&1; then
        echo "[smp] applied with fuzz"
        # Clean up reject files if any empty leftovers
        find . -name '*.rej' -size 0 -delete 2>/dev/null || true
        if find . -name '*.rej' | grep -q .; then
            echo "[smp] WARN: reject files remain:" >&2
            find . -name '*.rej' >&2
        fi
        exit 0
    fi

    echo "[smp] FAILED to apply backport:" >&2
    tail -40 /tmp/smp-apply2.log >&2 || true
    tail -40 /tmp/smp-apply.err >&2 || true
    exit 1
fi

# Older (6.12 LTS, etc.) -> not supported, stay UP
echo "[smp] ${KVER}: SMP backport not supported for this series (needs >= 6.18), staying UP"
exit 0
