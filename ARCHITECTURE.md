# UML Auto-Balloon Architecture

## Goal

Guest-driven memory balloon for User-Mode Linux so idle RAM is returned to
the host, while the guest can still grow back up to the boot-time hard cap
without host intervention.

## Non-goals

- Hot-add beyond boot `mem=` (hard cap is fixed).
- Changing guest `MemTotal` (stays at boot size; balloon pages are held out
  of the buddy allocator, same as upstream mconsole `mem=`).
- Host-orchestrated multi-VM balancer (can be added later on top).

## Background: upstream UML already has the punch primitive

`arch/um/drivers/mconsole_kern.c` registers mconsole device `mem`:

| Command | Effect |
|---|---|
| `config mem=-N` | Alloc page(s) from guest buddy, `madvise(MADV_REMOVE)` on physmem mapping, keep pointer in `unplugged_pages` |
| `config mem=+N` | `free_page` previously unplugged pages back to buddy |

Host backing is a memfd (`uml-physmem-memfd.patch`), so `MADV_REMOVE` returns
shmem pages to the host immediately. Verified: host supports `MADV_REMOVE`
on memfd.

What is missing is **policy + autonomy**: something inside the guest that
decides when to call the same unplug/plug path.

## Definitions

```
boot_cap      = physmem size from boot `mem=`          # immutable hard cap
ballooned     = unplugged_pages_count * PAGE_SIZE      # currently returned to host
allocated     = boot_cap - ballooned                   # still plugged into guest
usage         = allocated - available                  # actively used / not reclaimable
available     = si_mem_available()                     # free + lightly reclaimable
slack         = allocated - usage  (= available)       # headroom inside allocated
```

### Why not "free > 250MB"?

`MemFree` alone ignores page cache that the guest can drop. Using
`si_mem_available()` makes:

```
slack = allocated - usage ≈ MemAvailable
```

So the trigger is **"usage is ≥ 250MB below what is currently allocated"**,
not a raw free-page counter. Cache-heavy but idle guests still reclaim;
guests under reclaim pressure do not.

### Hard cap / logical RAM

- Apps still see `MemTotal = boot_cap`.
- Balloon never plugs above `boot_cap` (nothing to plug).
- Balloon never unplugs below a safety floor (kernel + reserve).

## Policy

Defaults (sysfs-tunable):

| Knob | Default | Meaning |
|---|---|---|
| `high_slack_bytes` | 250 MiB | Reclaim when `slack ≥ high_slack` |
| `reserve_slack_bytes` | 100 MiB | After reclaim, leave this much slack |
| `low_slack_bytes` | 32 MiB | Restore (plug) when `slack ≤ low_slack` |
| `min_allocated_bytes` | 64 MiB | Never balloon below this allocated floor |
| `interval_ms` | 5000 | Policy tick |
| `enabled` | 1 | Master switch |

### Reclaim (return RAM to host) — "deflate allocated"

```
if enabled and slack >= high_slack_bytes:
    target_allocated = usage + reserve_slack_bytes
    target_allocated = max(target_allocated, min_allocated_bytes)
    target_allocated = min(target_allocated, boot_cap)
    drop = allocated - target_allocated
    if drop >= PAGE_SIZE:
        unplug(drop)   # same path as config mem=-drop
```

Example: `boot_cap=2G`, `allocated=2G`, `usage=400MB` → `slack=1.6G ≥ 250MB`
→ `target_allocated = 400 + 100 = 500MB` → drop `1.5G` to host.

### Restore (give RAM back to guest) — "inflate allocated toward need"

```
if enabled and slack <= low_slack_bytes and ballooned > 0:
    # Aim for reserve_slack headroom again, without exceeding boot_cap
    target_allocated = min(usage + reserve_slack_bytes, boot_cap)
    need = target_allocated - allocated
    if need >= PAGE_SIZE:
        plug(min(need, ballooned))   # same path as config mem=+need
```

Example: workload jumps, `usage=450MB`, `allocated=500MB`, `slack=50MB ≤ 32MB`
→ wait, 50 > 32 so not yet. If `usage=480MB`, `slack=20MB` →
`target = min(480+100, boot_cap)=580MB` → plug 80MB from balloon.

