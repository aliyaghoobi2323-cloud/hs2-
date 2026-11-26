# hs2 rel69r-rc15p1 — release notes

- Binary `hs2-linux-amd64`, sha256
  `74f24c5337f87481b0d2ecd3dc6c9443de7dde6eb76c0e6f5eaf76eae93cd58e`, `hs2 version` shows `[build rel69r-rc15p1]`
  (the exact production bytes, published unchanged, so the stamp keeps the release-candidate name).
- Previous GitHub release: **rel69r-rc15** (tag `v-rel69r15`, binary sha256 `13bca10d…`, `[build rel69r-rc15]`).
- `install.sh`: unchanged from rel69r-rc15 / rc14 (the source branch did not touch the installer). Menu, live
  load-balancing screen, Alerts and watchdog screens, certificate hook unchanged. sha256 in `install.sh.sha256`.
- **Config files: no change needed.** Upgrade as usual (`bash install.sh upgrade`, Kharej first, then Iran), or
  with a zero-downtime handover if you turned ZDU on (README → *Zero-downtime upgrade*).
- **No wire change**: no new record type, header or capability bit. A rel69r-rc15p1 side next to a rel69r-rc15 /
  rc14 side (either way round) works; each side's new rules act on its own sends and opens.

## For the owner, in short

- **Light users (web, apps, messengers) stay fast when some links go bad — again much faster than rel69r-rc15.**
  When a light connection's link goes dark, it now moves to a live link and stays there (STICK), new opens pick
  only links that are reading (LIVEPICK, NEWPICK), and a silent connection reopens at once and resends what was in
  flight (FASTOPEN, TAILRES). Real WAN with a quarter of the links faulted and 2 % upload loss (8 pairs): light-user
  page time p95 -1813 ms, p99 -2915 ms (pooled p99 3879 -> 1072 ms); crowd late requests -47 a run; burst p90
  -849 ms; probe failures -3.1 a run.
- **Calls no longer get long freezes from open storms** (CALLOPEN): a call connection used to open dozens of new
  subs in about a second, many on dead links, while one open hung. Now one extra open at a time, only on live
  links. Real WAN: call freezes over 5 s 12 -> 2 (proven), longest freeze 8039 -> 5241 ms; lab: call p99
  3838 -> 854 ms.
- Cost: none proven. CPU equal (Iran +0.6, Kharej -1.0, n.s.); Kharej internal error-ends fewer (-116 a run).

## What changed since rel69r-rc15

Source = branch l485 `f3739759` (executable code = `151807c0`; `1c02ff8f` adds README rows and `f3739759` adds
only `TestRC15p1Defaults`). envdef unchanged (138 keys); the new keys are read by the engine.

| change | key (default) | side | off |
|---|---|---|---|
| l480b **master key** for STICK, LIVEPICK, NEWPICK, FASTOPEN, TAILRES and PAIRSK | `HS2_L480` (on) | both | `=0` |
| l480 **STICK**: after a lite connection's dark primary moves to a live sub, the old sub is not made primary again and gets no duplicate copies until its link reads again plus the hold | `HS2_L480_STICK` (on with the master), `HS2_L480_HOLD_MS` (3000) | both | `HS2_L480=0` |
| l480 **LIVEPICK**: opens for a dark connection pick only links that read within 400 ms (none = pick unchanged) | `HS2_L480_LIVEPICK` (on with the master) | both | `HS2_L480=0` |
| l480b/l480c **NEWPICK**: a new connection's first sub and hedge sub leave out links silent while they have opens pending | `HS2_L480_NEWPICK` (on with the master) | both | `HS2_L480=0` |
| l477/l478 **FASTOPEN + TAILRES**: a silent lite primary gets a fresh sub at once (at most 256 in flight per process), the primary moves to it and the silent episode's tail is resent on it | `HS2_L478_FASTOPEN`, `HS2_L477_TAILRES` (on with the master), `HS2_L478_GLOBAL` (256) | both | `HS2_L480=0` |
| l480d **PAIRSK**: while a STICK holds, a call pair is still placed (old stuck sub left out) | `HS2_L480D_PAIRSK` (on with the master) | both | `=0` |
| l485 **CALLOPEN**: one slow second open per pending open, LIVEPICK mark on every call open, no new call / slow-open sub while a connection has 3 fresh (< 2 s) subs that received nothing | `HS2_L485_CALLOPEN` (on) | both | `=0` |
| l476..l480d experiments and a debug trace (CALLPEND and CALLLIVE failed their A/B) | `HS2_L480D_CALLPEND`, `HS2_L480D_CALLLIVE`, `HS2_L476_JOINRES`, `HS2_L476_JOINPRIM`, `HS2_L480C_DBG` (off) | both | - |

