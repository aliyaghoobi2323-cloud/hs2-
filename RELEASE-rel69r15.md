# hs2 rel69r-rc15 — release notes

- Binary `hs2-linux-amd64`, sha256
  `13bca10da170753b82a32a2df56dedfea8e1496f8ea78b9f43ba3aef0e60087f`, `hs2 version` shows `[build rel69r-rc15]`
  (the exact production bytes, published unchanged, so the stamp keeps the release-candidate name).
- Previous GitHub release: **rel69r-rc14** (tag `v-rel69r14`, binary sha256 `2f3d34a2…`, `[build rel69r-rc14]`).
- `install.sh`: unchanged from rel69r-rc14 (the source branch did not touch the installer). Menu, live
  load-balancing screen, Alerts and watchdog screens, certificate hook unchanged. sha256 in `install.sh.sha256`.
- **Config files: no change needed.** Upgrade as usual (`bash install.sh upgrade`, Kharej first, then Iran), or
  with a zero-downtime handover if you turned ZDU on (README → *Zero-downtime upgrade*).
- No wire change that needs both sides at once: the Iran side hedges only toward an exit that announces the new
  capability (ext cap bit 18), so a rel69r-rc15 Iran next to a rel69r-rc14 / rc12 Kharej (or the other way round)
  behaves like rel69r-rc14. Upgrade Kharej first so the hedge is on as soon as Iran is upgraded.

## For the owner, in short

- **Light users (web, apps, messengers) stay fast when some links go bad.** A light connection now opens a second
  copy on another link at connect (the light-user hedge, K4M2); a download or burst drops to one copy at once, so
  heavy traffic is not doubled. Real WAN with a quarter of the links faulted and 2 % upload loss (8 pairs):
  light-user page time p50 -2.4 ms, p90 -1684 ms, p95 -893 ms, p99 -457 ms (pooled p90 2228 -> 541 ms), burst
  p90 -2549 ms; open failures -11 a run. Calls are not worse.
- **Calls recover faster after a link dies under them** (JOINRES): the Kharej side resends a call's stranded copies
  on a fresh sub the moment one joins, instead of waiting for the dead sub to time out.
- **A zero-downtime upgrade now ends on time at 24k connections** (PACEFIX): the old Kharej process used to exit
  1-2 s after its deadline with a hand-off timeout; now it exits on time.
- Cost: one small user column proven worse, game max +15 ms at a 12k ZDU exit (n=2); Kharej CPU about +5..10 % in some runs (not proven);
  the extra internal error-count lines on Kharej are ends of connections that had already finished.

## What changed since rel69r-rc14

Source = branch rel69r-rc15 `c043866f` (engine code = `93438864`; `c043866f` adds only README rows and lab notes;
later commits on that branch are lab notes). envdef unchanged (138 keys); the new keys below are read by the engine.

| change | key (default) | side | off |
|---|---|---|---|
| b455..b467 **light-user hedge (K4M2)**: a lite connection opens a second (hedge) sub at connect; the exit takes the first of the two or joins them (role 7, HEDGE3), makes Iran's first sub the primary (HEDGE4), and samples a download stripe early (HEDGE7) | `HS2_LITE_HEDGE8` (on) | both (Iran decides) | `=0` |
| hedge window: early buffer for the join replay and base of the heavy (one-copy) threshold (b464 lab values were 16384 / 400) | `HS2_LITE_HEDGE3_BYTES` (4096), `HS2_LITE_HEDGE3_MS` (200) | both | - |
| the exit waits this long for a hedged connection's first sub before the hedge sub creates it | `HS2_LITE_HEDGE8_MS` (20) | Kharej | - |
| rc15 **capability gate**: an exit with the hedge announces ext cap bit 18 (`capExtLiteHedge`); Iran hedges, uses a records first sub and sends role 7 only toward such a link, otherwise the old raw first sub and a plain join | no key | both | - |
| rc15b **rate-based heavy rule**: a hedged connection goes one copy only after more than HEDGE3_BYTES + 32 KB per second of its age in one direction (downloads and bursts at once; long-lived light connections keep both copies) | `HS2_RC15_HEAVY_KBPS` (32) | both | `=0` (cumulative rule) |
| rc15b **no-wait hedge open**: the hedge sub never queues for a PICKCAP slot; no slot = no hedge for that connection | `HS2_RC15_HEDGE_NOWAIT` (on) | Iran | `=0` |
| rc15f **JOINRES**: a fresh light sub joining a call gets the copies stranded on the call's subs that received nothing for >= 400 ms, resent at once; no wire change | `HS2_RC15F_JOINRES` (on) | Kharej | `=0` |
| b470 **PACEFIX** (ZDU): the drain-end user pace spreads its users over the lead it started with (never above `HS2_ZDU_PACE_RATE`), keeps a debt for close work and oversleep, and closes big batches in parallel, so the old process exits on time at 24k connections | `HS2_B470_PACEFIX` (on) | Kharej | `=0` (rel69r-rc14 pace) |
| rc15c / rc15d hedge-CPU experiments (burst-only copy, quiet cap, first-N bursts, freshness gate): all failed their A/B | `HS2_RC15C_BURST`, `HS2_RC15C_CAPQUIET_MS`, `HS2_RC15D_HEDGE_BURSTS`, `HS2_RC15D_FRESH_MS` (off) | both | - |
| b455..b469n lab experiments and debug counters (HEDGE10, H6 on ext cap bit 17, NORAW, DBG, HEDGE2) | `HS2_LITE_HEDGE10`, `HS2_LITE_H6`, `HS2_B463_NORAW`, `HS2_B462_DBG`, `HS2_LITE_HEDGE2` (off) | both | - |