### Hysteresis

```
high_slack (250)  >>>  reserve (100)  >>>  low_slack (32)
```

Prevents oscillate: reclaim stops at 100MB slack; restore only when slack
crumbles to ≤32MB.

### Coarse steps

Unplug/plug in whole pages; policy may quantize to e.g. 4 MiB steps to
amortize mconsole/buddy lock costs (`step_bytes`, default 4 MiB).

## Components

```
┌──────────────────────── guest (UML) ─────────────────────────┐
│                                                              │
│  uml-balloon policy kthread  (arch/um/drivers/uml_balloon.c) │
│       │ reads si_mem_available(), unplugged_pages_count      │
│       │                                                      │
│       ▼                                                      │
│  balloon core API  (refactored from mconsole mem_mc)         │
│       uml_balloon_unplug(n_pages)                            │
│       uml_balloon_plug(n_pages)                              │
│       uml_balloon_stats(...)                                 │
│       │                                                      │
│       ├─► buddy alloc_page / free_page                       │
│       └─► os_drop_memory() → madvise(MADV_REMOVE)            │
│                                                              │
│  sysfs: /sys/kernel/uml_balloon/                             │
│       enabled, high_slack_bytes, reserve_slack_bytes,        │
│       low_slack_bytes, min_allocated_bytes, interval_ms,     │
│       step_bytes, boot_cap_bytes, allocated_bytes,           │
│       ballooned_bytes, usage_bytes, slack_bytes              │
│                                                              │
│  mconsole mem=±N  ──► same core API (unchanged UX)           │
└──────────────────────────────────────────────────────────────┘
         │
         │ host physmem memfd pages punched
         ▼
┌──────────────── host ────────────────┐
│  UML process RSS / memfd shmem drops │
└──────────────────────────────────────┘
```

## Refactor of existing code

Extract from `mconsole_kern.c` into `arch/um/drivers/uml_balloon.c` + header:

- `unplugged_pages` list, mutex, count
- unplug/plug loops
- `can_drop_memory` gate

`mem_config()` becomes a thin wrapper calling the core API.
Auto-balloon kthread calls the same API.

## Safety

1. **Boot gate**: only register if `can_drop_memory()` (host MADV_REMOVE OK).
2. **min_allocated**: never unplug below floor (avoid guest OOM for kernel).
3. **GFP_ATOMIC / reclaim context**: unplug uses `GFP_ATOMIC` like upstream;
   on alloc failure, stop this tick (retry later).
4. **Hotplug lock**: single `plug_mem_mutex` for mconsole + kthread.
5. **Disable under OOM**: if `oom_killer` active or `slack` already low, skip reclaim.
6. **Sysfs `enabled=0`**: immediate pause; does not auto-plug everything back
   (operator can `mem=+` manually or set enabled and wait for low_slack).

## Observability

- `dmesg`: rate-limited notices on reclaim/restore (bytes in/out).
- sysfs stats (above) for scripts.
- Optional: keep mconsole `config mem` working for manual override.

## Interaction with base images

No rootfs change required for P0 (kernel thread is self-contained).
Optional later: `rootfs/nocloud/user-data` snippet to tune sysfs defaults
per workload.

## Failure modes

| Case | Behaviour |
|---|---|
| Host lacks MADV_REMOVE | balloon core not registered; boot warning; no-op |
| Guest under memory pressure | low slack → plug if ballooned; never reclaim |
| Spike after deep reclaim | reserve 100MB + low_slack 32MB hysteresis; plug on demand |
| Manual `mem=-` while auto on | shared mutex; next tick re-evaluates targets |

## Testing strategy

1. **Unit (host)**: pure policy functions in `balloon/tests/` (pytest) — every
   branch of reclaim/restore math.
2. **Unit (policy ABI)**: C tests mirroring kernel decision helper if built
   userspace-side.
3. **Integration**: boot UML `mem=512M`, allocate 100MB, idle → host RSS drops
   toward ~200MB; allocate 300MB more → RSS climbs; never exceeds 512M.