Unchanged from rel69r-rc15 and still on: the light-user hedge (HEDGE8, HEAVY_KBPS=32, HEDGE_NOWAIT), JOINRES,
PACEFIX, PICKCAP=2 + CAPLAT, DRAINFAST, DRAINSMOOTH, UPGSAFE and the other b449 fixes.

Installer (`install.sh`, root file == `hs2-src/install/install.sh`): byte-identical to rel69r-rc15 (sha256
`60404d42…`). `hs2-src/install/install.sh` on the source branch is the same at `f3739759` as at rel69r-rc15's
build commit `c043866f`, so the published installer is kept, as in the earlier releases.

## Lab results (rel69r-rc15p1 vs the previous production build rel69r-rc15)

Paired runs, 95 % confidence intervals; "n.s." = not proven either way.

- **Real WAN, partial link faults** (an Iran and an abroad lab host, 8 ABBA pairs; a quarter of the links
  blackholed 5 s out of every 15 s, 2 % upload loss, crowd 300, probe, burst, 8 calls, heavy; the exact release
  binary on both hosts): no collapse.
  - light p50 -5.8 [-8.7,-2.9] ms, p95 -1813 [-2358,-1268] ms, p99 -2915 [-3500,-2331] ms, p99.9 -1950
    [-2527,-1373] ms (pooled p99 3879 -> 1072 ms);
  - crowd late -47.2 [-56.9,-37.6] a run (better), crowd p99 +1.0 ms n.s., failures +0.1 n.s.;
  - burst p90 -849 [-1411,-286] ms, p50 -24 n.s., p99 -842 n.s.; probe failures -3.1 [-4.4,-1.8]; heavy equal;
  - calls: p99 -446 n.s., max -742 n.s.; freeze episodes over 1 s 67 -> 28 (-4.9 [-10.0,+0.2]), over 3 s 18 -> 20
    (equal), **over 5 s 12 -> 2 (-1.2 [-2.4,-0.1], proven)**, longest 8039 -> 5241 ms;
  - CPU Iran +0.6 n.s., Kharej -1.0 n.s.; Kharej internal error-ends -116 [-145,-88] (better), Iran +2.6 n.s.
- **CALLOPEN alone** (lab with link faults, 8 ABBA pairs, against the same build without CALLOPEN): call p99
  3838 -> 854 ms (-2984 [-4584,-1385]), longest call stall 6999 -> 2481 ms (-4518 [-6968,-2068]), call samples
  over 300 ms -476 [-921,-31] a run; nothing proven worse (crowd late +2.0 n.s., probe, burst, CPU n.s.). Confirmation run (2 pairs):
  call p99 -4300 [-4994,-3605], call freeze episodes over 3 s -3.0 [-5.0,-1.0] a run.
- Known and inherited from rel69r-rc15 (both arms): in some runs no call connection passes the call detector, and
  then a call waits out a whole 5 s link blackhole (the remaining > 3 s episodes are almost all 4.85..4.92 s). Not a
  regression; being traced separately.
- Release gate on the executable code `151807c0` (LF archive): gofmt 0 files, `go vet` clean; `go test ./...`
  passed with `HS2_ZDU` unset and with `HS2_ZDU=1` (16 packages, 0 failures, no rerun needed); `-race` on the engine
  areas touched (MConn, Lite, Hedge, Stripe, Rescue, Call, Darkcall, L47x, L48x, RC15, B449, ZDU, Drain, Raw,
  Upgrade, Pick) passed in both ZDU states, no data race; `TestRC15p1Defaults` passes (the release defaults hold
  after package init).

Release binary: built from `f3739759` with
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTag=rel69r-rc15p1" ./cmd/hs2`,
static, 18981024 bytes. The public Go source differs from that commit only in redacted addresses (tests and
comments), the published installer and `BUILD.md`, and the left-out `hs2-src/lab/` files (only `lab/netsim` is
kept).

Rollback to rel69r-rc15 behaviour: `HS2_L480=0` and `HS2_L485_CALLOPEN=0` (both sides; either side alone is safe),
or run the rel69r-rc15 binary (sha256 13bca10d…). If ZDU is on, hand over to the older binary with
`hs2-zdu-upgrade.sh` as usual (both are ZDU-capable).
