# UML Kernel Build

User-Mode Linux, batteries included:

- **Kernels** — x86_64 and arm64 UML kernels built by CI from vanilla
  releases, with a set of bundled patches (memfd physmem, host-RSS
  reclaim, SMP backport for 6.18 LTS, full arm64 port, container and
  storage config). Artifacts boot any Linux cloud image, no root
  needed.
- **Network helper** — [`vdeplug-go/`](vdeplug-go/), a Go rewrite of
  `vde_plug` on gVisor's netstack: privileged-free NAT, DHCP, port
  forwarding, IPv6, a MAC-learning switch shared between instances,
  hub failover with lease inheritance, and a WebSocket bridge for
  peers across the internet.
- **Launcher** — [`launcher/boot`](launcher/boot) wraps kernel +
  engine + cloud image into one command.

## Quick start

```sh
# get a kernel + a base image from CI artifacts, then:
cd dir-with-linux-and-base.img
./boot            # boots with 2G RAM; root/root login
./boot 4G 2       # custom memory and vCPUs
```

The launcher reads `config.yaml` (copy
[`launcher/config.example.yaml`](launcher/config.example.yaml)) to pick
the engine (`netstack` default / `slirp`), uplink mode, port forwards,
and DHCP range. Guests DHCP by default — no network setup inside.

## Repository layout

| Path | What |
|---|---|
| `vdeplug-go/` | The Go network helper (default engine) — [its README](vdeplug-go/README.md) |
| `launcher/` | `boot` script + commented `config.example.yaml` |
| `legacy/` | The generation-1 standalone `slirp` helper (kept building) |
| `vde_plug/` | The C generation-2 helper (`engine: slirp`) |
| `patches/` | Kernel patches + config fragments merged by every build |
| `rootfs/nocloud/` | NoCloud seed baked into base images (DHCP by default) |
| `docs/` | [Distributed setup](docs/distributed.md) · [Technical changelogs](docs/technical_changelogs.md) |
| `.github/workflows/` | Kernel matrix, base images, helper builds, releases |

## Docs

- [`vdeplug-go/README.md`](vdeplug-go/README.md) — switch model,
  failover, uplinks, port forwarding, testing.
- [`docs/distributed.md`](docs/distributed.md) — joining machines over
  the internet via the WebSocket bridge.
- [`docs/technical_changelogs.md`](docs/technical_changelogs.md) — the
  full technical record: every patch explained, benchmark and runtime
  verification results, design decisions, and the changelog.
