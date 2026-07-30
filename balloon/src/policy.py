"""
UML auto-balloon policy.

Mirrors the decision logic that will live in arch/um/drivers/uml_balloon.c.
Pure functions only — no I/O — so every branch is unit-testable.

Definitions
-----------
boot_cap    : immutable hard cap from boot ``mem=``
ballooned   : bytes currently unplugged (returned to host)
allocated   : boot_cap - ballooned   (still plugged into guest)
available   : si_mem_available() equivalent
usage       : allocated - available
slack       : allocated - usage  (== available)

Reclaim when slack >= high_slack → target allocated = usage + reserve_slack
Restore when slack <= low_slack  → target allocated = usage + reserve_slack
Never allocate above boot_cap; never go below min_allocated.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Literal, Optional

PAGE_SIZE = 4096
MiB = 1024 * 1024

ActionKind = Literal["none", "unplug", "plug"]


@dataclass(frozen=True)
class BalloonConfig:
    """Tunables (sysfs knobs)."""

    high_slack_bytes: int = 250 * MiB
    reserve_slack_bytes: int = 100 * MiB
    low_slack_bytes: int = 32 * MiB
    min_allocated_bytes: int = 64 * MiB
    step_bytes: int = 4 * MiB
    enabled: bool = True

    def validate(self) -> None:
        """Raise ValueError if config is internally inconsistent."""
        if self.high_slack_bytes <= 0:
            raise ValueError("high_slack_bytes must be > 0")
        if self.reserve_slack_bytes <= 0:
            raise ValueError("reserve_slack_bytes must be > 0")
        if self.low_slack_bytes < 0:
            raise ValueError("low_slack_bytes must be >= 0")
        if self.min_allocated_bytes <= 0:
            raise ValueError("min_allocated_bytes must be > 0")
        if self.step_bytes < PAGE_SIZE or self.step_bytes % PAGE_SIZE != 0:
            raise ValueError("step_bytes must be a positive multiple of PAGE_SIZE")
        if not (
            self.high_slack_bytes > self.reserve_slack_bytes > self.low_slack_bytes
        ):
            raise ValueError(
                "require high_slack > reserve_slack > low_slack for hysteresis"
            )
        if self.min_allocated_bytes < PAGE_SIZE:
            raise ValueError("min_allocated_bytes must be >= PAGE_SIZE")


@dataclass(frozen=True)
class BalloonState:
    """Observed guest memory state at a policy tick."""

    boot_cap_bytes: int
    ballooned_bytes: int
    available_bytes: int  # si_mem_available()

    def validate(self) -> None:
        if self.boot_cap_bytes < PAGE_SIZE:
            raise ValueError("boot_cap_bytes must be >= PAGE_SIZE")
        if self.boot_cap_bytes % PAGE_SIZE != 0:
            raise ValueError("boot_cap_bytes must be page-aligned")
        if self.ballooned_bytes < 0:
            raise ValueError("ballooned_bytes must be >= 0")
        if self.ballooned_bytes > self.boot_cap_bytes:
            raise ValueError("ballooned_bytes cannot exceed boot_cap_bytes")
        if self.ballooned_bytes % PAGE_SIZE != 0:
            raise ValueError("ballooned_bytes must be page-aligned")
        if self.available_bytes < 0:
            raise ValueError("available_bytes must be >= 0")


@dataclass(frozen=True)
class BalloonAction:
    """Decision for one policy tick."""

    kind: ActionKind
    bytes: int  # magnitude; 0 iff kind == "none"
    target_allocated_bytes: int
    reason: str


def pages_to_bytes(pages: int) -> int:
    if pages < 0:
        raise ValueError("pages must be >= 0")
    return pages * PAGE_SIZE


def bytes_to_pages(n_bytes: int) -> int:
    """Floor to whole pages. Negative input rejected."""
    if n_bytes < 0:
        raise ValueError("bytes must be >= 0")
    return n_bytes // PAGE_SIZE


def compute_allocated(boot_cap_bytes: int, ballooned_bytes: int) -> int:
    """Bytes still plugged into the guest."""
    if ballooned_bytes < 0 or boot_cap_bytes < 0:
        raise ValueError("sizes must be >= 0")
    if ballooned_bytes > boot_cap_bytes:
        raise ValueError("ballooned exceeds boot_cap")
    return boot_cap_bytes - ballooned_bytes


def compute_usage(allocated_bytes: int, available_bytes: int) -> int:
    """
    Actively used / non-reclaimable portion of allocated memory.

    usage = allocated - available.  Available is clamped to allocated so a
    transient accounting glitch cannot produce negative usage.
    """
    if allocated_bytes < 0 or available_bytes < 0:
        raise ValueError("sizes must be >= 0")
    avail = min(available_bytes, allocated_bytes)
    return allocated_bytes - avail


def compute_slack(allocated_bytes: int, usage_bytes: int) -> int:
    """Headroom inside current allocation. Equals clamped available."""
    if allocated_bytes < 0 or usage_bytes < 0:
        raise ValueError("sizes must be >= 0")
    if usage_bytes > allocated_bytes:
        raise ValueError("usage cannot exceed allocated")
    return allocated_bytes - usage_bytes


def _align_down(n: int, step: int) -> int:
    return (n // step) * step


def _clamp(n: int, lo: int, hi: int) -> int:
    return max(lo, min(n, hi))


def decide_action(cfg: BalloonConfig, st: BalloonState) -> BalloonAction:
    """
    Pure policy decision.

    Returns an action whose ``bytes`` is a multiple of ``step_bytes``
    (and of PAGE_SIZE), or kind ``none``.
    """
    cfg.validate()
    st.validate()

    allocated = compute_allocated(st.boot_cap_bytes, st.ballooned_bytes)
    usage = compute_usage(allocated, st.available_bytes)
    slack = compute_slack(allocated, usage)

    if not cfg.enabled:
        return BalloonAction("none", 0, allocated, "disabled")

    # --- reclaim (unplug / return to host) ---
    if slack >= cfg.high_slack_bytes:
        target = usage + cfg.reserve_slack_bytes
        target = _clamp(target, cfg.min_allocated_bytes, st.boot_cap_bytes)
        # never try to grow via reclaim path
        if target >= allocated:
            return BalloonAction(
                "none", 0, allocated,
                "reclaim_skipped_target_ge_allocated",
            )
        drop = allocated - target
        drop = _align_down(drop, cfg.step_bytes)
        if drop < cfg.step_bytes:
            return BalloonAction(
                "none", 0, allocated,
                "reclaim_below_step",
            )
        # do not unplug more than currently allocated above floor
        max_drop = allocated - cfg.min_allocated_bytes
        if max_drop < 0:
            max_drop = 0
        drop = min(drop, _align_down(max_drop, cfg.step_bytes))
        if drop < cfg.step_bytes:
            return BalloonAction(
                "none", 0, allocated,
                "reclaim_floor_hit",
            )
        return BalloonAction(
            "unplug", drop, allocated - drop,
            "reclaim_high_slack",
        )

    # --- restore (plug / take back from balloon) ---
    if slack <= cfg.low_slack_bytes and st.ballooned_bytes > 0:
        target = usage + cfg.reserve_slack_bytes
        target = _clamp(target, cfg.min_allocated_bytes, st.boot_cap_bytes)
        if target <= allocated:
            return BalloonAction(
                "none", 0, allocated,
                "restore_skipped_target_le_allocated",
            )
        need = target - allocated
        need = _align_down(need, cfg.step_bytes)
        need = min(need, st.ballooned_bytes)
        need = _align_down(need, cfg.step_bytes)
        if need < cfg.step_bytes:
            return BalloonAction(
                "none", 0, allocated,
                "restore_below_step",
            )
        return BalloonAction(
            "plug", need, allocated + need,
            "restore_low_slack",
        )

    return BalloonAction("none", 0, allocated, "within_hysteresis")


def apply_action(st: BalloonState, action: BalloonAction) -> BalloonState:
    """
    Return the state after applying ``action`` (idealised; assumes full success).

    available_bytes is adjusted so that *usage* stays constant across the
    plug/unplug (the workload did not change — only headroom did).
    """
    st.validate()
    if action.kind == "none":
        return st
    if action.bytes <= 0:
        raise ValueError("action.bytes must be > 0 for plug/unplug")
    if action.bytes % PAGE_SIZE != 0:
        raise ValueError("action.bytes must be page-aligned")

    allocated = compute_allocated(st.boot_cap_bytes, st.ballooned_bytes)
    usage = compute_usage(allocated, st.available_bytes)

    if action.kind == "unplug":
        if action.bytes > allocated:
            raise ValueError("cannot unplug more than allocated")
        new_ballooned = st.ballooned_bytes + action.bytes
        new_allocated = st.boot_cap_bytes - new_ballooned
    elif action.kind == "plug":
        if action.bytes > st.ballooned_bytes:
            raise ValueError("cannot plug more than ballooned")
        new_ballooned = st.ballooned_bytes - action.bytes
        new_allocated = st.boot_cap_bytes - new_ballooned
    else:
        raise ValueError(f"unknown action kind: {action.kind}")

    # Keep usage constant; available absorbs the allocation change.
    new_available = max(0, new_allocated - usage)
    return BalloonState(
        boot_cap_bytes=st.boot_cap_bytes,
        ballooned_bytes=new_ballooned,
        available_bytes=new_available,
    )
