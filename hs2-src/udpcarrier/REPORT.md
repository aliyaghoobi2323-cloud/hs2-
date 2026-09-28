# UDP transport (Noise + adaptive FEC) — build & lab report

> **All numbers in this report come from the in-process network simulator
> (`lab/netsim`) and the network-namespace lab (`lab/netem`), not from a real
> server path.** They are reproducible with `go test ./udpcarrier/` and the lab
> scripts. The engineering point of this transport is **latency and jitter
> stability under bursty loss**, not higher raw throughput — at 26 %+ loss the
> FEC redundancy costs real bandwidth (often 100 %+). Keep that in mind reading
> the throughput figures.

## 1. What was built, and how it plugs into the existing packages

The work is assembly of two pieces that already existed on
`claude/udp-carrier-noise-auth-tzn37h` (now in this branch): the `fec` package
(adaptive, interleaved forward error correction) and the datagram additions to
`core` (exporter/binding, `StaticFromSeed`, `SealDatagram`/`OpenDatagram`, the
fast replay window). Nothing new was invented in cryptography or FEC.

New code:

| File | Role |
|------|------|
| `udpcarrier/carrier.go` | The `Conn` type. Implements the engine's `Carrier` interface (`SendFrame`/`ReadFrame`/`Close`) so the engine drives it exactly like the TCP and reality carriers. Send: `core.SealDatagram` → `fec.Encoder.Encode` → pacer → `net.PacketConn`. Receive: `fec.Decoder.Decode` → `core.OpenDatagram` (replay-checked) → deliver. |
| `udpcarrier/dial.go` | Initiator: Noise IKpsk2 handshake over datagrams (with message-1 retransmission), then the key-confirmation exchange. Keys come from the tunnel's one shared secret via `core.StaticFromSeed`. |
| `udpcarrier/listen.go` | Responder: one shared `net.UDPConn`, demultiplexed by source address. Runs the responder handshake once per address (caching message 2 so a retransmitted message 1 is answered without re-running the replay-protected handshake), pins the initiator's static key, answers probes on the same port. |
| `udpcarrier/auth.go` | Post-handshake mutual key confirmation, the datagram analogue of `tlscarrier`'s channel binding: an HMAC over the session `Exporter` and the Noise transcript `Binding`, keyed by the shared secret. |
| `udpcarrier/rate.go` | Bandwidth/RTT rate control (BBR-style), **never loss-based**: windowed-max delivered bandwidth, windowed-min RTT, exponential startup, then pace at `btlBw` with a probe/drain gain cycle. |
| `udpcarrier/pacer.go` | Token-bucket pacer draining shards to the socket at the modelled rate, with a shallow queue (backpressure, not tail-drop) and **parity prioritised** ahead of data so parity beats the decoder's recovery window. |
| `udpcarrier/probe.go` | The independent, off-data-path reachability/loss probe: a tiny authenticated request/echo on the same UDP port, gated by the shared secret (not an open reflector). |
| `udpcarrier/wire.go` | Datagram framing (explicit sequence prefix), the feedback record, and the per-datagram wire-sequence used to measure loss accurately. |
| `engine/carrier_udp.go` | Thin `CarrierDialer`/`CarrierListener` adapters, plus the **auto** transport: probe UDP, use it when reachable with FEC-recoverable loss, otherwise silently return the TCP (noise) carrier. `autoListener` serves both UDP and TCP on the same port, independently. |
| `engine/constructors.go` | `NewUDPDialer`/`NewUDPListener`/`NewAutoDialer`/`NewAutoListener`. |
| `cmd/hs2/main.go` | `carrier: "udp"` and `carrier: "auto"` config values; UDP MTU defaulted to 1280 so a sealed, FEC-wrapped packet fits a 1500-byte path. |
| `install/install.sh` | Transport menu (`auto` / `udp` / `tcp`), encoded in the `hs2://` link so both ends agree; UDP-port validation. |
| `lab/netsim/` | In-process UDP network simulator (bursty Gilbert-Elliott loss, jitter, bottleneck rate + tail-drop) — the lab this environment can run without `netem`/root. |
| `lab/netem/main.go` | Added time-based Gilbert-Elliott bursty loss (`-burstloss/-goodms/-badms`) and `-dropudp` for the isolation test. |

