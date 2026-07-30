"""Unit tests for EVERY function in balloon.src.policy."""

from __future__ import annotations

import pytest

from balloon.src.policy import (
    PAGE_SIZE,
    MiB,
    BalloonAction,
    BalloonConfig,
    BalloonState,
    apply_action,
    bytes_to_pages,
    compute_allocated,
    compute_slack,
    compute_usage,
    decide_action,
    pages_to_bytes,
)


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

def cfg(**kwargs) -> BalloonConfig:
    return BalloonConfig(**kwargs)


def st(boot_cap, ballooned, available) -> BalloonState:
    return BalloonState(
        boot_cap_bytes=boot_cap,
        ballooned_bytes=ballooned,
        available_bytes=available,
    )


DEFAULT = cfg()  # 250 / 100 / 32 / 64 / 4 MiB steps


# ===========================================================================
# pages_to_bytes / bytes_to_pages
# ===========================================================================

class TestPagesBytes:
    def test_pages_to_bytes_zero(self):
        assert pages_to_bytes(0) == 0

    def test_pages_to_bytes_one(self):
        assert pages_to_bytes(1) == PAGE_SIZE

    def test_pages_to_bytes_many(self):
        assert pages_to_bytes(256) == 256 * PAGE_SIZE

    def test_pages_to_bytes_negative(self):
        with pytest.raises(ValueError):
            pages_to_bytes(-1)

    def test_bytes_to_pages_exact(self):
        assert bytes_to_pages(8 * PAGE_SIZE) == 8

    def test_bytes_to_pages_floors(self):
        assert bytes_to_pages(8 * PAGE_SIZE + 100) == 8

    def test_bytes_to_pages_zero(self):
        assert bytes_to_pages(0) == 0

    def test_bytes_to_pages_less_than_page(self):
        assert bytes_to_pages(PAGE_SIZE - 1) == 0

    def test_bytes_to_pages_negative(self):
        with pytest.raises(ValueError):
            bytes_to_pages(-PAGE_SIZE)

    def test_roundtrip(self):
        for p in (0, 1, 17, 1024):
            assert bytes_to_pages(pages_to_bytes(p)) == p


# ===========================================================================
# compute_allocated
# ===========================================================================

class TestComputeAllocated:
    def test_no_balloon(self):
        assert compute_allocated(512 * MiB, 0) == 512 * MiB

    def test_partial(self):
        assert compute_allocated(512 * MiB, 200 * MiB) == 312 * MiB

    def test_full_balloon(self):
        assert compute_allocated(512 * MiB, 512 * MiB) == 0

    def test_balloon_exceeds(self):
        with pytest.raises(ValueError):
            compute_allocated(100, 101)

    def test_negative_boot(self):
        with pytest.raises(ValueError):
            compute_allocated(-1, 0)

    def test_negative_ballooned(self):
        with pytest.raises(ValueError):
            compute_allocated(100, -1)


# ===========================================================================
# compute_usage
# ===========================================================================

class TestComputeUsage:
    def test_basic(self):
        # allocated 500, available 100 → usage 400
        assert compute_usage(500 * MiB, 100 * MiB) == 400 * MiB

    def test_all_available(self):
        assert compute_usage(500 * MiB, 500 * MiB) == 0

    def test_none_available(self):
        assert compute_usage(500 * MiB, 0) == 500 * MiB

    def test_available_clamped_above_allocated(self):
        # accounting glitch: available > allocated → usage 0
        assert compute_usage(100 * MiB, 200 * MiB) == 0

    def test_negative_allocated(self):
        with pytest.raises(ValueError):
            compute_usage(-1, 0)

    def test_negative_available(self):
        with pytest.raises(ValueError):
            compute_usage(100, -1)


# ===========================================================================
# compute_slack
# ===========================================================================

class TestComputeSlack:
    def test_basic(self):
        assert compute_slack(500 * MiB, 400 * MiB) == 100 * MiB

    def test_zero_usage(self):
        assert compute_slack(500 * MiB, 0) == 500 * MiB

    def test_full_usage(self):
        assert compute_slack(500 * MiB, 500 * MiB) == 0

    def test_usage_exceeds(self):
        with pytest.raises(ValueError):
            compute_slack(100, 101)

    def test_negative(self):
        with pytest.raises(ValueError):
            compute_slack(-1, 0)
        with pytest.raises(ValueError):
            compute_slack(100, -1)

    def test_slack_equals_clamped_available(self):
        allocated = 512 * MiB
        available = 180 * MiB
        usage = compute_usage(allocated, available)
        assert compute_slack(allocated, usage) == available


