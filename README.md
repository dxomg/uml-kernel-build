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

For port forwarding, create a file named `slirp_config` in same folder and add forwarded port in format `[guest_port]:[host_port]` like this:
```
22:2222
80:8080
...
```

---

## 4. Notes

* You can use any Linux Cloud Image compatible with UML.
* Adjust `mem` parameter according to your host's available RAM.
* SLIRP allows network access without needing root privileges.


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


### `uml-balloon-auto.patch`

Guest-driven memory balloon so idle UML RAM is returned to the host.

Upstream UML already supports manual ballooning via mconsole
(`config mem=-N` / `config mem=+N`) using `MADV_REMOVE` on the physmem
backing store. This patch:

1. Extracts that plug/unplug core into `arch/um/drivers/uml_balloon.c`
2. Keeps the mconsole `mem=` UX as a thin wrapper
3. Adds an auto-balloon kthread with the policy:

   - `allocated = boot_cap - ballooned` (boot_cap = boot `mem=`, hard cap)
   - `usage = allocated - si_mem_available()`
   - `slack = allocated - usage`
   - **reclaim** when `slack >= 250MiB` → leave **100MiB** reserve slack
   - **restore** when `slack <= 32MiB` → grow back toward 100MiB reserve
   - never plug above boot_cap; never unplug below 64MiB allocated

Sysfs (tunable): `/sys/kernel/uml_balloon/`

| file | default | role |
|---|---|---|
| `enabled` | 1 | master switch |
| `high_slack_bytes` | 250MiB | reclaim threshold |
| `reserve_slack_bytes` | 100MiB | target headroom after reclaim/restore |
| `low_slack_bytes` | 32MiB | restore threshold |
| `min_allocated_bytes` | 64MiB | floor |
| `step_bytes` | 4MiB | batch size |
| `interval_ms` | 5000 | policy tick |
| `boot_cap_bytes` / `allocated_bytes` / `ballooned_bytes` / `usage_bytes` / `slack_bytes` | ro | stats |

Policy math is unit-tested in userspace (`balloon/tests/`, pytest) and
must stay in sync with `decide()` in `uml_balloon.c`.

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

## Container support (Docker / Podman / LXC)

`patches/containers.config` is merged into every build via
`scripts/kconfig/merge_config.sh`, enabling full container primitives:

* **Namespaces** — `user/pid/net/ipc/uts/cgroup` (userns for rootless).
* **Cgroup v2** — all controllers (`memory/cpu/io/pids/device/...`). The
  kernel exposes v2 by mounting `cgroup2`; no Kconfig switch is needed,
  only the controllers. To use v2 in the guest:

  ```bash
  mount -t cgroup2 none /sys/fs/cgroup
  echo "+memory +cpu +pids" > /sys/fs/cgroup/cgroup.subtree_control
  ```

  `/sys/fs/cgroup/cgroup.controllers` then lists `cpu io memory pids misc`.
  (systemd does this automatically.)
* **Seccomp-filter** — hard-required by Docker/Podman.
* **Networking** — `veth`, `bridge`, `bridge-nf`, full netfilter/iptables
  (`MASQUERADE`, `conntrack`, `addrtype`, ...) for publish-port NAT.
* **Storage** — `overlay2` (overlayfs), `fuse`, `fhandle` for
  fuse-overlayfs rootless fallback.
* **BPF / security** — `bpf_syscall`, `keys`, `security`, `securityfs`.

### UML-specific caveats

1. **No loop device** (`/dev/loop*`) — Docker's loopback/devicemapper
   storage won't work. Use **overlay2** on a raw `ubd` backing file.
2. **No host TAP** — `veth`/`bridge` work *inside* the guest for
   container↔container traffic, but egress to the host goes through UML's
   own vector/slirp transport, not a host bridge.
3. **No KVM/hardware virt** — nested containers run as plain user-space
   (not nested VMs).

### Verified at runtime (6.18.38 + SMP + container config)

```
smp: Brought up 1 node, 2 CPUs
/sys/fs/cgroup/cgroup.controllers: cpu io memory pids misc
unshare --user --mount --pid --net --fork: NS_OK
ip link add veth0 type veth peer veth1: ok
ip link add br0 type bridge: ok
```