### Data path in one line

```
send:  IP pkt → core.SealDatagram(seq,ct) → [seq|ct] → fec.Encode → tag+wireseq → paced UDP send
recv:  UDP → tag → fec.Decode → [seq|ct] → core.OpenDatagram (AEAD + replay) → IP pkt
control (feedback / auth / keepalive): sealed but sent raw (no FEC, no pacing) so timing is clean
```

Data rides FEC; small control frames are sent raw and unpaced, because feedback
that had been delayed and reordered by FEC recovery made the rate/RTT estimates
useless (this was a real bug found and fixed during the build).

## 2. Security review

All of the following are automated tests that pass under `go test -race`.

| Property | Test | Result |
|----------|------|--------|
| **Anti-MITM on UDP** (mirrors `tlscarrier`'s `TestMITMRejected`) | `udpcarrier.TestMITMRejected` | A real relay that terminates the carrier on each side with its own (wrong) secret is rejected: the client (which pins the server key derived from the true secret) refuses the relay's handshake, and the true server grants no carrier. A direct dial then succeeds. Pinned-key Noise IKpsk2 rejects it at the handshake; the exporter/binding confirmation is the second layer. |
| **Wrong key** | `udpcarrier.TestWrongKeyRejected` | A client with a different shared secret cannot open a carrier. |
| **Replay** | `udpcarrier.TestReplayRejected` | A relay that duplicates every datagram: 200/200 payloads delivered exactly once; every duplicate dropped (decoder de-dup + `core` replay window). |
| **Isolation / silent fallback (selection)** | `engine.TestProbeSeesBlockedUDP`, `engine.TestAutoFallsBackToTCP` | A fully black-holed UDP path is reported unreachable by the independent probe; the auto dialer then returns the TCP carrier, which carries frames. UDP and TCP are separate sockets and code paths — a UDP failure never touches TCP. |

**Real-kernel confirmation of fallback:** in the `netem` lab, with all UDP
blocked, the Iran side logs, repeatedly, `transport: UDP unusable
(reachable=false loss 100%); using TCP` — the selector detects the blackhole on
a real kernel and switches. See *Limitations* for what could not be finished
end-to-end in this sandbox.

## 3. Performance (lab/netsim, in-process, reproducible)

One-way bulk transfer, 20 Mbit/s bottleneck, 25 ms one-way delay, 1 ms jitter.
Loss is time-based Gilbert-Elliott (genuine bursts). Representative run:

| Scenario | Wire loss | Goodput | Residual loss (after FEC) | FEC overhead | Latency p50 / p95 | Jitter (p95−p50) |
|----------|-----------|---------|---------------------------|--------------|-------------------|-------------------|
| **Bursty ~26 %** (real-path profile: 12 ms bursts of 88 %) | 26.7 % | 6.4 Mbit/s | **3–7 %** (run-to-run) | 124 % | 70 / 140 ms | ~70 ms |
| **Adaptive 5 %→50 %** (loss jumps mid-run) | ~20 % avg | 8.8 Mbit/s | 6.4 % | 54 % → higher | 91 / 195 ms | ~104 ms |
| **Low loss ~1.4 %** | 1.4 % | 14.3 Mbit/s | 0.09 % | 23 % | 69 / 147 ms | ~78 ms |
| **High loss ~41 %** | 41 % | 5.2 Mbit/s | 16.8 % | 146 % | 68 / 148 ms | ~80 ms |

Reading these:

- **FEC recovers most bursty loss**: at ~26 % wire loss the inner protocol sees
  ~3–7 % residual, at the cost of ~120 % parity overhead. This matches the
  `fec` package's own sweep (`HS2_FEC_SWEEP=1 go test -run Sweep ./fec`), which
  puts the achievable residual for this profile at ~3–5 %.
- **The overhead is adaptive**: 23 % at 1.4 % loss → 146 % at 41 % loss. The
  redundancy tracks the loss, it is not a fixed tax.
- **Latency and jitter stay flat** across every scenario — p50 near the 50 ms
  base RTT, jitter under ~110 ms — even as loss swings from 1 % to 50 %. This is
  the transport's reason to exist.
- **Above ~40 % loss it is marginal** (16.8 % residual at 41 %). That is why the
  auto selector's threshold to prefer UDP is 45 % loss; past that, TCP (no
  redundancy overhead) is the better choice and auto falls back.

