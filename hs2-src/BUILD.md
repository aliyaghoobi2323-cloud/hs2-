# Building hs2 from source

Requires Go 1.27+.

```bash
go mod tidy      # fetches deps from the Go proxy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -ldflags="-s -w" -o hs2-linux-amd64 ./cmd/hs2
```

**`CGO_ENABLED=0` is required, not optional.** The published `hs2-linux-amd64`
is one static binary that must run on every target server — old-glibc CentOS 7,
musl Alpine, whatever a user has. With cgo enabled (the Go default when a C
compiler is present) the binary is dynamically linked against the build host's
libc and fails on those servers with `GLIBC_2.xx not found`. Building without
the flag also silently produces a ~45 KB-larger, non-reproducible binary. Always
pin `CGO_ENABLED=0`.

After building, regenerate the published hash so the installer's fail-closed
verification keeps working, and keep the two copies of the installer identical:
```bash
sha256sum hs2-linux-amd64 | cut -d' ' -f1 > hs2-linux-amd64.sha256
cmp install.sh install/install.sh   # must be byte-identical (release test checks this)
```

### Version stamp

`hs2 version` prints the capability line followed by a build stamp, e.g.
`… mutual)  [build 3ca1c2d19894 2026-10-01]`. The stamp is read at runtime from
the binary's embedded Git metadata (`vcs.revision` / `vcs.time` / `vcs.modified`
— Go records these automatically when building inside the checkout, and
`-trimpath` keeps them), so **no extra build flag is needed** to carry it. Build
from a clean, committed tree for a clean stamp; a dirty tree is marked with a
trailing `+`. An optional human label can be added with
`-ldflags "-X main.buildTag=v3.1"`. The stamp is metadata only: two binaries
with different stamps are fully wire-compatible, so it never gates a connection —
it only lets an operator confirm both ends of a tunnel (and the server vs. the
published build) run the same code.

Run tests (the race detector is worth the extra time; it needs cgo, so run it
with the default `CGO_ENABLED=1` rather than the release flag above):
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
between `min_links` and the ceiling — `max_links`, or with `max_links: 0`
(new installs) an auto ceiling from the server's RAM/cores: one link per 48 MB
of RAM (after six times the smux window pool, 1/128 of RAM, 16–128 MB) up to
300, never below the 32 / 48 / 64 profile value (1 core: the
profile value; 2–3 cores: at most 128; every carrier alike), see
`tune.RecommendedMaxLinks`; absent = the historical 32. Links beyond that
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
- **Full path:** once growth was found not to help, a later gain must last a
  minute before it is kept, and a path found full again at more links with
  no more throughput goes back to the smaller size — on a full path every
  link reads as pressed, so nothing else would take back links a lucky
  probe added (tested for 8 h with ±20% cross-traffic noise).
- **Shrink:** after 60 s below target, step down every 30 s toward what the
  last minute needed (active flows, pressure, peak throughput at 70% of the
  measured per-link capacity), capped by the links the active flows can use.
  A shrink that immediately leaves links short is undone by un-retiring
  (no dials) and held (10 min, doubling). Nothing that holds the size up is
  latched, so an idle pool always returns to `min_links`.

`engine/linkmanager.go` is the actuator: un-retire before dialing, retire the
links that will empty soonest, close empty retiring links (asynchronously, 2
per tick), and close connections idle for `drain_idle_sec` on retiring links.
A link that has received nothing for 12 s (not even the other side's
keepalive or a control reply) is suspect: no new users and not counted as
serving, so a replacement is used at once.

