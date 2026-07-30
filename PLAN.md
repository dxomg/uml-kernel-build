# Plan: UML Guest Auto-Balloon

## Policy (locked)

- `allocated = boot_cap - ballooned`
- `usage = allocated - si_mem_available()`
- `slack = allocated - usage` (= available)
- **Reclaim** when `slack >= 250MiB` → target slack `100MiB`
- **Restore** when `slack <= 32MiB` and `ballooned > 0` → target slack `100MiB`
- **Hard cap** = boot `mem=`; never plug above it
- **Floor** = `min_allocated` 64MiB

## Phases

### P0 — Policy library + unit tests (this session)
- [x] ARCHITECTURE.md / PLAN.md
- [x] `balloon/src/policy.py` — pure functions
- [x] `balloon/tests/test_policy.py` — every function/branch (74 passed)
- [x] pytest green

### P1 — Kernel patch (refactor + kthread)
- [x] `patches/uml-balloon-auto.patch`
  - Extract balloon core from mconsole mem_mc
  - sysfs under `/sys/kernel/uml_balloon/`
  - kthread running policy each `interval_ms`
  - mconsole `mem=±` calls core API
- [x] Wire into kernel build workflows (all `*.patch`, incl. lts_latest)
- [x] Document sysfs + defaults in README

### P2 — Integration test on this VM
- [ ] Build or patch-test against running 6.18.38 UML if feasible
- [ ] Boot mem=512M, measure host RSS before/after idle reclaim
- [ ] Stress alloc inside guest, confirm restore
- [ ] Confirm hard cap respected

### P3 — Optional polish
- [ ] `step_bytes` batching, dmesg ratelimit
- [ ] cloud-init knob to set defaults
- [ ] Host-side helper to read stats via mconsole `proc`

## Acceptance

1. pytest 100% on policy module.
2. Idle guest with boot_cap=512M and usage≈50M ends near allocated≈150M
   (usage+100M reserve), host RSS down accordingly.
3. Guest can grow back to 512M under load without reboot.
4. Manual mconsole `config mem=±` still works.