# ===========================================================================
# BalloonConfig.validate
# ===========================================================================

class TestConfigValidate:
    def test_defaults_ok(self):
        DEFAULT.validate()  # must not raise

    def test_high_not_greater_than_reserve(self):
        with pytest.raises(ValueError, match="hysteresis"):
            cfg(high_slack_bytes=100 * MiB, reserve_slack_bytes=100 * MiB).validate()

    def test_reserve_not_greater_than_low(self):
        with pytest.raises(ValueError, match="hysteresis"):
            cfg(reserve_slack_bytes=32 * MiB, low_slack_bytes=32 * MiB).validate()

    def test_inverted_hysteresis(self):
        with pytest.raises(ValueError, match="hysteresis"):
            cfg(
                high_slack_bytes=50 * MiB,
                reserve_slack_bytes=100 * MiB,
                low_slack_bytes=10 * MiB,
            ).validate()

    def test_zero_high(self):
        with pytest.raises(ValueError, match="high_slack"):
            cfg(high_slack_bytes=0).validate()

    def test_zero_reserve(self):
        with pytest.raises(ValueError, match="reserve_slack"):
            cfg(reserve_slack_bytes=0).validate()

    def test_negative_low(self):
        with pytest.raises(ValueError, match="low_slack"):
            cfg(low_slack_bytes=-1).validate()

    def test_step_not_multiple(self):
        with pytest.raises(ValueError, match="step_bytes"):
            cfg(step_bytes=PAGE_SIZE + 1).validate()

    def test_step_too_small(self):
        with pytest.raises(ValueError, match="step_bytes"):
            cfg(step_bytes=0).validate()

    def test_min_allocated_zero(self):
        with pytest.raises(ValueError, match="min_allocated"):
            cfg(min_allocated_bytes=0).validate()


# ===========================================================================
# BalloonState.validate
# ===========================================================================

class TestStateValidate:
    def test_ok(self):
        st(512 * MiB, 0, 100 * MiB).validate()

    def test_boot_cap_too_small(self):
        with pytest.raises(ValueError, match="boot_cap"):
            st(100, 0, 0).validate()

    def test_boot_cap_unaligned(self):
        with pytest.raises(ValueError, match="page-aligned"):
            st(512 * MiB + 1, 0, 0).validate()

    def test_ballooned_negative(self):
        with pytest.raises(ValueError):
            st(512 * MiB, -PAGE_SIZE, 0).validate()

    def test_ballooned_exceeds(self):
        with pytest.raises(ValueError):
            st(512 * MiB, 512 * MiB + PAGE_SIZE, 0).validate()

    def test_ballooned_unaligned(self):
        with pytest.raises(ValueError, match="page-aligned"):
            st(512 * MiB, 123, 0).validate()

    def test_available_negative(self):
        with pytest.raises(ValueError):
            st(512 * MiB, 0, -1).validate()


# ===========================================================================
# decide_action — disabled / hysteresis
# ===========================================================================

class TestDecideDisabledHysteresis:
    def test_disabled(self):
        c = cfg(enabled=False)
        # huge slack would normally reclaim
        s = st(2 * 1024 * MiB, 0, 1500 * MiB)
        a = decide_action(c, s)
        assert a.kind == "none"
        assert a.reason == "disabled"
        assert a.bytes == 0

    def test_within_hysteresis_no_action(self):
        # allocated=512M, available=80M → slack=80 (between 32 and 250)
        s = st(512 * MiB, 0, 80 * MiB)
        a = decide_action(DEFAULT, s)
        assert a.kind == "none"
        assert a.reason == "within_hysteresis"

    def test_slack_just_below_high(self):
        # slack = 249 MiB < 250 → none
        s = st(512 * MiB, 0, 249 * MiB)
        a = decide_action(DEFAULT, s)
        assert a.kind == "none"


# ===========================================================================
# decide_action — reclaim (unplug)
# ===========================================================================

