# UML Kernel + SLIRP + Linux cloud images

This guide shows how to boot an **User-Mode Linux (UML)** kernel with a Linux cloudimg using **SLIRP** for network connectivity.

---

## 1. Prerequisites

Make sure you have in the same directory:

- `linux` → UML kernel binary
- `slirp` → static SLIRP binary
- `disk.img` → Any Linux cloudimg

---

## 2. Boot UML

Run the following command from the directory containing the files:

```bash
./linux \
    mode=skas0 \
    mem=2048M \
    ubd0=disk.img \
    root=/dev/ubda1 \
    rw \
    init=/lib/systemd/systemd \
    eth0=slirp,,./slirp \
    con0=fd:0,fd:1 \
    con=null
````

### Explanation of key options:

* `mode=skas0` → required for SKAS0 mode for UML
* `mem=2048M` → allocate 2GB memory to the guest
* `ubd0=...` → the root filesystem image
* `root=/dev/ubda1 rw` → use first partition (partition 1) of UML disk as root, read-write mode
* `init=/lib/systemd/systemd` → start systemd as init
* `eth0=slirp,,./slirp` → connect guest `eth0` to SLIRP for network
* `con0=fd:0,fd:1` → console attached to current terminal
* `con=null` → disable other consoles

---

## 3. Setup Networking inside UML

Once inside the UML guest, configure the network to access the internet:

```bash
# Configure eth0 IP and bring it up
ip a add dev eth0 10.0.2.1
ip l set eth0 up

# Set default route
ip r add default dev eth0

# Configure DNS
echo "nameserver 10.0.2.3" > /etc/resolv.conf
```

After this, the guest should be able to access the internet via SLIRP.

### Networking config (`config.yaml`)

`vde_plug` reads `config.yaml` from **its own directory** (resolved via
`/proc/self/exe`, so finding the binary through `PATH` still works). See
`slirp.yaml` in this repo for a fully commented template.

Note that UML locates the helper with `execvp("vde_plug")` — the path in
`vnl=` is *not* used for that. The binary's directory must be in `PATH`:

```bash
export PATH="$PWD:$PATH"
```

#### Private network between instances

With `switch: true` (the default) every instance sharing one socket lands on
a single L2 segment. The first instance to bind the socket becomes the **hub**
and runs the uplink (slirp NAT or a host tap); later instances are **peers**,
plain wires into the hub. The hub is a MAC-learning switch, so unicast between
two guests does not hit the others.

Because one DHCP server now serves the whole segment, guests receive
**distinct leases** — `10.0.2.15`, `.16`, `.17`, … — instead of every instance
claiming `.15`. Nothing needs to be configured per instance.

Socket path resolution, in order:

1. `socket:` in `config.yaml`
2. `$VDE_SWITCH_SOCKET`
3. `/tmp/vde.socket` — the shared default
4. `$TMPDIR/vde.socket`
5. `<binary dir>/vde.socket`

Steps 4–5 matter in sandboxes such as Pterodactyl where `/tmp` is missing or
read-only. The resolved path is printed on the status line. A leftover socket
file from a crashed hub is reclaimed automatically (`connect()` is tried before
`bind()`, so a live hub is never disturbed).

Set `switch: false` for the classic one-NAT-per-instance behaviour.

#### Nested hypervisor: Proxmox/LXC bridging onto `vmbr0`

The common Proxmox layout works as-is. Inside the UML guest, make `vec0` a
plain bridge port and let `vmbr0` carry the address:

```bash
ip link add name vmbr0 type bridge
ip link set vmbr0 type bridge forward_delay 0   # else DHCP times out
ip link set vec0 master vmbr0
ip link set vec0 up
ip link set vmbr0 up
```

Containers attached to `vmbr0` with `dhcp` then lease directly from
`vde_plug`, exactly like a bridged network. Each distinct MAC gets its own
address — no per-container configuration, no NAT inside the guest:

```
vmbr0 -> 10.0.2.15/24     (the Proxmox host itself)
ct1   -> 10.0.2.16/24
ct2   -> 10.0.2.17/24
...
```

Guest-to-guest traffic and gateway access both work. One caveat: slirp
forwards TCP and UDP but **not** ICMP to the internet, so `ping 1.1.1.1` fails
from a container while TCP/DNS succeed. `ping` to the gateway (`10.0.2.2`) and
between containers is fine.

The DHCP pool is sized for this case. Upstream libslirp caps itself at
`NB_BOOTP_CLIENTS=16` leases, which a container host exhausts quickly, so CI
builds libslirp from source with the cap raised to **240** — the rest of the
/24 above `dhcp_start`. Verified with 40 simultaneous containers:

```
LEASES_OK=40 / 40      last lease 10.0.2.55
```

A distro-packaged libslirp still works but stops at 16 leases.

#### Uplink modes

| `uplink:` | Behaviour |
|---|---|
| `slirp` | Userspace NAT, no privileges. Default. |
| `tap:NAME` | Attach to an existing host tap. DHCP comes from the real LAN, so guests get real addresses — this is the Proxmox/bridged case. Needs `/dev/net/tun`; falls back to slirp if unavailable. |
| `none` | Isolated segment: guest-to-guest only, no internet. |

The tap must already exist and be enslaved to the bridge (e.g. `vmbr0`);
`vde_plug` only opens it.

#### Port forwarding

```yaml
portfwd: true
ports:
  - 2222:22                 # fallback: first client on the segment
  - 10.0.2.16:2223:22       # pinned to one guest
  - .17:2224:22             # same, short form
  - udp 5353:53