Unchanged from rel69r-rc14 and still on: PICKCAP=2 + CAPLAT, DRAINFAST, DRAINSMOOTH, STEP_MS=5, UPGSAFE, CAPWAIT,
FLOORFAST and the other b449 fixes.

Installer (`install.sh`, root file == `hs2-src/install/install.sh`): byte-identical to rel69r-rc14 (sha256
`60404d42…`). `hs2-src/install/install.sh` on the source branch is the same as at rel69r-rc14's build commit and
lacks the live load-balancing screen, so the published installer is kept, as in the earlier releases.

## Lab results (rel69r-rc15 vs the previous production build rel69r-rc14)

Paired runs, 95 % confidence intervals; "n.s." = not proven either way.

- **Real WAN, partial link faults** (an Iran and an abroad lab host, 8 ACCA pairs; a quarter of the links faulted, 2 %
  upload loss, crowd 300): light p50 -2.4 [-3.1,-1.8] ms, p90 -1684 [-2200,-1168] ms, p95 -893 [-1257,-530] ms,
  p99 -457 [-903,-11] ms (pooled A -> B: p50 83.8 -> 81.4, p90 2228 -> 541, p95 3381 -> 2367, p99 4544 -> 4276 ms;
  share over 120 ms 29.5 % -> 16.2 %); burst p50 -30 [-50,-11], p90 -2549 [-3489,-1608], p99 -2955 ms; open
  failures -10.9 [-15.4,-6.3] a run. Calls: p99 -1215 [-4245,+1816] n.s., longest stall -1813 n.s., stalls over
  3 s +0.1 n.s. (not worse). Crowd p99 and failures equal; heavy download, stalls equal. CPU Iran +1.7 n.s.,
  Kharej +9.6 [-1.5,+20.6] n.s. Internal end-error counters Iran +64, Kharej +436 a run (proven): connection ends
  of hedged light connections that had already finished on both sides (no user-visible effect; a clean end
  instead of the reset is on the backlog).
- **Link kills under load** (production-like harness: links killed every 4 s, 15 % blackholed every 12 s; 15 ABBA
  pairs): crowd failures -276 [-308,-245], crowd p99 -69 [-136,-2] ms (better); lost connections Iran equal
  (-2.3 n.s.), download equal (n.s.), upload +140 Mbps; Kharej lost connections +11.8 [+0.1,+23.5] (the same
  already-finished ends as above); CPU per byte Iran +2.5 n.s., Kharej +2.0 n.s.
- **ZDU exit at 12k users** (2 pairs): game p99 -11.5 [-21,-2] ms better, crowd max -48 n.s., calls loss n.s.,
  hand-off timeouts 0, lost pings 0; game max +15 [+12,+18] ms (n=2). A rel69r-rc15 exit with a rel69r-rc14
  Iran side: equal to rel69r-rc14 (mixed versions safe).
- **ZDU exit at 24k connections** (n=1, the rc15e candidate, same hedge code without JOINRES): the old process
  exits on time (-0.55 s vs +1.68 s), hand-off timeout 0 vs 1, lost pings 0 vs 5, calls loss 0.139 vs 0.143 %;
  cap wait and crowd max higher (both arms saturate the lab host at 24k).
- **Fault-free real WAN** (earlier K4M2 build): light p90 -22..-55 ms, p95 -83..-92 ms, p99 -265..-300 ms.
- Release gate on `c043866f` (built from an LF archive): gofmt 0 files, `go vet` clean; `go test ./...` passed
  with `HS2_ZDU` unset; with `HS2_ZDU=1` one timing test (`TestMixedOldExitShrinksViaRetireIfOver`, pool code
  unchanged since rel69r-rc14) failed under load and passed 5/5 alone; `-race` on the engine areas touched
  (MConn, Lite, Hedge, Stripe, Rescue, Call, B449, ZDU, Drain, PICKCAP, RC15, Raw, Upgrade) passed in both ZDU
  states, no data race.

Release binary: built from `c043866f` with
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTag=rel69r-rc15" ./cmd/hs2`,
static; the same bytes as the build of `93438864` used in the WAN series (reproducible). The public Go source
differs from that commit only in redacted addresses (tests and comments), the published installer and
`BUILD.md`, and the left-out `hs2-src/lab/` files (only `lab/netsim` is kept).

Rollback to rel69r-rc14 behaviour: `HS2_LITE_HEDGE8=0` (both sides), `HS2_RC15F_JOINRES=0` and
`HS2_B470_PACEFIX=0` (Kharej), or run the rel69r-rc14 binary (sha256 2f3d34a2…). If ZDU is on, hand over to the
older binary with `hs2-zdu-upgrade.sh` as usual (both are ZDU-capable).