## 4. UDP vs TCP (lab/netem, real kernel, network namespaces)

Same path for all: 20 Mbit/s, 25 ms delay, ~26 % time-based bursty loss
(88 % for 12 ms bursts). The probe carries **inner TCP** over the tunnel and
measures bulk throughput plus a long-lived interactive "echo" connection.
Single 15 s runs — **high variance**, treat as directional, not precise:

| Transport | Bulk goodput | Interactive echo (latency-sensitive) |
|-----------|--------------|--------------------------------------|
| `udp` (Noise+FEC) | ~1.9 Mbit/s | **all echoes delivered, 0 lost**, p50 ~570 ms |
| `auto` | ~1.1 Mbit/s | mostly delivered (chose UDP) |
| `mtcp` (TLS multi-link, 8–16 links) | ~2.9 Mbit/s | **interactive connection died** (echo_lost, 0 samples) |
| `tls` (single TLS link) | ~1.2 Mbit/s | delivered but p95 ~2.1 s tail |

The multi-link TCP carrier gets the **highest bulk throughput** (parallel links,
no FEC tax) — consistent with the honest expectation that UDP+FEC is not about
raw speed. But under the same bursty loss the TCP transports' single
interactive connection either dies (`mtcp`) or shows multi-second tail latency
(`tls`), while UDP+FEC keeps interactive traffic alive with no loss. That is the
stability trade the transport is for.

## 5. Limitations and work not done (honest)

- **The end-to-end isolation/fallback data-path run could not be completed in
  this sandbox.** The selector logic is proven (Go tests + the real-kernel
  "UDP unusable → using TCP" log), but driving traffic over the *TCP fallback*
  in `netem` did not complete here: this environment has no `ping`, adding any
  `iptables` rule breaks veth TCP forwarding, and a minimal raw-TCP-through-
  `netem` harness was itself flaky. The `mtcp`/`tls`/`udp`/`auto` transports
  each carried real inner-TCP traffic end-to-end, so the tunnel data path works;
  the specific "block UDP mid-stream, watch it ride TCP" run is validated only
  at the Go-test level, not end-to-end on the kernel.
- **Fallback is not instant.** When a live UDP carrier is blocked mid-session,
  the engine notices via the 15 s liveness timeout, then re-dials → re-probes →
  TCP. The inner TCP survives the pause (it is a pause, not a reset, because the
  TUN is persistent), but there is a ~15 s hiccup. Continuous probing to switch
  faster is future work.
- **The UDP/auto transport is a TUN IP link, not a port-forwarder.** Unlike
  `mtcp`, it does not do smux per-port forwarding; it bridges IP over `hs0`
  (like the `l3mtcp`/`noise` carriers). The installer sets it up that way and
  says so; users route panel traffic over `hs0`.
- **The per-datagram wire sequence and the FEC shard header are in clear.** They
  are needed for loss measurement and FEC, but they are a fingerprint an
  observer could use; the datagram transport is less traffic-shaped than the
  reality/TLS carriers. It is intended for paths where UDP+FEC throughput
  matters, not for the strongest DPI resistance.
- **Rate control is a pragmatic BBR-lite.** It reaches path capacity and keeps
  the queue small in the lab, but it is not a hardened congestion controller and
  was not tested against competing flows or a shared bottleneck.
- **Residual loss at 26 % is ~3–7 %, not near-zero**, and it is bandwidth-
  expensive. On paths where TCP already works fine (low loss), UDP+FEC only adds
  overhead — which is exactly why the default transport is `auto`.

## 6. How to reproduce

```
# unit + security + selection tests, race-clean
go test ./udpcarrier/ ./engine/ ./core/ ./fec/ -race

# lab performance numbers (in-process simulator)
go test ./udpcarrier/ -run TestLab -v

# fec parameter sweep this build's config is drawn from
HS2_FEC_SWEEP=1 go test ./fec -run Sweep -v

# real-kernel end-to-end (needs root + netns + veth; see scratch scripts):
#   MODE=udp|auto|mtcp|tls  BURSTLOSS=0.88 BADMS=12 GOODMS=30 RATE=20mbit  lab
```

---