```

Two rule shapes:

| Form | Meaning |
|---|---|
| `HOST:GUEST` | **Fallback.** Follows the first client to lease an address. |
| `ADDR:HOST:GUEST` | **Pinned** to that guest. `ADDR` may be full (`10.0.2.16`) or the `.N` shorthand (`.17`), completed from `network`. |

In **standalone** mode there is only ever one guest, so `HOST:GUEST` resolves to
`dhcp_start` right away and behaves exactly as it always has.

On a **shared switch** several clients lease addresses, so a bare `HOST:GUEST`
rule is ambiguous. It is installed against `dhcp_start` up front, then latched
onto whichever address is actually handed out first (the hub watches DHCP ACKs
on the uplink) and left there. Pinned rules never move. Leases are allocated in
order from `dhcp_start`, so the second guest is `.16`, the third `.17`, etc.

Only the hub publishes forwards, since only the hub owns the slirp stack.
Malformed rules are reported and skipped rather than aborting startup.

#### Micro-batching

```yaml
batch: 16     # frames per recvmmsg/sendmmsg; 1 disables, 64 is the ceiling
```

The hot path used to cost one `recv` plus one `send` per frame. It now drains up
to `batch` frames with a single `recvmmsg(MSG_DONTWAIT)` and forwards them with
`sendmmsg`.

`MSG_DONTWAIT` is the important part: only frames `poll()` has *already* made
ready get taken. There is no timer and no waiting for a batch to fill, so the
batch size is whatever happened to be queued — which is why throughput improves
without latency moving. A blocking `recvmmsg` would wait for the whole batch and
would add exactly the delay that must be avoided.

Measured between two UML guests on one switch (2-core host, iperf3, 4 runs each):

| | `batch: 1` | `batch: 16` |
|---|---|---|
| TCP throughput | 1225–1275 Mbit/s | 1256–1396 Mbit/s |
| ping avg / max | 0.203 / 0.972 ms | 0.199 / 0.280 ms |
| hub syscalls (6 s) | 530k | 131k |
| retransmits per GB | ~9.5k | ~9.1k |

About +6% throughput for a 4x drop in syscalls, and the latency tail got
*tighter* rather than worse. Retransmits per byte did not rise, which is the
check that matters for ordering — see below.

Details worth knowing before tuning this:

* **Ordering is preserved.** The hub walks the batch, computes each frame's
  destination, and flushes one `sendmmsg` per *run* of consecutive frames
  sharing a destination. A run is emitted before the next begins, so frames to
  any given port leave in arrival order. Reordering inside a TCP flow would cost
  more in retransmits than batching saves.
* **The win scales with peers, not frame rate.** A broadcast costs one send per
  port, so flood traffic is where batching pays; `recvmmsg` alone saves little
  because on a unix socket the syscall boundary (~86 ns here) is dwarfed by
  per-message kernel work (~900 ns).
* **Batches rarely fill.** `poll()` returns on the first frame, so most reads
  pick up well under 16. Raising `batch` past 16 mostly just costs memory
  (`batch` × 9234 bytes per direction; 16 ≈ 150 KiB).
* **The uplink stays unbatched.** slirp is a userspace NAT reached through
  `vdeslirp_send()`, not a socket, so it is still fed one frame at a time.
* `batch: 1` uses plain `recv`/`send`, not a one-element `recvmmsg`, so it is a
  genuine A/B switch.

---

## 4. Notes

* You can use any Linux Cloud Image compatible with UML.
* Adjust `mem` parameter according to your host's available RAM.
* SLIRP allows network access without needing root privileges.
* `init=/sbin/init` works for both systemd and OpenRC guests.


---

## 5. Bundled kernel patches (`patches/`)

Every kernel-build workflow applies all `patches/*.patch` on top of the
released tarball before configuring. Patches are authored against the latest
LTS but use minimal context so they forward/back-port cleanly.

### `uml-physmem-memfd.patch`

By default UML backs the entire guest "physical" memory with an unlinked
tempfile created in `$TMPDIR` (falling back to `/dev/shm`, then `/tmp`). On a
host where the tempdir is **not** tmpfs-backed, those pages become regular
file-backed dirty pages and are throttled by the host's `vm.dirty_ratio`,
which noticeably degrades guest performance.

This patch makes `create_mem_file()` prefer an anonymous **`memfd_create()`**
file descriptor for physmem. The memfd is backed by the kernel's internal
shmem/tmpfs, so guest RAM pages are never written back to a real device,
regardless of whether the host has a usable tmpfs tempdir. If `memfd_create`
is unavailable (very old host kernels) it falls back to the original
on-disk tempfile. This mirrors the exact pattern already used by UML's own
stub-executable allocator (`init_stub_exe_fd` in `arch/um/os-Linux/skas/process.c`).

**Result:** the guest boots and runs at full speed even when `TMPDIR` points
at a plain on-disk directory (verified: guest memory shows as
`/memfd:uml-physmem (deleted)` and the tempdir stays empty).


### `uml-memdrop-on-free.patch`

`memfd` stops host writeback, but host RSS still tracks the guest high-water
mark because physmem is one long `MAP_SHARED` mapping.

This patch registers UML with the kernel **PAGE_REPORTING** framework (same
machinery virtio free-page reporting uses). When enough free buddy pages of
a given order accumulate, mm isolates them (they cannot be allocated), calls
our reporter which `MADV_REMOVE`s the host backing, then returns them to the
freelist. Guest free-count is unchanged; host RSS shrinks; next touch
zero-faults.

This avoids the traps of naive approaches:

* delayed punch of a just-freed address → use-after-reuse
* punching a random `alloc_page` → wrong pages (cold freelist)
* fixed PFN queue → overflows on large munmap

Default is **batch**, not per-4K free-path syscalls:

* `memdrop=batch` (default) — report free pages of order ≥ **3** (~32KiB)
* `memdrop=on` — order ≥ **0** (framework still batches into scatterlists)
* `memdrop=<order>` — custom minimum free order
* `memdrop=off` — disabled

Requires `CONFIG_PAGE_REPORTING=y` (enabled in `containers.config`).


### UML SMP support (`patches/apply-smp.sh`, `patches/smp-backport/`)

Upstream UML was single-CPU for its entire history until **v6.19** (Oct 2025),
which landed the initial SMP support (commit `1e4ee5135d81` by Tiwei Bie).
6.18 LTS — the current LTS — predates that and will never get SMP natively.

This repo bridges that gap:

* **Kernel ≥ 6.19** — native SMP; `apply-smp.sh` is a no-op, only
  `CONFIG_SMP=y NR_CPUS=64` is enabled in the build config.
* **Kernel 6.18.x** — the full upstream SMP series (20 commits, base
  6.18-rc3) is backported. `apply-smp.sh` applies the cumulative patch via
  `git apply --3way` (falls back to `patch --fuzz=3`), which survives minor
  context drift as 6.18.x stable backports accumulate. Series applies
  cleanly to 6.18.37/6.18.38 and builds/boots verified.
* **Kernel ≤ 6.17 (incl. 6.12 LTS)** — the series base is too far away to
  port safely; SMP stays off and the kernel boots single-CPU as upstream.

To actually use multiple vCPUs, boot with:

```bash
./linux mem=2G ncpus=8 seccomp=on ...   # up to NR_CPUS (64) vCPUs
```

`ncpus=N` sets how many vCPUs to start; `seccomp=on` is **required**
(SMP is incompatible with the default PTRACE userspace mode and will refuse
to boot without it). Each vCPU is a host thread.

Note: with SMP enabled, UML userspace stubs remain single-threaded per
process — kernel-mode execution and kthreads are parallel, but userspace
threads of a single process still serialize within that process's stub.
This is an upstream limitation of the initial SMP support.

### Backported generic UML fixes (`patches/um-backport/`)

The [linux-um-arm64](https://github.com/zalexdev/linux-um-arm64) series
(41 commits on a 7.2-rc4 uml-tree base) contains a set of generic `um/`
and x86 fixes that are not arm64-specific. Three of them apply cleanly to
6.18.x **on top of the SMP backport** and are shipped here.
`patches/apply-backports.sh` applies them after the bundled patches,
best-effort (`git apply --3way`; a patch whose context drifted is skipped
with a `::warning`, never fatal):

| Patch | What it does |
|---|---|
| `um-backport-01` | `PTRS_PER_PTE` derived from `PAGE_SHIFT` instead of a literal 512 |
| `um-backport-02` | no panic at shutdown when `uml_dir` was never created |
| `um-backport-03` | hand dead stubs to the SIGCHLD reaper instead of blocking in `waitpid()` — faster `execve` path, no zombie leak |

Everything else in the series is deliberately **not** backported:

* `um/arm64:` and `tools/um-arm64:` commits need the arm64 UML subarch
  skeleton, which exists only in the uml tree `next` branch — not in any
  LTS, and not even in mainline as of v7.2-rc4.
* The fault-around, ncpus-default and seccomp-probe fixes target the 7.2
  stub redesign; their context is too far from 6.18 to port mechanically.
* One more fix that applies cleanly (`ARCH_INIT_SP_RESERVE` for the stub
  syscall handler stack) is dropped anyway: the symbol it uses does not
  exist on 6.18 — it arrived with the 7.2 stub redesign. A clean
  `git apply` is not sufficient on its own; the accepted set is
  compile-tested before shipping.

Selection was verified empirically: the whole series was applied to
v6.18.38 + SMP backport with `git apply --check --3way`, and the accepted
set was compile-tested. `apply-backports.sh` must run **after**
`apply-smp.sh` — the reaper fix builds on the SMP backport's threading
changes in `os-Linux/skas/process.c`.

### Full arm64 port for 7.2.4 (`patches/arm64-port-7.2.4.patch`, `stable_arm64.yml`)

The arm64 subarch skeleton has since landed in the uml tree as a complete
**54-commit series** rebased on **v7.2.4**. `arm64-port-7.2.4.patch` is that
series, consolidated: every commit was applied in order on top of v7.2.4 +
this repo's bundled patches (including `vector-static-link.patch`, whose
`MAY_HAVE_RUNTIME_DEPS` removal the series' bionic commit redoes with a full
explanation in `arch/um/drivers/Kconfig`), and the handful of hunks that
conflicted were merged by hand. It applies as a single clean `git apply`.

What it brings to `ARCH=um SUBARCH=arm64`:

* the `arch/arm64/um/` subarch (defconfig, ptrace/signal, FP/SIMD state
  save/restore across signals, `arm64_defconfig` with 16 KB pages)
* loadable module support, and build against bionic (Android NDK)
* the 7.2 stub rework fixes: no `-ENOSYS` leak into the first guest syscall,
  syscall interception where `-1` cannot be written, elided stub-handoff
  wake with brief spin, cycle-counter probe
* `/proc/cpuinfo` reworked behind `arch_show_cpuinfo()` /
  `arch_parse_host_cpu_flags()` callbacks so each subarch prints the same
  lines real `/proc/cpuinfo` has (x86: `fpu`/`flags`; arm64: `Features`);
  `get_host_cpu_features()` now takes a single line callback

`.github/workflows/stable_arm64.yml` builds it on `ubuntu-22.04-arm`
(**native** — no cross toolchain or sysroot needed) with `LLVM=1`. The jammy
archive only ships clang 14, too old for the stub link below, so the workflow
installs LLVM 18 from apt.llvm.org first. Two
toolchain notes carry over to any manual build:

* `LLVM=1` is required on 7.2.x: the stub link uses `-Wl,--no-rosegment`,
  which GNU ld (≤ 2.43) rejects. The stub link goes through the clang
  driver, which does not pick up lld from `LLVM=1`, so the make invocation
  passes `STUB_EXE_LDFLAGS="-Wl,-n -Wl,--no-rosegment -static -fuse-ld=lld"`.
* The UML SMP backport is **not** applied here — 7.2.4 has native SMP, and
  the series was gate-tested against the plain tree.

The x86 workflows are unaffected: they target 6.18 LTS, where this patch
does not apply. The x86-relevant parts (generic `um/` fixes, the cpuinfo
callback split) were compile-tested on the merged tree with
`make ARCH=um LLVM=1`.

## Container support (Docker / Podman / LXC)

`patches/containers.config` is merged into every kernel build, then the
workflow re-asserts critical knobs with `scripts/config` so unknown symbols
on older LTS do not silently drop features. Enabled primitives:

* **Namespaces** — `user/pid/net/ipc/uts/cgroup` (userns for rootless).
* **Cgroup v2** — controllers including `memory/cpu/io/pids/device/cpuset`,
  plus `FAIR_GROUP_SCHED` / `CFS_BANDWIDTH` / freezer / cpuacct / BPF.
  Mount v2 in the guest:

  ```bash
  mount -t cgroup2 none /sys/fs/cgroup
  echo "+memory +cpu +pids +cpuset" > /sys/fs/cgroup/cgroup.subtree_control
  ```

  (systemd does this automatically.)
* **Seccomp-filter** — hard-required by Docker/Podman.
* **Checkpoint/restore** — `CHECKPOINT_RESTORE` for CRIU-style tooling.
* **Networking** — `veth`, `bridge`, `bridge-nf`, `macvlan`, `ipvlan`, `tap`,
  full netfilter/iptables (+ ip6tables) for publish-port NAT.
* **Storage** — `overlay2` (overlayfs), `fuse`, `fhandle`, ext4 POSIX ACL +
  security xattrs. Extra filesystems and volume managers live in
  `patches/storage.config` (see [Storage & filesystems](#storage--filesystems)).
* **memdrop** — `PAGE_REPORTING=y` (pairs with `uml-memdrop-on-free.patch`).

### UML-specific caveats

1. **No host TAP** — `veth`/`bridge` work *inside* the guest for
   container↔container traffic, but egress to the host goes through UML's
   own vector/slirp transport, not a host bridge.
2. **No KVM/hardware virt** — nested containers run as plain user-space
   (not nested VMs). Nested *UML* does not work either: UML deliberately
   returns `-EIO` for `PTRACE_SYSEMU` (`arch/um/kernel/ptrace.c`), which an
   inner UML requires. Under `seccomp=on` the inner kernel boots as far as
   `Run /sbin/init as init process` and then its userspace dies, because
   `MAP_SHARED|MAP_ANONYMOUS` is not coherent across `CLONE_VM` in a UML
   guest — exactly what the seccomp stub's `struct stub_data` relies on.
3. **`WRITE_ZEROES` unsupported** — the ubd backend rejects it, so `mkfs`
   on a loop device prints one
   `operation not supported error, dev loop0, ... op 0x9:(WRITE_ZEROES)`
   line. The block layer falls back to writing zeros; the result is correct.

ptrace itself is otherwise complete: `PTRACE_SYSCALL`, `GETREGSET`/`SETREGSET`,
`PEEKUSER`, `PEEKDATA` and `process_vm_readv` all behave as on hardware, so
**strace, gdb, proot and fakeroot work** (verified). gdb logs a cosmetic
`Attempted to relay unknown signal 5 (si_code = 128)` per breakpoint —
`relay_signal()` in `arch/um/kernel/trap.c` does not recognise the siginfo
layout of an `int3` trap, prints the warning, then delivers via `force_sig()`.

### Verified at runtime (6.18.38 + SMP + container config)

```
smp: Brought up 1 node, 2 CPUs
/sys/fs/cgroup/cgroup.controllers: cpu io memory pids misc
unshare --user --mount --pid --net --fork: NS_OK
ip link add veth0 type veth peer veth1: ok
ip link add br0 type bridge: ok
```

## Storage & filesystems

`patches/storage.config` is merged after `containers.config` in every kernel
workflow, and the critical symbols are re-asserted with `scripts/config`
afterwards. `merge_config.sh` silently drops a symbol whose dependencies are
not yet satisfied at merge time, and `olddefconfig` will not bring it back, so
the workflow re-sets each one and then **fails the build** if any is missing:

```bash
for sym in BLK_DEV_LOOP SQUASHFS VFAT_FS XFS_FS BTRFS_FS BLK_DEV_DM \
           BLK_DEV_MD DM_CRYPT DM_THIN_PROVISIONING NLS_UTF8 ISO9660_FS; do
  grep -q "^CONFIG_${sym}=[ym]" .config || exit 1
done
```

| Area | Symbols |
|---|---|
| Loop devices | `BLK_DEV_LOOP`, `BLK_DEV_LOOP_MIN_COUNT=16` |
| Squashfs | `SQUASHFS` + `FILE_DIRECT`, `XATTR`, zlib/lz4/lzo/xz/zstd |
| FAT | `FAT_FS`, `MSDOS_FS`, `VFAT_FS`, `EXFAT_FS`, default iocharset `utf8` |
| NLS | `NLS_UTF8`, `NLS_CODEPAGE_437/850`, `NLS_ISO8859_1/15`, `NLS_ASCII` |
| XFS | `XFS_FS`, `XFS_QUOTA`, `XFS_POSIX_ACL`, `XFS_RT`, `XFS_ONLINE_SCRUB` |
| btrfs | `BTRFS_FS`, `BTRFS_FS_POSIX_ACL` |
| Optical | `ISO9660_FS`, `JOLIET`, `ZISOFS`, `UDF_FS` |
| device-mapper | `BLK_DEV_DM`, `DM_SNAPSHOT`, `DM_THIN_PROVISIONING`, `DM_MIRROR`, `DM_RAID`, `DM_ZERO`, `DM_CACHE`, `DM_WRITECACHE`, `DM_ERA`, `DM_CLONE`, `DM_DELAY`, `DM_FLAKEY`, `DM_MULTIPATH` |
| LUKS / integrity | `DM_CRYPT`, `DM_VERITY`, `DM_INTEGRITY` + `CRYPTO_XTS/AES/SHA256/ESSIV` |
| md RAID | `BLK_DEV_MD`, `MD_LINEAR`, `MD_RAID0/1/10/456` |
| Quota | `QUOTA`, `QUOTACTL`, `QFMT_V2` |
| Out-of-tree modules | `MODULES`, `MODULE_UNLOAD`, `KALLSYMS_ALL`, `MODULE_SIG` off |

NLS matters more than it looks: VFAT stores long filenames as UTF-16, so
without `NLS_UTF8` a `mount -o iocharset=utf8` fails outright.

`SQUASHFS_FILE_DIRECT` and the decompressor mode are Kconfig `choice` members,
which `merge_config.sh` cannot always move off the default — the workflow sets
them explicitly.

### Using a loop device in the guest

Loop devices work; the earlier "no loop device" note in this README was wrong.
`CONFIG_BLK_DEV_LOOP=y` is built in, `/dev/loop-control` is present, and
devices past `BLK_DEV_LOOP_MIN_COUNT` are allocated on demand.

```bash
# explicit
L=$(losetup -f --show /path/to/image.img)
mount "$L" /mnt

# or the shorthand
mount -o loop /path/to/image.img /mnt
mount -o loop,ro,offset=1048576 disk.img /mnt      # offset/sizelimit work too
```

Verified in-guest: `mkfs.ext4` + mount + read/write, `mount -o loop`,
12 simultaneous loop devices, a loop image nested inside another loop image,
`ro`, and `-o offset=/--sizelimit`.

### LVM on a ubd disk

```bash
pvcreate /dev/ubdb
vgcreate vg0 /dev/ubdb
lvcreate -L 2G -n data vg0
mkfs.xfs /dev/vg0/data
mount /dev/vg0/data /mnt
```

Thin pools and snapshots are available (`DM_THIN_PROVISIONING`, `DM_SNAPSHOT`).
Install `lvm2` in the guest; the kernel side needs nothing beyond the above.

### LUKS

```bash
cryptsetup luksFormat /dev/ubdb          # aes-xts-plain64 + sha256 default
cryptsetup open /dev/ubdb secret
mkfs.ext4 /dev/mapper/secret
```

### ZFS

ZFS is **not** enabled by a kernel config option and cannot be — OpenZFS is
CDDL-licensed and lives outside the mainline tree, so no `CONFIG_ZFS` symbol
exists. What these builds provide instead are the prerequisites for compiling
it out-of-tree (`MODULES`, `MODULE_UNLOAD`, `KALLSYMS_ALL`, unsigned modules
allowed, zlib/lz4/zstd):

```bash
# against the same kernel tree the workflow built
./autogen.sh
./configure --with-linux=/path/to/linux-6.18.x --with-linux-obj=/path/to/linux-6.18.x
make -j"$(nproc)"
```

Be aware this is unproven on `ARCH=um`: OpenZFS's SPL leans on x86 FPU
save/restore and per-cpu primitives that UML implements differently, so expect
to do porting work. If you want CoW with snapshots and send/receive and don't
specifically need ZFS, **btrfs is enabled** and needs no out-of-tree build.

### Verified at runtime (6.18.41, artifact `linux-uml-lts-latest`)

Booted with three scratch ubd disks plus an ISO image, guest packages
`squashfs-tools lvm2 cryptsetup xfsprogs btrfs-progs dosfstools exfatprogs mdadm`:

```
/proc/filesystems: ext2 ext3 ext4 squashfs vfat msdos exfat iso9660 udf xfs btrfs
                   fuseblk overlay hostfs ...

squashfs -comp gzip/lz4/lzo/xz/zstd   all five mount + read back
mount -o loop,ro,threads=2            threads= accepted (MOUNT_DECOMP_THREADS)
vfat -o iocharset=utf8                'tên dài tiếng việt.txt' round-trips
exfat                                 mkfs + mount + write
xfs                                   mount, write, -o uquota -> "Quotacheck: Done."
btrfs                                 subvolume create + snapshot, crc32c
lvm  linear / striped -i2 / thinpool  all created, mkfs'd, mounted
lvm  snapshot                         origin="after", snapshot still "before"
thin volume 256M on a 96M pool        over-provisioning works
LUKS2 aes-xts-plain64, keysize 512    open, mkfs, write, close
md raid1 over 2 ubd disks             "active with 2 out of 2 mirrors", resync done
dm-integrity sha256                   format, open, mkfs, write
dm-verity sha256                      mount ro OK; after corrupting a block:
                                      "data block 2000 is corrupted" -> EIO
iso9660+Joliet                        mounted from /dev/ubdd and over loop
/dev/loop0..15                        BLK_DEV_LOOP_MIN_COUNT=16
```

Two tool-level gotchas found while testing, neither a kernel issue:

* `mkfs.xfs` refuses a device smaller than 300 MB.
* `mkfs.ext4` picks a 1024-byte block size on small images, which dm-verity
  then rejects with `bad block size 1024`. Use
  `mkfs.ext4 -b 4096` plus `veritysetup --data-block-size 4096`.

FAT also logs `utf8 is not a recommended IO charset for FAT filesystems,
filesystem will be case sensitive!` — upstream's standing advice is
`iocharset=iso8859-1` with `utf8=1` if case-insensitivity matters to you.
