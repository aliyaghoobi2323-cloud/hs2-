# dl241 — young-sub proof (HS2_DL240_YOUNG) + slow first link (lab2, 2026-11-16 16:37–17:20 UTC)

## Code (worktree C:\h2w\b240, branch dl240, on top of b460ca19; not pushed)
- **HS2_DL240_YOUNG stays default OFF** (the A/B below does not prove it; task 3 "default ON" NOT done, no envdef change).
- New: `HS2_DL241_SLOWSLOT` (Iran/edge decides growth; default OFF, `=1` on; no wire change) — engine/mconn_dl241.go
  (+ test TestDL241SlowSlot), hook in stripeTickLocked (mconn_stripe.go: `!m.dl241SlowSlotLocked(c, up, rate, now)` next to
  dlfair's extra slot). A NEW download (raw conn < 20 s, never an upload) that passed the usual hot ticks (4 x 1 s at
  >= 5 Mbit/s/sub) but is under stripeDlFastRate (80 Mbit/s) may take one of dlfair's 4 extra slots past stripeMaxConns.
  Logs `dl241: slow new download took stripe slot N ...`; with HS2_MCONN_STRIPE_LOG also `dl241: new download refused a
  stripe slot ...` (key off too: diagnostic for prod).
- Lab harness: hs2-src/lab/dl241/ (soak241a.sh = static slow paths, soak241b.sh = + transient slow events, arm.sh,
  chain*.sh, an.py, young2.py, shapes.sh). Also fixes the bs201 soak idle-group subshell that kept the lab locks until
  el=900/1000 when TT < 1000 (the 5 min gaps between dl240 arms).
- lab2 binaries: /opt/hs2lab/dl241/bin/hs2A (b460ca19, sha 17a6057c) and hs2S (+dl241, sha fc1f5f0e).

## Task 1 — targeted young-sub repro, ABAB (same binary hs2A; B = HS2_DL240_YOUNG=1 both sides, only exit acts)
Harness: bs201 crowd (3000 crowd, 9 bg heavy, 10 paths, max_links 200), TT=390, tests back to back (2 downloads : 1 upload,
iperf -R 12 s), no RST windows, HS2_MCONN_STRIPE_LOG=1.
- Pair 1 (A1/B1) "static": paths C, F, I = delay 200 ms + 2 % loss + rate 60 mbit for the whole run.
- Pair 2 (A2/B2) "transient": static + every 13 s two paths get delay 300 ms + 1 % loss for 6 s (cold / queued links).

| arm | downloads | mean / p10 / min Mbit | seconds <100 after 1st | stalls (sec <30 % of best before, <200) | slow first link (first 3 s <80) | young-HOL stripes (Iran oooB >= 4 MB within 3.5 s of striping; >= 8 MB) | Iran ticks oooB >= 8 MB | uploads n / mean / p10 | crowd p99 avg / max ms, fails |
|---|---|---|---|---|---|---|---|---|---|
| A1 off | 15 | 375 / 23 / 17 | 37 | 1 | 4 | 7 of 54 (5) | 95 | 8 / 222 / 91 | 244.6 / 577.5, 4 |
| B1 on  | 15 | 399 / 180 / 117 | 15 | 3 | 2 | 7 of 54 (5) | 190 | 8 / 318 / 206 | 246.9 / 595.9, 0 |
| A2 off | 15 | 244 / 16 / 13 | 68 | 8 | 6 | 14 of 53 (11) | 555 | 8 / 177 / 8 | 726.4 / 874.6, 0 |
| B2 on  | 15 | 196 / 14 / 7 | 67 | 9 | 6 | 9 of 54 (8) | 485 | 8 / 175 / 47 | 711.2 / 961.7, 1 |
| S1 young+slowslot | 15 | 211 / 16 / 5 | 59 | 7 | 5 | 8 of 54 (8) | 308 | 8 / 118 / 9 | 728.0 / 875.3, 0 |

- The repro works: young-sub HOL went from 2 of 161 stripes (dl240) to 7/54 (static) and 14/53 (transient); A2 t280
  per second 92, 332, **10**, 710 = the prod shape.
