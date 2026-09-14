# vdeplug-go — gVisor netstack rewrite of vde_plug

A drop-in replacement for the C `vde_plug` SLIRP helper that UML's VDE
VECTOR transport execs, written in Go on top of gVisor's netstack.
The C helper is still available (`engine: slirp`); this rewrite exists
to fix its structural problems: CPU burn in the frame pump, no failover,
and no isolation between guests.

## Layout

    main.go               fleet state machine + runInstance (all uplinks)
    internal/link         seqpacket fd <-> gVisor channel.Endpoint bridge
    internal/vswitch      L2 switch: MAC learning, flood, inline fast path
    internal/nat          netstack NAT (NAT44, NAPT66, ICMP, DNS)
    internal/dhcp         frame-layer DHCP server + lease pool w/ restore
    internal/portfwd      hostport -> guest port forwarding
    internal/tap          host tap uplink (TUNSETIFF)
    internal/unixseq      SOCK_SEQPACKET bind/connect/accept helpers
    internal/elect        flock seat + heartbeat/lease-gossip beacons
    e2e_test.go           full fleet failover test, no UML kernel needed

## Roles and failover

Every instance runs the same binary. On startup it either

- joins an existing hub (`TryConnect`) and becomes a **peer**, or
- takes the **hub seat** — an exclusive `flock` on `<socket>.lock` —
  and binds the switch socket.

Peers watch hub **heartbeats** (L2 ethertype 0x88B5, magic `VDHB`):
three missed 1-second beats mean the hub is wedged, and the peer
re-enters discovery. Because the seat is a kernel flock, a dead hub
releases it even on SIGKILL; the first peer to win the lock rebinds the
socket and promotes. Promotion is crash-stop safe and split-brain
impossible: only the seat holder ever binds the socket.

Failback is **sticky**: a returning old hub finds the seat taken and
joins as a plain peer. Hub heartbeats also carry the DHCP lease table
(≤32 leases per frame), so a promoted hub keeps issuing the addresses
the old hub had handed out.

## Uplinks (`uplink:` in config.yaml)

- `slirp` — netstack NAT44 + DHCP + portfwd (default, C-binary parity)
- `tap:NAME` — raw host tap device (needs cap_net_admin; see below)
- `none` — isolated guest LAN

The uplink is per-instance; guests on the same switch see each other
east-west regardless of their hubs' uplinks.

## Guest MAC persistence

UML derives the vec0 MAC from the kernel cmdline, so the launcher
generates a random local MAC once per rootfs image (sidecar
`<image>.mac`, mode 0600) and injects `mac=` on every boot. DHCP leases
and switch learning stay stable across reboots.

## Engine selection (M6)

`config.yaml` `engine: netstack|slirp` — the launcher symlinks the
chosen binary onto `vde_plug` (the kernel execs that exact name from
PATH). `UML_ENGINE=slirp ./boot` overrides per boot for A/B testing.
Both engines speak the same vde protocol and can serve different VMs
side by side.

## Testing

    go vet ./... && go test ./...

The e2e test builds the binary and drives three fake guests over
socketpairs (fd 3): first node takes the seat, ARP flows, the hub is
killed, a survivor promotes, the old hub returns as a peer. CI runs it
plus a static build matrix (`.github/workflows/vdeplug_go.yml`).

## Deploy note

The tap uplink needs `cap_net_admin` on the helper binary; re-apply
after every install (`sudo setcap cap_net_admin+ep vdeplug-netstack`).
