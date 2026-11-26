# hs2 rel69r-rc15p3 — release notes

- Binary `hs2-linux-amd64`, sha256
  `e67e050a3a42e3fef8f8d29d2f61e788f4fc6f45af3227f64f26a104954935b0`, `hs2 version` shows `[build rel69r-rc15p3]`
  (the exact production bytes, published unchanged, so the stamp keeps the release-candidate name).
- Previous GitHub release: **rel69r-rc15p1** (tag `v-rel69r15p1`, binary sha256 `74f24c53…`, `[build rel69r-rc15p1]`).
  rel69r-rc15p2 (= rc15p1 + CALLDETECT) was not published on its own; this release includes it.
- `install.sh`: unchanged from rel69r-rc15p1 / rc15 (the source branch did not touch the installer). sha256 in
  `install.sh.sha256`.
- **Config files: no change needed.** Upgrade as usual (`bash install.sh upgrade`, Kharej first, then Iran), or
  with a zero-downtime handover if you turned ZDU on (README → *Zero-downtime upgrade*).
- **No wire change**: no new record type, header or capability bit. A rel69r-rc15p3 side next to a rel69r-rc15p1 /
  rc15 side (either way round) works.

## For the owner, in short

- **Calls freeze less when links go dark** (CALLDETECT): some call connections were never watched by the dark-call
  rescue (their first sub opened hedged), so a call could wait out a whole 5 s link blackhole. Now every call-like
  connection is watched. Lab (14 pairs): call samples over 3 s -176 a run, call freeze episodes over 3 s -2.3 a
  run, longest call stall -1316 ms; light p99 -170 ms.
- **Light users stay fast right after a blackhole ends** (FRESHBACK): a link that just came back can still be in
  TCP retransmit backoff for up to ~4 s; new light connections no longer start on it. Lab (15 pairs): light p99.9
  2874 -> 188 ms, light samples over 2 s 32 -> 3 (pooled), probe failures -1.6 a run.
- Cost: none proven to matter. CPU about +1 point with CALLDETECT (of ~90 % in the loaded lab); FRESHBACK CPU n.s.;
  FRESHBACK does not hurt calls.

## What changed since rel69r-rc15p1

Source = branch l488 `5a481ff2` (= `f3739759` + l487 + l488; `5a481ff2` itself is gofmt only; the release gate
commit `36a5168c` adds only lab notes). envdef unchanged; the new keys are read by the engine.

| change | key (default) | side | off |
|---|---|---|---|
| l487 **CALLDETECT**: a call-like connection that is not rescue-capable (first sub opened hedged, non-raw, so it was never on darkcall's list) is registered for darkcall too: its dark primary is proven and switched to a live sub, and a fresh sub is asked for when no live candidate is left; finished connections leave the list | `HS2_L487_CALLDETECT` (on) | both | `=0` |
| l488 **FRESHBACK**: a link whose read resumes after >= 600 ms of pending silence is "recovering" until its TCP_INFO shows no retransmit backoff (at most `REC_MAX_MS`; `REC_NOTCP_MS` without a socket); a new light connection's first sub and hedge sub leave it out (none left = pick unchanged) | `HS2_L488_FRESHBACK` (on), `HS2_L488_REC_MAX_MS` (5000), `HS2_L488_REC_NOTCP_MS` (1500) | both (picks on Iran) | `=0` |
| l487 darkcall registration / kick trace (lab) | `HS2_L487_DBG` (off) | both | - |

Unchanged from rel69r-rc15p1 and still on: the HS2_L480 family (STICK, LIVEPICK, NEWPICK, FASTOPEN, TAILRES,
PAIRSK), CALLOPEN, the light-user hedge, JOINRES, PACEFIX, PICKCAP=2 + CAPLAT, DRAINFAST, DRAINSMOOTH, UPGSAFE and
the other b449 fixes.

Installer (`install.sh`, root file == `hs2-src/install/install.sh`): byte-identical to rel69r-rc15p1 (sha256
`60404d42…`). `hs2-src/install/install.sh` on the source branch is the same at `5a481ff2` as at `f3739759`, so the
published installer is kept.

## Lab results

Paired ABBA runs, 95 % confidence intervals; "n.s." = not proven either way. **Lab tests only**: no real-WAN lab
exists any more, so unlike earlier releases these notes have no WAN series. Lab harness: a quarter of the links
blackholed 5 s out of every 15 s, crowd, probe (22/s), burst, 8 calls, heavy, 60 s runs.

- **CALLDETECT** (14 pairs, key on vs rc15p1 behaviour): call samples > 3 s -176 [-303,-50] a run, call freeze
  episodes > 3 s -2.3 [-3.9,-0.6] a run, longest call stall -1316 [-2331,-301] ms; light p99 -170 [-309,-32] ms,
  p99.9 -147 n.s.; crowd, burst and losses n.s.; CPU Iran +1.0 [+0.0,+2.0], Kharej +1.3 [+0.1,+2.6]. An earlier
  10-pair series: call samples > 3 s -121 a run, call p99 -993 ms, freeze episodes > 3 s 16 -> 1 (pooled).
- **FRESHBACK** (15 pairs, key on vs rc15p2): light p95 -10.5 ms, p99 -100 [-164,-37] ms, p99.9 -2129
  [-2633,-1626] ms (pooled 2874 -> 188 ms), samples > 2 s -1.9 a run (pooled 32 -> 3), > 3 s -0.7 a run, probe
  failures -1.6 a run (all proven better); calls n.s.; crowd, burst, losses and CPU n.s.
- **FRESHBACK and calls** (4 pairs, on vs off): call samples > 3 s 0 / 0, longest call stall -14 ms n.s.; light
  p99.9 240 ms with it on vs 3119 ms off. In 14 lab runs with FRESHBACK on, no call sample over 3 s.
- Known (both arms): a call whose subs all go dark in the same blackhole window can still wait it out; a fix is
  being tested separately and is not part of this release.
- Release gate on `5a481ff2`: gofmt clean, `go vet` clean; `go test ./...` passed with `HS2_ZDU` unset and with
  `HS2_ZDU=1` (16 packages, 0 failures); `-race` on the engine areas touched (MConn, Lite, Hedge, Stripe, Rescue,
  Call, Darkcall, L47x, L48x, RC15, B449, ZDU, Drain, Raw, Upgrade, Pick) passed in both ZDU states, no data race;
  `TestRC15p1Defaults` checks the release defaults including the two new keys.

Release binary: built from `5a481ff2` with
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTag=rel69r-rc15p3" ./cmd/hs2`,
static, 18993312 bytes; a rebuild from `5a481ff2` gives the same sha256. The public Go source differs from that
commit only in redacted addresses (tests and comments), the published installer and `BUILD.md`, and the left-out
`hs2-src/lab/` files (only `lab/netsim` is kept).

Rollback to rel69r-rc15p1 behaviour: `HS2_L487_CALLDETECT=0` and `HS2_L488_FRESHBACK=0` (both sides; either side
alone is safe), or run the rel69r-rc15p1 binary (sha256 74f24c53…). If ZDU is on, hand over to the older binary
with `hs2-zdu-upgrade.sh` as usual (both are ZDU-capable).