In reverse, only the exit dials, so the edge publishes its serving target down
a pool-control stream (`kindPool`); the exit dials only while it has fewer
slots than the target and retires a slot whose link the edge closed while it
holds more (`exit_pool.go`). A link that arrives while the edge already has its
target is born retiring (and kept 30 s first: it may be replacing a link that
died on the exit's side before this side noticed). Close guards keep an exit that has not yet learned a
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
`HS2_TUNE_SMUX_FRAME`, `HS2_TUNE_SMUX_SESSBUF` (8 MiB),
`HS2_TUNE_SMUX_MINSTREAMBUF` (64 KiB), `HS2_TUNE_SMUX_LAGTARGET` (128 KiB;
0 = fixed stream window), `HS2_TUNE_SENDGATE` (bytes/s under which a link's
socket is held to about half its unsent cap and a burst, on Linux 5.10+; 0 =
off) and `HS2_TUNE_CC`; the defaults in the code are the ones the sweeps
chose. Each value read is logged at start (`tuning: NAME=value`). These are
read once at start: on an installed server set them with `systemctl edit
<unit>` (`[Service]` / `Environment=NAME=value`) and a restart.

The stream windows, the window core (Phase Z5; `engine/wcore_run.go`, which
lets a fast download's window grow past the need only as far as its path
carries) and the exit's pacing net (`engine/pacenet.go`):

| Variable | Default | Meaning |
|---|---|---|
| `HS2_TUNE_SMUX_NEEDBUF` | 2 MiB (2097152) | how far a stream's window grows on its reader keeping up alone; past it only under the window core's ceiling, and only from the window pool. A value outside [`HS2_TUNE_SMUX_MINSTREAMBUF`, `HS2_TUNE_SMUX_STREAMBUF`) turns windows past it off: a fixed window of it (2 MiB for 0) within `HS2_TUNE_SMUX_SESSBUF`, logged at start |
| `HS2_TUNE_SMUX_STREAMBUF` | 16 MiB | the largest stream window (what the window core may grow one to) |
| `HS2_TUNE_SMUX_POOL` | 1/128 of the RAM, 16–128 MiB (32 MiB if the RAM is unknown) | the process's window pool, bytes: what windows past the need hold, and what links whose buffer ran full borrow; 0 = no window past `HS2_TUNE_SMUX_NEEDBUF` and no buffer borrowed |
| `HS2_TUNE_SMUX_COVER` | 1 | a stream whose window holds it back (its data pauses at the limit while its reader keeps up) gets the need plus a step, 3 MiB, from the window pool: an update leaves each step read, so at the need the sender has only the need less a step in flight (1 MiB per round trip: the half-window lock), and with the cover the need, at one update per MiB as before; receiver side only, any sender works; a link's window core keeps the cover as its floor until it finds the path full, or while other cores to the same exit run, and then takes the covers (its floor the need as before); in OFF it takes them for a minute (or until it leaves OFF) once two of the exit's latest three records show its queue overflowing (resends with a queue past Δq: links to one exit that fill the path at the need, red team H2), and on an edge whose exit's records carry no TCP_INFO (an old exit) or stopped, since nothing there sees a policer the covers run past. Without a window core (`HS2_TUNE_WCORE=0`, or a link with none) nothing supervises the covers: in the model, links to one exit that fill the path at the need resent 0.5–3% with them (base 0–0.2%), and behind a per-connection policer under the need's rate a download resent 5–10% (base 0.2–0.3%). 1: on the edge's end only (downloads; Iran server); 2: on the exit's end too (uploads; lab only: the edge's sender has no pacing net and the exit no record to see a policer by); 0 keeps every window without the window core's ceiling at the need, as before; so does a window pool of 0 or one with nothing left. 0 leaves smux's windows and the window core as before the covers to the byte (smux `TestCoverOffIsBase`, engine `TestWcoreModelCoverOff`). The covers hold at most a quarter of the window pool in all (the rest stays for the windows the cores grow and for links whose buffer ran full); a link's end line says how often its streams were covered and lost the cover (and why), the lab trace has the same per evaluation (`cover`), and `hs2 status` the streams covered now (windows line) |
| `HS2_TUNE_WCORE` | 1 | 0 turns the window core off: no link gets one and every window stays within the need (or its cover, `HS2_TUNE_SMUX_COVER`), as before (the field safety switch; Iran server). Rolling the window work back takes both `HS2_TUNE_WCORE=0` and `HS2_TUNE_SMUX_COVER=0`: with the core off the covers still give windows past the need, and with the covers off the core still grows them |
| `HS2_TUNE_PACENET` | 1 | 0 turns the exit's pacing net off: no link socket's pacing is touched (the field safety switch; Kharej server) |
| `HS2_TUNE_PACENET_K` | 1.25 | the net's cap as a multiple of the socket's delivery rate |
| `HS2_TUNE_PACENET_LOSS` | 0.02 | the resend share (of the last second) that engages the net |
| `HS2_TUNE_WCORE_QTARGET` | 15 ms (exit side 10) | ms of standing queue the core keeps in HOLD |
| `HS2_TUNE_WCORE_PROBEQ` | 20 ms (exit side 15) | ms of queue one probe adds |
| `HS2_TUNE_WCORE_UPLOAD` | 0 | lab only: 1 gives the exit a core for its uploads too. Off until step 3 sends the exit a record of the edge's socket: without one that core cannot see a per-connection policer (the edge's download core likewise starts only once the exit's 96-byte stats record arrives, so an old exit leaves the windows as without the core) |
| `HS2_TUNE_WCORE_STAGE` | 1 | lab only: 2 lets ceilings go below the need once the core found the path full (step 2, not yet measured) |
| `HS2_TUNE_WCORE_TRACE` | none | lab only: a file that gets one JSON line per link per evaluation |
| `HS2_TUNE_WCORE_FIXEDWIN` | 0 | lab only: bytes; every large stream held at that window instead of the core's decision, still traced |
| `HS2_TUNE_WCORE_PACECAP` | 0 (off) | lab experiment: each end caps its link socket's pacing (`SO_MAX_PACING_RATE`) at x times its own delivery rate, and, while the path shows no queue, never under what the peer's windows allow per round trip; takes the pacing net's place while on |
| `HS2_TUNE_WCORE_BURST` | 0 (off) | lab experiment: the budget held where the burst after the sender's PROBE_RTT, that share of the window, fits the queue learned |