class TestDecideReclaim:
    def test_idle_large_guest_reclaims_to_reserve(self):
        """
        boot=2G, ballooned=0, available=1.6G
        usage=0.4G, slack=1.6G >= 250M
        target_allocated = 400M + 100M = 500M
        drop = 2G - 500M = 1.5G (aligned to 4M)
        """
        boot = 2048 * MiB
        avail = 1600 * MiB
        s = st(boot, 0, avail)
        a = decide_action(DEFAULT, s)
        assert a.kind == "unplug"
        assert a.reason == "reclaim_high_slack"
        usage = boot - avail  # 448 MiB
        assert usage == 448 * MiB
        expected_target = usage + 100 * MiB  # 548 MiB
        expected_drop = boot - expected_target
        # align down to 4 MiB
        expected_drop = (expected_drop // (4 * MiB)) * (4 * MiB)
        assert a.bytes == expected_drop
        assert a.target_allocated_bytes == boot - expected_drop
        # after action, slack should land near reserve
        new = apply_action(s, a)
        new_alloc = compute_allocated(new.boot_cap_bytes, new.ballooned_bytes)
        new_usage = compute_usage(new_alloc, new.available_bytes)
        new_slack = compute_slack(new_alloc, new_usage)
        # slack roughly reserve (within one step)
        assert abs(new_slack - 100 * MiB) < 4 * MiB

    def test_reclaim_respects_min_allocated(self):
        """usage very small; target would be ~100M but min_allocated=64M still ok.
        Force min_allocated high so floor binds."""
        c = cfg(min_allocated_bytes=400 * MiB)
        # boot=512M, avail=500M → usage=12M, slack=500 >= 250
        # target = 12+100=112 → clamped to min 400
        # drop = 512-400 = 112 → align 4M = 112? 112/4=28 exact
        s = st(512 * MiB, 0, 500 * MiB)
        a = decide_action(c, s)
        assert a.kind == "unplug"
        assert a.target_allocated_bytes >= 400 * MiB
        assert a.target_allocated_bytes == 400 * MiB

    def test_reclaim_floor_hit_when_already_at_min(self):
        c = cfg(min_allocated_bytes=512 * MiB)
        # already at floor, huge slack relative but cannot drop
        s = st(512 * MiB, 0, 400 * MiB)
        a = decide_action(c, s)
        assert a.kind == "none"
        assert a.reason in ("reclaim_floor_hit", "reclaim_below_step",
                            "reclaim_skipped_target_ge_allocated")

    def test_reclaim_step_alignment(self):
        s = st(512 * MiB, 0, 300 * MiB)  # usage=212, slack=300
        a = decide_action(DEFAULT, s)
        assert a.kind == "unplug"
        assert a.bytes % (4 * MiB) == 0
        assert a.bytes % PAGE_SIZE == 0

    def test_reclaim_never_exceeds_boot_cap_inverse(self):
        """target_allocated never > boot_cap (clamp)."""
        s = st(512 * MiB, 0, 400 * MiB)
        a = decide_action(DEFAULT, s)
        assert a.target_allocated_bytes <= 512 * MiB

    def test_user_scenario_250mb_threshold(self):
        """
        Explicit user rule: slack >= 250MB triggers reclaim to 100MB reserve.
        allocated=1G, usage=700MB → slack=300MB → reclaim.
        target_allocated = 700+100 = 800MB; drop=200MB.
        """
        boot = 1024 * MiB
        usage = 700 * MiB
        avail = boot - usage  # 324 MiB
        s = st(boot, 0, avail)
        a = decide_action(DEFAULT, s)
        assert a.kind == "unplug"
        # target 800M, drop 224M? boot - (700+100) = 224M → align 4M = 224M
        assert a.bytes == 224 * MiB
        assert a.target_allocated_bytes == 800 * MiB

    def test_exactly_at_high_slack_triggers(self):
        # slack == 250 MiB exactly
        boot = 500 * MiB
        avail = 250 * MiB  # usage=250, slack=250
        s = st(boot, 0, avail)
        a = decide_action(DEFAULT, s)
        assert a.kind == "unplug"

    def test_below_step_after_align(self):
        # tiny excess over high_slack so drop aligns to 0
        c = cfg(
            high_slack_bytes=250 * MiB,
            reserve_slack_bytes=100 * MiB,
            low_slack_bytes=32 * MiB,
            step_bytes=64 * MiB,  # large step
        )
        # slack just 250M, usage such that drop < 64M
        # allocated=400M, avail=250M → usage=150, target=250, drop=150 → align 64 = 128
        s = st(400 * MiB, 0, 250 * MiB)
        a = decide_action(c, s)
        # drop=150 aligned to 64 = 128, ok unplug
        if a.kind == "unplug":
            assert a.bytes % (64 * MiB) == 0
        else:
            assert a.reason in ("reclaim_below_step", "reclaim_floor_hit")


# ===========================================================================
# decide_action — restore (plug)
# ===========================================================================

class TestDecideRestore:
    def test_restore_when_slack_low(self):
        """
        boot=2G, ballooned=1.5G → allocated=512M
        available=20M → slack=20 <= 32 → plug toward usage+100
        usage=492, target=min(592, 2G)=592, need=80M
        """
        boot = 2048 * MiB
        ballooned = 1536 * MiB
        allocated = boot - ballooned  # 512
        avail = 20 * MiB
        s = st(boot, ballooned, avail)
        a = decide_action(DEFAULT, s)
        assert a.kind == "plug"
        assert a.reason == "restore_low_slack"
        usage = allocated - avail  # 492
        expected_target = usage + 100 * MiB  # 592
        expected_need = expected_target - allocated  # 80
        expected_need = (expected_need // (4 * MiB)) * (4 * MiB)
        assert a.bytes == expected_need
        assert a.bytes <= ballooned

    def test_restore_capped_by_ballooned(self):
        """Only 8M ballooned; need more → plug only 8M (aligned 4M = 8M)."""
        boot = 512 * MiB
        ballooned = 8 * MiB
        allocated = boot - ballooned  # 504
        # slack low: avail=10M
        s = st(boot, ballooned, 10 * MiB)
        a = decide_action(DEFAULT, s)
        assert a.kind == "plug"
        assert a.bytes <= ballooned
        assert a.bytes == 8 * MiB

    def test_restore_capped_by_boot_cap(self):
        """target would exceed boot_cap → clamp."""
        boot = 512 * MiB
        ballooned = 100 * MiB
        allocated = 412 * MiB
        # usage high: avail=10 → usage=402, target=502, need=90
        s = st(boot, ballooned, 10 * MiB)
        a = decide_action(DEFAULT, s)
        assert a.kind == "plug"
        assert a.target_allocated_bytes <= boot

    def test_no_restore_without_ballooned(self):
        # low slack but nothing ballooned
        s = st(512 * MiB, 0, 10 * MiB)
        a = decide_action(DEFAULT, s)
        assert a.kind == "none"
        assert a.reason == "within_hysteresis" or a.bytes == 0

    def test_no_restore_in_hysteresis_band(self):
        # slack=50 (32 < 50 < 250), some ballooned → none
        boot = 1024 * MiB
        ballooned = 200 * MiB
        allocated = 824 * MiB
        s = st(boot, ballooned, 50 * MiB)
        a = decide_action(DEFAULT, s)
        assert a.kind == "none"
        assert a.reason == "within_hysteresis"

    def test_exactly_at_low_slack_triggers(self):
        boot = 1024 * MiB
        ballooned = 512 * MiB
        allocated = 512 * MiB
        s = st(boot, ballooned, 32 * MiB)  # slack=32
        a = decide_action(DEFAULT, s)
        assert a.kind == "plug"


# ===========================================================================
# apply_action
# ===========================================================================

class TestApplyAction:
    def test_none_identity(self):
        s = st(512 * MiB, 0, 100 * MiB)
        a = BalloonAction("none", 0, 512 * MiB, "x")
        assert apply_action(s, a) == s

    def test_unplug_keeps_usage(self):
        s = st(1024 * MiB, 0, 600 * MiB)
        usage_before = compute_usage(1024 * MiB, 600 * MiB)
        a = BalloonAction("unplug", 200 * MiB, 824 * MiB, "t")
        n = apply_action(s, a)
        assert n.ballooned_bytes == 200 * MiB
        alloc = compute_allocated(n.boot_cap_bytes, n.ballooned_bytes)
        assert alloc == 824 * MiB
        usage_after = compute_usage(alloc, n.available_bytes)
        assert usage_after == usage_before

    def test_plug_keeps_usage(self):
        s = st(1024 * MiB, 400 * MiB, 50 * MiB)
        alloc_before = 624 * MiB
        usage_before = compute_usage(alloc_before, 50 * MiB)
        a = BalloonAction("plug", 100 * MiB, 724 * MiB, "t")
        n = apply_action(s, a)
        assert n.ballooned_bytes == 300 * MiB
        alloc = compute_allocated(n.boot_cap_bytes, n.ballooned_bytes)
        usage_after = compute_usage(alloc, n.available_bytes)
        assert usage_after == usage_before

    def test_unplug_too_much(self):
        s = st(64 * MiB, 0, 10 * MiB)
        a = BalloonAction("unplug", 128 * MiB, 0, "t")
        with pytest.raises(ValueError):
            apply_action(s, a)

    def test_plug_too_much(self):
        s = st(512 * MiB, 8 * MiB, 10 * MiB)
        a = BalloonAction("plug", 16 * MiB, 0, "t")
        with pytest.raises(ValueError):
            apply_action(s, a)

    def test_unaligned_action_bytes(self):
        s = st(512 * MiB, 0, 100 * MiB)
        a = BalloonAction("unplug", 123, 0, "t")
        with pytest.raises(ValueError, match="page-aligned"):
            apply_action(s, a)

    def test_zero_bytes_plug_rejected(self):
        s = st(512 * MiB, 8 * MiB, 10 * MiB)
        a = BalloonAction("plug", 0, 0, "t")
        with pytest.raises(ValueError):
            apply_action(s, a)


# ===========================================================================
# end-to-end policy convergence
# ===========================================================================

class TestConvergence:
    def test_idle_converges_near_reserve(self):
        """Repeated ticks on idle guest settle near usage+reserve."""
        s = st(2048 * MiB, 0, 1600 * MiB)
        for _ in range(20):
            a = decide_action(DEFAULT, s)
            if a.kind == "none":
                break
            s = apply_action(s, a)
        alloc = compute_allocated(s.boot_cap_bytes, s.ballooned_bytes)
        usage = compute_usage(alloc, s.available_bytes)
        slack = compute_slack(alloc, usage)
        assert slack < DEFAULT.high_slack_bytes
        assert slack >= DEFAULT.low_slack_bytes
        # close to reserve
        assert abs(slack - DEFAULT.reserve_slack_bytes) <= DEFAULT.step_bytes

    def test_load_spike_restores_then_stable(self):
        """After deep reclaim, spike usage → plug → hysteresis band."""
        # start reclaimed
        s = st(2048 * MiB, 1500 * MiB, 80 * MiB)  # alloc=548, slack=80
        # simulate spike: available drops to 10M (usage climbs)
        s = BalloonState(s.boot_cap_bytes, s.ballooned_bytes, 10 * MiB)
        saw_plug = False
        for _ in range(20):
            a = decide_action(DEFAULT, s)
            if a.kind == "plug":
                saw_plug = True
            if a.kind == "none":
                break
            s = apply_action(s, a)
        assert saw_plug
        alloc = compute_allocated(s.boot_cap_bytes, s.ballooned_bytes)
        assert alloc <= s.boot_cap_bytes
        usage = compute_usage(alloc, s.available_bytes)
        slack = compute_slack(alloc, usage)
        assert slack > DEFAULT.low_slack_bytes

    def test_never_exceeds_hard_cap_across_ticks(self):
        s = st(512 * MiB, 200 * MiB, 5 * MiB)
        for _ in range(50):
            a = decide_action(DEFAULT, s)
            if a.kind == "none":
                break
            s = apply_action(s, a)
            alloc = compute_allocated(s.boot_cap_bytes, s.ballooned_bytes)
            assert alloc <= 512 * MiB
            assert s.ballooned_bytes >= 0

    def test_hard_cap_means_no_inflate_past_boot(self):
        """Even with zero slack and zero ballooned, cannot grow past boot."""
        s = st(512 * MiB, 0, 0)
        a = decide_action(DEFAULT, s)
        assert a.kind == "none"

    def test_user_reserve_100_not_zero(self):
        """After reclaim, must keep ~100MB slack, not drain to zero."""
        s = st(1024 * MiB, 0, 900 * MiB)
        a = decide_action(DEFAULT, s)
        assert a.kind == "unplug"
        s = apply_action(s, a)
        alloc = compute_allocated(s.boot_cap_bytes, s.ballooned_bytes)
        usage = compute_usage(alloc, s.available_bytes)
        slack = compute_slack(alloc, usage)
        assert slack >= 100 * MiB - 4 * MiB
        assert slack > 0
