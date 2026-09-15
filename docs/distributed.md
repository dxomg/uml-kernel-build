# Distributed vdeplug-go over WebSocket

Connecting machines that have no shared L2 or filesystem — typically
across the internet — to one netstack switch. The switch's hub runs on
one machine; every other machine bridges the remote switch to a local
unix socket and attaches guests to it as peers.

```
[UML guest] → vde_plug (hub, holds the seat) → vdews serve :5001
                                                     ↑ WSS (TLS at the proxy)
                                        https://quangdz.example.com/vde
                                                     ↓
[UML guest / fakeguest] ← vde_plug (peer) ← vdews connect (peer machine)
```

Design points worth knowing before you start:

- One WebSocket connection per peer wire. Frames ride as binary
  messages, so packet boundaries survive end-to-end.
- The bridge turns the remote switch into a **local unix socket**;
  the helper on the peer side needs no new code paths — it reads
  `socket_file_location` and connects as an ordinary peer.
- The helper's own retry loop drives reconnects: when the hub reboots
  or the internet blips, the bridge re-dials and the peer rejoins.
- **The seat belongs to the hub's machine** (flock + socket file live
  there). Failover promotes among instances on that machine only;
  remote peers keep retrying while the hub's machine is down.

Binaries needed on both ends: `vdews` (the bridge), `vde_plug`
(the helper; the CI artifact ships it as `vdeplug-go-x86_64`), and
`helpers/fakeguest` if you want a guest without UML.

## Hub side

The hub is a normal local setup plus one bridge process:

```sh
./boot                                   # first instance holds the seat
vdews -mode serve -listen :5001 -upstream /tmp/vde.socket -token <SECRET>
```

Publish it through any WebSocket-capable reverse proxy. Caddy
(handles the upgrade headers automatically):

```
quangdz.example.com {
    reverse_proxy /vde <HUB-LAN-IP>:5001
}
```

nginx needs the headers spelled out:

```nginx
location /vde {
    proxy_pass http://<HUB-LAN-IP>:5001;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 3600s;
}
```

## Peer side

```sh
# 1. Binaries (from CI, no local build needed):
gh run download <run-id> -R <owner>/<repo> -n vdeplug-go-x86_64 -D /opt/vde

# 2. Bridge: pull the remote switch to a local socket.
#    Keep stdin out of the picture when backgrounding by hand:
vdews -mode connect -downstream /tmp/vde-remote.sock \
      -url wss://quangdz.example.com/vde -token <SECRET>

# 3. Helper config, placed next to the vde_plug binary:
cat > /opt/vde/config.yaml <<'EOF'
switch: true
socket_file_location: /tmp/vde-remote.sock
uplink: none
EOF

# 4a. A guest without UML — fakeguest answers ARP and ICMP like a NIC:
/opt/vde/fakeguest -bin /opt/vde/vde_plug -ip 10.0.2.42 \
                   -mac 02:de:ad:be:ef:42 -ping 10.0.2.15

# 4b. Or a real UML guest: boot as usual — the helper reads the
#     adjacent config.yaml and joins the remote switch as a peer.
```

`10.0.2.15` above is the hub guest's address; any address on the
switch works.

## Verification

| Where | What to look for |
|---|---|
| Peer bridge log | `bridge up: wss://...` after dialing |
| Peer helper log | `peer of /tmp/vde-remote.sock` |
| Hub bridge log | accepted connection (no rejects) |
| Hub VM console | `switch: peer 0 joined` |
| Ping across | `fakeguest -ping 10.0.2.15` → 0% loss, ~1×RTT |

Measured on the reference setup (hub behind a reverse proxy, peer on
another continent): ICMP both directions between a real UML guest and
a `fakeguest` peer, 0% loss at one internet RTT (~225 ms).

## Running it for keeps

Hand-run processes die with your shell; use systemd on both ends.

Hub machine — `/etc/systemd/system/vdews.service`:

```ini
[Unit]
Description=vde switch WebSocket bridge (hub)
After=network.target

[Service]
ExecStart=/opt/vde/vdews -mode serve -listen :5001 -upstream /tmp/vde.socket -token SECRET
Restart=always

[Install]
WantedBy=multi-user.target
```

Peer machine:

```ini
[Unit]
Description=vde switch WebSocket bridge (peer)
After=network.target

[Service]
ExecStart=/opt/vde/vdews -mode connect -downstream /tmp/vde-remote.sock -url wss://quangdz.example.com/vde -token SECRET
Restart=always

[Install]
WantedBy=multi-user.target
```

The helper itself is started by `./boot` per guest; restart it and it
will rejoin the switch on its own.

## Gotchas

- **Token in the query string.** `-token` rides the URL
  (`...?token=...`), so it shows in `ps` output on the peer. Fine for
  test setups; for production, move auth to a header (`DialConfig` in
  `golang.org/x/net/websocket` supports custom headers).
- **Don't delete or recreate the downstream socket file while the
  bridge runs.** The bridge has a watchdog that rebinds if the file is
  replaced, but a clean restart of `vdews` is more predictable.
- **Proxy must pass WebSocket upgrades** (automatic in Caddy, two
  header lines in nginx) and needs a generous read timeout — the
  heartbeat is 1 Hz but the traffic is sparse.
- **Seat topology.** Failover promotes within the hub's machine only.
  True multi-machine failover (distributed locking or a multi-peer
  hub mode) is future work.