- The rule engages (B1 kh: held 45000, fallback 7190; B2 held 80000).
- **Not proven**: young-HOL A 21/107 vs B 16/108; stalls A 9 vs B 12; seconds <100 A 105 vs B 82 (driven by slow first
  links: raw phase, the rule cannot touch it); download mean A 310 vs B 298. Uploads and crowd p99 unaffected.
- **Why it cannot work (trace B1 2ec1a4, 16:47:43–45):** the exit held the young sub at budget (unread 1409 KB, consumed
  0 KB, age 1.39 s), yet Iran sat at rx next = 24,578,026 = the young sub's FIRST chunk (kept0 unchanged > 1 s) with
  oooB 24 MB. Capping how much is queued on the young sub does not move the hole: everything the primary sends after the
  young sub's first chunk waits for that first chunk. The stall length = the young link's time to first delivery
  (raw handshake was already 523 ms), not its queue.
- Next fix (proposal, not built): on the exit, while a stripe sub has consumed 0 after ~max(300 ms, 2 x primary RTT),
  re-send its unconsumed chunks on the primary (the STRIPE_RESEND / keep machinery; Iran dedups by offset), or do not
  put in-order data on a sub before its first window update (send it only a probe/duplicate chunk). Exit-only, no wire
  change if Iran's reassembly already drops duplicate offsets (to verify).

## Task 2 — download whose first link crawls and never stripes
- Root cause found (A1 t200, per second 2, 4, 16, 6, 29, 22, 23, 31, 5, 34, 0, 34): `stripe nostripe 583d34: life 12.6s
  recvd 25 MB max 34 Mbit/s active=8 [... nothot=4 slot=5 dlcold=9]` — the usual rule passed its hot ticks but all 8
  stripe slots were held by background streams (`slot=5` refusals); dlfair's 4 extra slots only go to a download at
  >= 80 Mbit/s, which a slow first link never reaches. Same as dl240 B1 t260 (`stripe slot 9 past the cap of 8 (101
  Mbit/s)` only 10 s in). The slowest download is the one refused the other links.
- Fix HS2_DL241_SLOWSLOT (above). S1 (one arm, transient plan, same as B2 + the key): engaged 5 times (`took stripe
  slot 9 ... age 5.0-11.2 s, 6-23 Mbit/s`), 0 nostripe lines; e.g. S1 t200 1, 2, 6, 13, 12, 15, 22, 32, 18, **202, 158,
  168** (striped at 7.0 s). Download mean 211 vs B2 196, seconds <100 59 vs 67, stalls 7 vs 9; crowd p99 728 vs 711.
  Uploads 118 / p10 9 vs B2 175 / 47 (n=8 each, harsh plan, noisy; but an extra slot held by a slow download is also a
  slot an upload's first growth cannot take — check in the next A/B). Single arm: **key stays default OFF**.
- Many slow downloads in pair 2 DID stripe and still crawled (subs on event paths): the stripe is as slow as its
  slowest sub in order — same HOL family as task 1.

## Tests
- `go test ./... -timeout 60m` (dl241 tree, on lab2 with GOMAXPROCS=2 on 2 cores at nice 19 next to the lab):
  all ok except engine TestL3FecEndToEndStuckLink (189 of 200 packets) and TestMeteredConnBlockedTime (47 % blocked) —
  timing flakes under CPU load; both pass on rerun (-count=3). `go vet ./engine` ok; TestDL24*|TestStripe*|TestDlFair* ok.
- `go test -race ./engine`: started 17:0x on lab2 (/opt/hs2lab/dl241/race.txt), still running at write-up time.
- No C:\h2w\bin241d build (task 3 condition not met).

## Next
1. Exit "young-sub first-chunk rescue" (above) — the real fix for the 1 s stall; test on soak241b (pair-2 plan, which
   reproduces it at 14/53 stripes) ABAB, 2 pairs.
2. HS2_DL241_SLOWSLOT: ABAB 2 pairs on soak241a (static plan; the cap case shows there: A1 t200) and watch uploads.
3. Optionally prod: HS2_MCONN_STRIPE_LOG is off in prod, so count `nostripe ... slot=` only in a lab-like canary.
