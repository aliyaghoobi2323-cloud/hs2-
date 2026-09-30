# Building hs2 from source

Requires Go 1.27+.

```bash
go mod tidy      # fetches deps from the Go proxy
go build -trimpath -ldflags="-s -w" -o hs2-linux-amd64 ./cmd/hs2
```

Run tests (the race detector is worth the extra time):
```bash
go test -race ./...
```

## Layout
- `core/`       — crypto core (Noise IKpsk2 handshake, frame format, replay window)
- `tlscarrier/` — TLS carrier: real cert, channel-bound mutual auth, probe resistance, socket tuning
- `engine/`     — stream core (link pool, TCP/UDP forwarding, hs0 side channel), legacy engine for noise/reality
- `reality/`    — experimental Reality-style carrier (not used by default)
- `fec/`        — adaptive interleaved Reed-Solomon FEC (standalone; see `fec/README.md`)
- `obfs/`       — traffic shaping (length/timing), used by the noise/reality carriers
- `tun/`        — Linux TUN device
- `tune/`       — RAM/CPU-aware kernel tuning applied at every start (`hs2 tune`)
- `cmd/hs2/`    — the binary (run, keygen, version, check, status, tune, config)
- `install/`    — installer script
- `lab/`        — network emulator and benchmark harness (see below)

## Commands

- `hs2 run -c cfg`        — run the tunnel
- `hs2 check -c cfg`      — validate a config (used by the tunnel manager on edit)
- `hs2 status -c cfg [--watch]` — live dashboard: link pattern, throughput, cert
- `hs2 tune -c cfg [--apply]`   — show (and optionally apply) the kernel tuning plan
- `hs2 config -c cfg set|get|unset <key> [val]` — safe JSON-aware config edits
- `hs2 keygen`, `hs2 version`

The daemon also reacts to signals: SIGHUP hot-reloads the TLS certificate
(`systemctl reload hs2`, the certbot deploy-hook) with no dropped connections.

## Adaptive link pool (autopilot)

`engine/autopilot.go` is a pure controller that sizes the parallel-link pool
between `min_links` and `max_links` (installer default 2–32). Each health tick
it is fed a sample (users, live links, aggregate goodput, saturation) and
returns the desired link count; `engine/linkmanager.go` (direct edge) and
`engine/exit_pool.go` (reverse exit) are the actuators. Two pressures set the
size: the connection floor `ceil(users/per_link)`, and a throughput probe that
grows a link only while the links are saturated and an added link raises
goodput, settling at the path ceiling and re-probing as demand/the path change.
The pool starts warm (`warmStartLinks`) so a startup burst spreads at once.

In reverse, only the exit dials, so the edge sends its target down a
pool-control stream (`kindPool`); the exit's dial pool follows it. Additive and
backward-compatible — an old peer just closes the stream. Growing is immediate.
Shrinking never cuts a connection: the exit cannot see which of its links carry
users, so it never closes one itself; the edge closes one link with **no**
users, and the exit retires that slot instead of redialing it. Both ends start
at the same warm size, so a restart causes no churn.

The controller is covered by a per-connection-throttling simulator in
`engine/autopilot_test.go`; the reverse target propagation end-to-end in
`engine/stream_reverse_test.go` (`TestReverseExitPoolFollowsEdgeTarget`).

## Kernel tuning (`tune/`)

`hs2 tune` detects RAM and cores, picks a low/medium/high profile (buffers,
backlogs, somaxconn), a congestion control (default bbr) and a qdisc (default
fq_codel, with fallback), plus fixed BBR-/multi-IP-friendly sysctls. It is
applied on every `hs2 run` (single source of truth — no static sysctl file) and
overridable via the config `"tuning"` section (`mode` auto|manual|off). Unit
tests in `tune/tune_test.go`. Set `HS2_NO_TUNE=1` to skip applying (the lab does
this so experiments control tuning via `HS2_TUNE_*`).

## Carriers (config `"carrier"`)

All three TLS carriers share one data path, the **stream core**
(`engine/stream*.go`): a user's TCP connection ends on the Iran server and its
bytes ride an smux stream over a pool of TLS links; the kharej server opens
its own connection to the panel. There is no TCP-inside-TCP anywhere.

| carrier  | links                          | hs0 (10.77.0.1/2)      |
|----------|--------------------------------|------------------------|
| `mtcp`   | adaptive pool `min_links`..`max_links` (autopilot) | no |
| `l3mtcp` | adaptive pool                  | yes, as a side channel |
| `tls`    | exactly 1                      | yes, as a side channel |

`"udp": true` (Iran config) also forwards UDP on `forward_ports`; each client
address gets its own stream. The side channel carries hs0 packets (ping,
non-TCP traffic) on one stream per link with a 60 ms queue-time limit.

`noise` and `reality` are older experimental carriers on the packet engine
(`engine/engine.go`) and are not offered by the installer.

## Wire compatibility

v3 is **not** compatible with v2: links authenticate with a channel-bound,
mutual scheme (`tlscarrier/auth.go`) and streams carry a kind byte. Upgrade
both servers. A v3 kharej logs a clear message when a v2 Iran server
connects; a v3 Iran server reports that the kharej server is too old.

## Security model (TLS carriers)

- TLS 1.3 with a real certificate; the client uses a Chrome fingerprint (uTLS).
- The client does **not** validate the certificate chain. Instead both sides
  prove the shared key bound to the TLS exporter of *this* session:
  client tag = HMAC-BLAKE2s(key, nonce, minute, EKM), server tag =
  HMAC-BLAKE2s(key, nonce, EKM). An interceptor has a different EKM on each
  side, so it can neither obtain a link nor impersonate the server; the
  client sends nothing until the server's tag verifies. See `TestMITMRejected`.
- Auth records are padded to HTTP-request/response sizes. The server reads
  only the first TLS record, so a probe gets the cover website at once.
- Link sockets use BBR, `TCP_NOTSENT_LOWAT` and `TCP_USER_TIMEOUT`
  (`tlscarrier/tune_linux.go`), independent of the system defaults.

The Noise core is not layered inside TLS: once TLS is channel-bound and
mutually authenticated, a second AEAD layer adds CPU cost and no security.

## Lab

`lab/` reproduces a long-haul, lossy, throttled path on one Linux machine
(root, iproute2, ethtool, openssl), since stock kernels in containers often
lack `sch_netem`:

- `lab/netem`: userspace L2 bridge between two network namespaces with
  rate, bounded queue, delay, loss and a per-connection policer (DPI-style
  throttling of each single flow).
- `lab/probe`: plays the panel, and on the user side measures bulk
  throughput together with echo latency, new-connection time and UDP echo.
- `lab/run.sh`: one experiment (real hs2 binaries on both sides), one JSON line.
- `lab/sweep.py` / `lab/analyze.py`: run a plan of experiments in parallel
  and summarise medians.

```bash
go build -o /tmp/hs2 ./cmd/hs2
BIN=/tmp/hs2 MODE=mtcp RATE=50mbit DELAY=40ms LOSS=0.005 FLOWRATE=10mbit lab/run.sh
```

Data-path tuning can be overridden for experiments with `HS2_TUNE_NOTSENT`,
`HS2_TUNE_SMUX_FRAME`, `HS2_TUNE_SMUX_STREAMBUF`, `HS2_TUNE_SMUX_SESSBUF` and
`HS2_TUNE_CC`; the defaults in the code are the ones the sweeps chose.