# Direct vs Reverse direction (added)

The tunnel now supports two **directions**, chosen by the user in the installer,
independent of the transport (tcp/udp/auto) and of the roles (iran = user-facing
edge, kharej = panel/exit — these never change).

- **Direct** (default, classic): the iran edge **dials out** to the kharej. The
  first SYN originates in Iran.
- **Reverse**: the kharej **dials in** to the iran edge. The first SYN
  originates abroad — useful when outbound-from-Iran is filtered/throttled but
  inbound is not, or when the kharej is behind NAT/CDN.

```
DIRECT :  user ─▶ [iran: user ports] ══dial══▶ [kharej: listen] ─▶ panel
REVERSE:  user ─▶ [iran: user ports + listen] ◀══dial══ [kharej: dials] ─▶ panel
          (data path identical; only who initiates the connection flips)
```

## Design: role decoupled from direction

The key change is separating two axes that were fused:

- **Role** (fixed by deployment): `edge` = iran (opens user ports, originates
  smux streams), `exit` = kharej (connects to the panel, accepts smux streams).
- **Direction** (user choice): who dials the carrier.

The smux roles stay put in both directions — the edge is always the smux client,
the exit always the smux server. Only the TLS/UDP dial-vs-listen flips:

| | edge (iran) | exit (kharej) |
|---|---|---|
| direct  | TLS/UDP **client** + smux client | TLS/UDP **server** + smux server |
| reverse | TLS/UDP **server** + smux client | TLS/UDP **client** + smux server |

Implementation:
- Stream carriers (mtcp/l3mtcp/tls): `LinkManager` gained an accept mode
  (`AddLink`); the reverse edge accepts links from a `tlscarrier.Server` and
  feeds them to the pool (`engine/stream_reverse.go`), and the reverse exit
  dials a fixed pool of links and runs the smux server on each. `serveUserTCP`
  and `serveStream` are reused unchanged.
- Engine carriers (udp/auto/noise/reality): reverse is a pure dial/listen flip.
  `cmd` derives it as `dialing = (mode==edge) XOR reverse`, so the installer
  writes `mode` as the fixed role and a `reverse` flag.
- Reverse for TLS carriers moves the **certificate to the iran side** (it is the
  TLS server in reverse); the installer's reverse flow provisions it there.
- The `hs2://` link gained a `direction` field; the **listener side generates
  the link** (it knows its own endpoint) and the dialer pastes it — so in
  reverse the iran side generates the link and kharej pastes it.

## Testing

- `engine.TestReverseStreamTCPAndUDP`, `engine.TestReverseStreamRedials`: the
  reverse stream path end-to-end over real TCP (iran `net.Listen` TLS server +
  kharej `DialFrom`), TCP and UDP payloads, with `-race`. Pass.
- **Real-kernel end-to-end (network namespaces, kharej dials into the iran
  edge):** all three transports carried real inner traffic through the reverse
  tunnel, user → iran edge → tunnel → kharej → panel, with zero echo loss:

  | reverse transport | goodput | interactive echo p50 |
  |---|---|---|
  | udp (Noise+FEC)   | ~405 Mbit/s (unloaded veth) | ~10 ms |
  | auto              | ~367 Mbit/s | ~15 ms |
  | tcp (mtcp)        | ~8.5 Gbit/s | ~1.5 ms |

  (Unloaded loopback-class veth, so the throughput figures are just "the pipe
  is full"; the point is that the reverse path works end-to-end for every
  carrier.)
- Existing direct tests, all carriers, and the UDP suite: unchanged, `-race`
  clean.
- Installer: `bash -n` clean; generated config JSON validated for all four
  role×direction cases; `hs2://` link round-trips with the direction field.

## A note on the lab harness

Running the reverse tunnel through the userspace `lab/netem` bridge additionally
requires TX/RX **checksum offload** to be disabled on the veths
(`ethtool -K … tx off rx off`): netem re-injects raw frames via AF_PACKET, and
with offload on those frames carry incomplete UDP checksums that the receiver
drops. `lab/run.sh` already disables it; a hand-rolled bridge script must too, or
the handshake silently fails (no reply). This is a lab-harness detail, not a
property of the transport — the direct-veth runs above (no netem) and the Go
tests exercise the identical reverse code path with no such caveat.
