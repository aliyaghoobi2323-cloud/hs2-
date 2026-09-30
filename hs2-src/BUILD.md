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

`engine/autopilot.go` is a pure controller (no locks, sockets or clock) that
decides how many links should be **serving** — taking new connections —
between `min_links` and `max_links` (installer default 2–32). Links beyond that
are **retiring**: no new connections, closed once empty. Each 2 s health tick
`LinkManager.sampleHealth` feeds it per-link throughput, active flows (a
per-stream rate EWMA; idle connections never count) and pressure (the link's
sender blocked by the network: the edge's own writer + TCP_INFO for uploads,
the exit's for downloads, reported over the optional `kindStats` stream).

- **Floor:** `ceil(active flows / per_link)`.
- **Grow:** only when pressed links leave no free link for new flows; a probe
  adds ~25% (50% while probes keep succeeding), arms once the links exist, and
  is kept only if the new links' traffic *added* to the total. A full path
  fails the probe (exponential backoff, capped at 8 min); a probe that no new
  flow reached keeps its links as spares.
- **Shrink:** after 60 s below target, step down every 30 s toward what the
  last minute needed (active flows, pressure, peak throughput at 70% of the
  measured per-link capacity), capped by the links the active flows can use.
  A shrink that immediately leaves links short is undone by un-retiring
  (no dials) and held (10 min, doubling). Nothing that holds the size up is
  latched, so an idle pool always returns to `min_links`.

`engine/linkmanager.go` is the actuator: un-retire before dialing, retire the
links that will empty soonest, close empty retiring links (asynchronously, 2
per tick), and close connections idle for `drain_idle_sec` on retiring links.

In reverse, only the exit dials, so the edge publishes its serving target down
a pool-control stream (`kindPool`); the exit dials only while it has fewer
slots than the target and retires a slot whose link the edge closed while it
holds more (`exit_pool.go`). A link that arrives while the edge already has its
target is born retiring. Close guards keep an exit that has not yet learned a
lower target, redials what is retired (its `min_links` above the edge's
target) or predates pool control from churning links. All additions are
backward compatible: an older peer closes the unknown streams and the pool
falls back to what that peer supports.

Tests: a flow-level simulator of the whole loop (`engine/autopilot_sim_test.go`,
scenarios in `engine/autopilot_test.go`), actuator and wire unit tests
(`engine/pool_v2_test.go`, `engine/stats_test.go`), and real-TLS integration
tests including mixed versions (`engine/stream_reverse_test.go`,
`engine/stream_v2_test.go`).

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
