# Changelog — installer & binary hardening

This log covers a hardening pass done in ordered phases. It has three tracks:

- **Track A — installer robustness** (`install.sh`, bash only, **no binary
  change**): safe to roll out to a live server because the shared binary is
  untouched.
- **Track B — binary features** (Go; the binary is rebuilt, so both ends of a
  tunnel should run the new build).
- **Phase C — active-probe resistance** (the TLS front door and the cover page).

Where each change came from: a line-by-line audit of `install.sh` (findings
numbered **#1–#23**), a set of UX findings (**U1–U14**), and a field **probe
report** on how the tunnel port answered unauthorized connections. Every phase
was covered by tests (Go unit/integration, bash unit + pty, the release-file
check) **and** an independent adversarial review whose confirmed findings were
fixed in a follow-up commit before the phase shipped.

The published binary is static (`CGO_ENABLED=0`) and carries a build stamp
visible in `hs2 version`; see `hs2-src/BUILD.md`.

## rel69r-rc15p1 — GitHub release 2026-11-26: rel69r-rc15 + sticky primary and live-link picks for light users (HS2_L480 family: STICK, LIVEPICK, NEWPICK, FASTOPEN, TAILRES, PAIRSK) + call open fixes (CALLOPEN)

Source = branch l485 `f3739759` (executable code = `151807c0`; `1c02ff8f` adds README rows and `f3739759` adds only
`TestRC15p1Defaults`). envdef unchanged (138 keys); the new keys below are read by the engine. No wire change: no new
record type, header or capability bit, so mixed versions with rel69r-rc15 / rc14 are safe in either direction.

| change | key (default) | side | off |
|---|---|---|---|
| l480b **master key** for the family below (STICK, LIVEPICK, NEWPICK, FASTOPEN, TAILRES, PAIRSK) | `HS2_L480` (on) | both | `=0` (members can still be turned on one by one with `=1`; PAIRSK needs the master) |
| l480 **STICK**: after a lite connection's dark primary moves to a live sub, the old sub is not made primary again and gets no duplicate copies until its link reads again plus the hold | `HS2_L480_STICK` (on with the master), `HS2_L480_HOLD_MS` (3000) | both | `HS2_L480=0` |
| l480 **LIVEPICK**: opens for a dark connection pick only links that read within 400 ms (pool-wide; none = pick unchanged) | `HS2_L480_LIVEPICK` (on with the master) | both | `HS2_L480=0` |
| l480b/l480c **NEWPICK**: a new connection's first sub and hedge sub leave out links that are silent while they have opens pending | `HS2_L480_NEWPICK` (on with the master) | both | `HS2_L480=0` |
| l477/l478 **FASTOPEN + TAILRES**: a lite connection whose primary went silent opens a fresh sub at once (at most `HS2_L478_GLOBAL` in flight per process), moves the primary to it and resends the silent episode's tail on it | `HS2_L478_FASTOPEN`, `HS2_L477_TAILRES` (on with the master), `HS2_L478_GLOBAL` (256) | both | `HS2_L480=0` |
| l480d **PAIRSK**: while a STICK holds, a call pair is still placed with the old stuck sub left out, instead of the plain duplicating path | `HS2_L480D_PAIRSK` (on with the master) | both | `HS2_L480D_PAIRSK=0` or `HS2_L480=0` |
| l485 **CALLOPEN**: one slow second open (b449e) per pending open (it used to start a fresh one on every manager tick while the first open hung on a dead link: storms of 35..60 subs in ~1 s), the LIVEPICK mark on every call open, and no new call / slow-open sub while a connection has 3 fresh (< 2 s) subs that received nothing | `HS2_L485_CALLOPEN` (on) | both | `=0` |
| l476..l480d experiments and a debug trace (CALLPEND and CALLLIVE failed their A/B) | `HS2_L480D_CALLPEND`, `HS2_L480D_CALLLIVE`, `HS2_L476_JOINRES`, `HS2_L476_JOINPRIM`, `HS2_L480C_DBG` (off) | both | - |
| open-path trace (lab, only with `HS2_MCONN_TRACE`) | no key | both | - |

Unchanged from rel69r-rc15 and still on: the light-user hedge (HEDGE8, HEAVY_KBPS=32, HEDGE_NOWAIT), JOINRES, PACEFIX,
PICKCAP=2 + CAPLAT, DRAINFAST, DRAINSMOOTH, UPGSAFE and the other b449 fixes.

Installer (`install.sh`, root file == `hs2-src/install/install.sh`): byte-identical to rel69r-rc15 / rc14 (sha256
`60404d42…`). `hs2-src/install/install.sh` on the source branch did not change between `c043866f` and `f3739759`, so
the published installer is kept.

Binary sha256 `74f24c5337f87481b0d2ecd3dc6c9443de7dde6eb76c0e6f5eaf76eae93cd58e` (`[build rel69r-rc15p1]`, the exact production bytes). Lab results and rollback: `RELEASE-rel69r15p1.md`.

## rel69r-rc15 — GitHub release candidate 2026-11-25: rel69r-rc14 + light-user hedge (K4M2, rate-based heavy rule, no-wait hedge open, ext cap bit 18) + call join resend (JOINRES) + on-time ZDU drain end (PACEFIX)

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

Binary sha256 `13bca10da170753b82a32a2df56dedfea8e1496f8ea78b9f43ba3aef0e60087f` (`[build rel69r-rc15]`, the exact production bytes). Lab results and rollback: `RELEASE-rel69r15.md`.

## rel69r-rc14 — GitHub release candidate 2026-11-24: rel69r + heavy-download stall fixes (b449, UPGSAFE) + smooth ZDU drain end (DRAINFAST, STEP_MS, DRAINSMOOTH, PICKCAP=2 + CAPLAT) + close pacing + freebind + certificate hook fix

Source = branch rel69r-rc14 `2b66b136` (the build commit; later commits on that branch are lab notes and the next
candidate's work, not in this release). envdef unchanged (138 keys); the new keys below are read by the engine.

| change | key (default) | side | off |
|---|---|---|---|
| b449 **stuck primary**: a lite one-copy connection whose primary sub stops writing for 1.5 s while another usable sub exists moves to the live sub (resending what it kept); a connection with nothing kept is ended cleanly after 8 s instead of hanging for a minute | `HS2_B449_PSTUCK` (on), `HS2_B449_PSTUCK_MS` (1500), `HS2_B449_PSTUCK_HARD_MS` (8000) | both | `=0` |
| b449 **dark open**: a download whose link goes silent while it waits for an answer gets a new sub on another link at once | `HS2_B449_DARKOPEN` (on) | Iran | `=0` |
| b449c **keep floor**: a connection's first 64 KB of kept bytes are dropped only on TCP proof, so a small request never loses its resend copy | `HS2_B449_KEEP_FLOOR_KB` (64) | both | `=0` |
| rc14d **floor fast**: the same result as the keep floor without rescanning the kept chunks on every write (Iran CPU) | `HS2_B449_FLOORFAST` (on) | both | `=0` |
| b449d **target wait**: where a lost copy used to reset the connection, its chunks wait up to 1.5 s for a live target and are resent | `HS2_B449_TGT_WAIT_MS` (1500) | both | `=0` |
| b449e **slow open**: a light open still pending after 1 s gets one more open in parallel on another link | `HS2_B449_SLOWOPEN_MS` (1000) | Iran | `=0` |
| b449e **cap wait**: the slow-open clock does not count time spent in the PICKCAP queue (no second open storm at a ZDU exit) | `HS2_B449E_CAPWAIT` (on) | Iran | `=0` |
| b449h **open skip**: an open retry leaves out the links this connection already failed on | `HS2_B449H_OPENSKIP` (on) | Iran | `=0` |
| b449j **UPGSAFE**: a sub that dies during the raw -> records upgrade handshake no longer ends a heavy download; the upgrade is repeated on another sub (same boundary) | `HS2_B449J_UPGSAFE` (on) | both | `=0` |
| b447/b454 **paced drain end** (ZDU): an old Kharej process with users left at its deadline closes them at a fixed rate, starting early enough to exit on time | `HS2_ZDU_PACE_RATE` (2667/s) | Kharej | `=0` |
| b468 **DRAINFAST**: the pace closes user connections, not links; links stay up for the users not yet closed | `HS2_B468_DRAINFAST` (on) | Kharej | `=0` (b454 link pace) |
| b469 **STEP_MS**: the same rate in smaller steps (5 ms batches instead of 25 ms) | `HS2_B469_STEP_MS` (5; 1..25) | Kharej | `=25` (old step) |
| b469 **DRAINSMOOTH**: >= 500 connection ends in 500 ms raise the GC target to 400 % until 2 s after the storm (15 s max; the soft memory limit still applies) | `HS2_B469_DRAINSMOOTH` (on), `HS2_B469_ENDS` (500), `HS2_B469_PCT` (400) | Iran | `=0` |
| b457 **PICKCAP**: at most 2 join-sub placements run at once (a new connection's first sub is not capped) | `HS2_B457_PICKCAP` (2) | both | `=0` |
| b458 **CAPLAT**: call- and game-like connections skip the PICKCAP queue; `pickcap_*` / `join_*` counters in `status.json` and a `b458:` log line every 30 s while they move | `HS2_B458_CAPLAT` (on) | both | `=0` |
| b450 **close pacing**: a burst of connection closes is torn down at 500 x cores a second (2.5 s wait cap) so the TUN path keeps its turn | `HS2_CLOSE_PACE` (on), `HS2_CLOSE_PACE_MAXWAIT` (2.5s) | both | `=0` |
| b451 rescue placement on the read-locked fast path (same choice, no lock convoy in open storms) | `HS2_RESC_FASTPICK` (on) | both | `=0` |
| b438 the LB judge's verdicts ride a ZDU hand-off (the new process does not start "blind" for 10-16 s) | `HS2_ZDU_LBSEED` (on) | both | `=0` |
| b428 a ZDU hand-off frees the sockets the new config no longer binds (moving a user port between tunnels) | no key | both | - |
| b430 `hs2 zdu-window`: `hs2-zdu-upgrade.sh` asks the running binary whether a handover may start (one end-of-hour rule) | no key | both | - |
| b425 **freebind**: specific listen addresses bind before the address is up (IPv6 DAD, late DHCP); dials never use it | no key | both | - |
| b414/b429/b437 censorship detector: abort reports from Iran, fewer false resets, counters; still log only | `HS2_CENSOR_DETECT` (Iran on) | both | `=0` |
| b437 one summary line per certificate reload; the first log line names the running build | no key | both | - |
| b459 / b460 (lab, off): fail / batch join requests pinned to a dead old ZDU generation | `HS2_B459_DEADGEN`, `HS2_B460_DGBATCH` (off) | Iran | - |

Installer (`install.sh`, root file == `hs2-src/install/install.sh`): the rel69r installer plus the b430 hunks only:
the certbot deploy/renew hook is now a small helper, `/usr/local/sbin/hs2-reload-certs` (written by the installer
before every certbot run and when a renewal config is pointed at it), that reloads every running hs2 tunnel unit by
unit name. The old hook, `pkill -HUP -x hs2`, matched no process once the binary had a versioned name, so a renewed
certificate loaded only at the next restart. `kill_this_tunnel` finds the tunnel's process by its command line
instead of `pgrep -x hs2` for the same reason. Nothing else in the menu changed.

Lab results (production-like harness, rel69r-rc14 vs the previous production build rel69r-rc12):

Paired A/B runs (ABBA), 95 % confidence intervals; B = rel69r-rc14 with no extra environment.

- ZDU exit at 12k users (4 pairs): ping max 0-2 s after the exit -577 ms, 2-4 s -390 ms, 4-8 s -66 ms; call loss
  -0.70 pp (0.7..1.7 % -> 0.05..0.09 %); game frames lost -55; crowd max -823 ms; Iran peak CPU -4.9 cores.
  Calls p99, crowd p99, crowd failures (0/0), steady-state calls and Kharej CPU: equal.
- ZDU exit at 24k users (2 pairs, wide intervals): the same direction (ping max after the exit ~200 vs 380..2000 ms,
  lost pings 1-2 vs 37-86, game frames lost -111, Iran peak CPU 9 -> 4 cores). At 24k the pace ends 1-2 s after
  the deadline (no user-visible effect seen); a rate fix is planned for the next release.
- Link faults under load (6 pairs; a link killed every 4 s, 15 % of links blackholed every 12 s, 37 ms netem,
  0.2 % loss): lost connections Iran 193 -> 10, Kharej 67 -> 33, crowd open failures -22 %; download, upload,
  CPU equal.
- Steady state (5 more runs, both builds at the same time): no difference between rel69r-rc12 and rel69r-rc14.
- Release gate on `2b66b136`: `go test ./...` passed with `HS2_ZDU` unset and `=1` (0 FAIL); -race on the touched
  engine areas passed in both ZDU states.

Release binary: built from `2b66b136` with
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTag=rel69r-rc14" ./cmd/hs2`,
static. The public Go source differs from that commit only in redacted addresses (tests and comments) and
in the left-out `hs2-src/lab/` files (only `lab/netsim` is kept).

Rollback to rel69r behaviour: the keys in the table (`=0`), or run the rel69r binary (sha256 58982b22…). If ZDU is
on, hand over to the older binary with `hs2-zdu-upgrade.sh` as usual (both are ZDU-capable).

## rel69r — GitHub release 2026-11-20: rel69q + zero-downtime upgrade (ZDU, opt-in) + censorship detector (Iran, log only) + STALLPICK + saved peer IPs for the reality front + auto6 default-route fix + L3 duplicate filter per process

Source = rel69r-rc7 `859f2810` (the build commit; later commits on that branch add files under `hs2-src/lab/` only).
rel69r = rel69q + b372/b396 zero-downtime upgrade + b399/b406 STALLPICK + b382 censor351 detector (level 1) + b411
saved peer IPs and /64 exemption + the rc4 auto6 fix + the rc6 uninstall cleanup + the d421 L3 duplicate filter.
envdef 135 -> 138 keys (`HS2_B399_STALLPICK`, `HS2_B399_STALL_MS`, `HS2_CENSOR_DETECT`). One new **optional**
capability bit (ext bit 15, ZDU: a peer announces that it can take part in a handover; older peers ignore it). No
other wire change: the two sides can be upgraded in either order (Kharej first, as usual).

| change | key (default) | side | off / escape hatch |
|---|---|---|---|
| **Zero-downtime upgrade** (ZDU): the running process hands its listeners and tun device to a new binary (SIGUSR2 + `/run/hs2/UNIT.next`), new users go to the new process, existing users drain on the old one until they end or the next HH:03 UTC; 45 s probation with automatic revert. Driven by `hs2-src/install/zdu/` (`hs2-zdu-install.sh` turns it on with one cold restart, `hs2-zdu-upgrade.sh` hands over, `hs2-safe-deploy.sh` is the cold path with revert). Refused HH:00-HH:03 UTC, in direct mode, while a drain runs, when a peer lacks ZDU, or when the unit's `HS2_*` environment differs from the running one. README: *Zero-downtime upgrade* | `HS2_ZDU` (**off**; `31-zdu-env.conf` sets `=1`) | both | `hs2-zdu-install.sh --remove UNIT` |
| d421: the L3 (`l3mtcp` full-tunnel) duplicate filter keeps one window per sending process (the peer's ZDU instance id) and the second copy rides a link of the same peer process, so a handover no longer delivers duplicate packets (`ping` `DUP!`, repeated UDP). Lab: rel69r -> rel69r handovers 0 duplicates; an older receiver still sees a few during the other side's handover | no key | both | none |
| b399/b406 **STALLPICK**: new user opens rank a silent path's links last (no read on any link of the path for `HS2_B399_STALL_MS` and >= 2 links with unanswered writes, another path healthy, the path up and reading in the last 10 s) before the LB judge's cut verdict; nothing is closed or moved; a marked path is no call-move target. Lab (sk405): reconnect-wave open failures -97 %, storm call tails within the A/A noise | `HS2_B399_STALLPICK` (**on**), `HS2_B399_STALL_MS` (300) | both | `HS2_B399_STALLPICK=0` |
| b382 **censorship detector** (censor351 v1, level 1): read-only hooks feed a bounded event ring; every 5 s episodes per Iran IP are classified (9 censorship and 7 ordinary classes, with a confidence). **Log only**: `censor351: v1 ep=… class=… conf=…` lines (max 30/min), `censor` status block, `hs2 status --censor`; feeds no LB, dial, close or timing decision | `HS2_CENSOR_DETECT` (alias `HS2_CENSOR`; **Iran on, Kharej off**) | Iran | `=0` (no goroutine, no allocation) |
| b411 reality peer exemption, fixed: the IPs that authenticated a link are saved to `<config>.peer-ips` (0600, 7 days, at most 256) and loaded as static exemptions at the next start (production showed `static 0`: the listening side's config does not list the dialer's source IPs, so after a restart the other server could hit the 64-per-/24 pre-hello cap); an IPv6 peer is exempt by its /64 (an `auto6` move stays exempt). Counters `reality.peer_exempt` (`static`, `saved`, `learned`, `bound`, `exempted`, `capped`) | `HS2_B388_PEEREXEMPT` (on) | both (listening side acts) | `=0` |
| rc4 **auto6** default-route fix: `auto6` takes only a global IPv6 of the interface with the default v6 route (or the named `auto6:IFACE`); a WARP / WireGuard /128 (`wg10`, rotates at each reconnect) is never taken; when the default route goes away the current address is kept (logged once) | no key | the side whose lb list says `auto6` | name the interface (`auto6:eth0`) or a literal IP |
| rc6 installer: `bash install.sh uninstall` and tunnel delete also remove `<config>.peer-ips` (2 lines in `remove_tunnel`) | - | both | - |

Installer: the rel69q installer plus only the `.peer-ips` cleanup in `remove_tunnel` (sha256 in `install.sh.sha256`; the
root `install.sh` and `hs2-src/install/install.sh` are the same file). The menu, the live load-balancing screen and the
Alerts (Telegram + watchdog) screens are unchanged. The ZDU scripts are new files under `hs2-src/install/zdu/`; the
installer does not install or call them.

Lab results (production-like harness):
- rc6 vs rel69q-rc2 under the exact production tunnel-1 drop-ins, swapped A/B plus A/A: equal on calls, crowd, opens,
  UDP, ICMP, storm rebuild, CPU and RSS.
- ZDU handovers on a production-shaped reverse `l3mtcp` pair under load (ping, UDP, 4 calls, a crowd of 1500): every
  handover OK in 49-55 s, 0 real loss (UDP 0 of 44800, 0 calls lost, 0 crowd connect failures), 0 panics; rel69r ->
  rel69r 0 duplicates.
- 2x scale (two hosts): rel69r = rel69q within noise (call p99 527 vs 777 ms, ICMP loss 0.40 vs 0.54 %). A 4x run
  overloaded both builds equally on the lab hosts, so 4x is not proven by the lab yet.

Release gate on the build commit: gofmt and `go vet ./...` clean; `go test ./...` passed with `HS2_ZDU` unset and `=1`;
-race on the L3/dup/ZDU/censor/STALLPICK/peer-exemption engine tests (140) passed in both ZDU states; the installer
tests passed (29/29).

Release binary: `hs2-linux-amd64` sha256 `58982b229788aeb5e9211a1c9dd89219c75ec0dd2b29f3fe5bbce8407709f102`,
`hs2 version` shows `[build rel69r-rc7]` (the exact production bytes are published). Built from an LF `git archive`
of `859f2810` with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTag=rel69r-rc7" ./cmd/hs2`,
go1.27.3, static. The public Go source differs from that commit only in redacted addresses (tests and comments).

Rollback to rel69q behaviour: `HS2_B399_STALLPICK=0` (both), `HS2_CENSOR_DETECT=0` (Iran), `HS2_B388_PEEREXEMPT=0`
(both; also drops the saved peer IPs), turn ZDU off with `hs2-zdu-install.sh --remove UNIT` before going back to a
pre-rel69r binary, or run the rel69q binary (sha256 13a120a4...). auto6's default-route rule and the per-process
duplicate filter have no key.

## rel69q — GitHub release 2026-11-19: rel69p + sec370 security phase 1 + b352 watchdog rules (`hs2 watch`) + b388 reality peer exemption + b375 records-rescue cover (off) + sd381 fast stop

Source = b389 `1c832439` (the rel69q-rc2 build commit); the commits after it add files under `hs2-src/lab/` only.
rel69q = rel69p + sec370 security phase 1 (b370, with the b376/b378/f379 review fixes) + b352 watchdog rules
(`hs2 watch`) + b388 reality peer exemption + b375 records-rescue cover and ring floor (both **off**) + the sd381
shutdown fix. envdef 132 -> 135 keys (`HS2_B388_PEEREXEMPT`, `HS2_B375_RRCOVER`, `HS2_B375_RINGFLOOR`). **No wire
change and no capability bit**: every new behaviour is local to one side (b388 on the listening side; b375 uses the
existing RREC frames), so the two sides can be upgraded in any order (kharej first, as usual).

| change | key (default) | side | off / escape hatch |
|---|---|---|---|
| b388: the reality front's pre-hello cap (64 per client /24 before the ClientHello) exempts peer IPs (the other side's lb IPs, re-read on reload, and IPs that authenticated a link in the last 24 h); each peer IP has its own bound 2*max_links+8. Probe answers unchanged. Status `reality.peer_exempt`, reject reason `prehello_cap_peer` | `HS2_B388_PEEREXEMPT` (**on**) | both | `=0` |
| b375: an er97 records rescue covers the exit's missing range from the retired subs' queued / in-write / kept chunks (held by reference) instead of ending the conn (nocover). Off since rel69q-rc2 (lab: drop-storm opens +30-40 % in 3 runs with it on; it never extended a rescue in any lab run) | `HS2_B375_RRCOVER` (**off**) | both | `=1` on |
| b375: records ring floor (cap >= the conn's in-flight bytes) + young window | `HS2_B375_RINGFLOOR` (**off**) | both | `=1` calls + heavy queue, `=2` all |
| sec370 verify-before-exec: `hs2 tunnel renew --upgrade-from BIN` reads BIN once and checks its sha256 against `--sha256 HEX` or `BIN.sha256` **before** anything runs; BIN itself is never executed (the verified copy is staged and answers `version`). `BIN.sha256` is trusted only when BIN and it are root-owned regular files in a directory that is not group/world-writable (and on ext4/xfs/btrfs); otherwise pass `--sha256`. A mismatch is always refused | on | both | `HS2_ALLOW_UNVERIFIED=1` (no expected hash: allowed, full sha256 printed) |
| sec370 `HS2_PPROF=unix` / `unix:/abs/path`: profiles on a root-only 0600 unix socket (default `/run/hs2/pprof-<pid>.sock`); loopback TCP still accepted for labs; `/debug/pprof/cmdline` removed | off (unset) | both | `HS2_PPROF_CMDLINE=1` puts cmdline back |
| sec370 heap dumps (`/debug/hs2heapdump`) go to a 0700 dir on disk, files 0600 O_EXCL, refused when free space < heap (was a fixed world-readable `/tmp` path) | `HS2_HEAPDUMP_DIR` (`/var/lib/hs2/heap`) | both | set another dir |
| sec370 join code kept out of argv: `hs2 tunnel join` with no code reads it at a no-echo prompt (stdin when not a terminal), `hs2 tunnel join -`, `--code-file FILE`; `hs2-menu` hands it over on fd 3 | on | both | the argv form still works (prints a warning) |
| sec370 permissions: `/etc/hs2`, `/var/lib/hs2`, `/run/hs2` dirs 0700, files 0600 (status files, 24 h stats, configs, history); `bash install.sh upgrade` tightens files left by older releases (`fix_perms`, never follows symlinks, ACME webroot untouched); `hs2 doctor --perms` lists loose files, `--perms-fix` fixes them (also a numbered deep-doctor fix) | on | both | `HS2_RUN_SHARED=1` (old world-readable `/run/hs2` + stats for a local non-root reader; leaves the marker `/run/hs2/run-shared` that the installer and doctor respect), `HS2_KEEP_PERMS=1` (installer upgrade does not tighten) |
| b352 watchdog rules in the existing 60 s alert check (`hs2 watch` = `hs2 alert`): `stale`, `loss`, **`dark`** (a path that carried links in the last 24 h drained to 0 links / target 0 with loss >= `dark_loss_pct` 50 % or cut, 3 checks, critical, resolves below 20 %), `disk`, `mem`, `cpuidle`, `usersdrop` (skips minutes `00-06` of each hour), `subdied`, `newerr`, optional `tprobe*` / `peer` probes and auto-`rollback` (off). Menu: Monitoring -> Alerts -> w | on where alerts are on; probes + rollback off | both | `disable=NAME`, `hs2 alert rule NAME off`, threshold `=0` |
| sd381: a shutdown closes the carrier (`context.AfterFunc`), so an idle udp/auto/noise dial side stops in ~50 ms instead of up to the 3 s exit guard (l3mtcp was not affected) | no key | both | none (stop path only) |

Installer: the rel69p installer plus the sec370 / b352 parts of b389: menu **Alerts -> w** (watchdog checks:
probes, disk/RAM/CPU-idle thresholds, auto-rollback), `fix_perms` on `upgrade` (`HS2_KEEP_PERMS=1` skips it,
`HS2_RUN_SHARED=1` or its marker leaves `/run/hs2` alone), and the menu's join hands the code to hs2 on fd 3
(`--code-file /dev/fd/3`; an older binary without `--code-file` gets it in argv as before). `install.sh` and
`hs2-src/install/install.sh` are the same file.

Lab results (production-like harness: a crowd of light users, 8 call streams, a game, ping, heavy flows, path
cuts, a data-dark path, hourly-style reconnect storms and drop storms; A/B arms in parallel on split CPU halves,
swapped between runs, plus A/A runs for the noise floor):
- rel69q-rc1 vs rel69p-rc3, 5 swapped pairs: equal within the A/A floor on every metric — ICMP per-minute p99 223
  vs 240 ms, crowd p99 274 vs 283 ms, call per-stream p99 385 vs 367 ms (stream-paired 20/20 of 40), call stalls
  >= 1 s 26 vs 25 events, heavy up/down within noise (Wilcoxon p 0.32 / 0.41); CPU, RSS, goroutines and fds equal;
  0 panics, 0 open failures in storms.
- sd381: an idle udp dial side under `systemctl stop`: median 3046 ms -> 51 ms (10 stops each).
- Drop-storm opens: rc1 with RRCOVER on was slower in 3 runs (p50/p99 +30-40 %), so rc2 ships it off; rc2 vs rel69p
  in the same hour: ratios 0.90 / 0.96 / 1.05 against an A/A floor of 1.16 -> equal.
- Found, not a regression (present in rel69p too): a one-way data-dark of the link that carries the other side's
  call primary can stall that call for up to ~8 s; a fix is queued for a later release.

Release gate on the build commit: gofmt and `go vet ./...` clean, `go test ./...` passed (engine 961 s), -race on
cmd/hs2, envdef and the rescue/defaults engine tests passed on rc2; the full `-race ./engine` ran on rc1 (identical
code except the RRCOVER default; one load flake, 4/4 on rerun); the installer test scripts passed.

Release binary (production tunnel 1 since 2026-11-19 13:06 UTC, tunnel 2 since 16:07 UTC): `hs2-linux-amd64` sha256
`13a120a433b91f716595c3005975f262f4705a4e7644ed469301e791bab6f5ed`, `hs2 version` shows `[build rel69q-rc2]` (the
exact production bytes are published, so the stamp is the release candidate's name). Built from a `git archive` of
`1c832439` (LF) with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTag=rel69q-rc2"`,
go1.27.3, static; the build is reproducible (same command, same sha256). The public Go source differs from that
commit only in redacted server addresses (tests and two comments); the public installer additionally keeps the rel69p
live load-balancing screen and its menu test (installer only, not in the binary).

Rollback to rel69p behaviour: `HS2_B388_PEEREXEMPT=0` (both), `HS2_RUN_SHARED=1` (units and stats timer),
`HS2_KEEP_PERMS=1` for the installer, `HS2_ALLOW_UNVERIFIED=1` for a hash-less `--upgrade-from`, `disable=` the
watchdog rules you do not want, or run the rel69p binary (sha256 c0e04036...). RRCOVER and RINGFLOOR are already
off; the sd381 stop fix has no key.

## rel69p — GitHub release 2026-11-19: rel69m + rel69n scale/locking + rel69o exit watchers, auto6 + rel69p pick list, path-stall handling, spare per path, cut-mark fixes, call move, fast darkcall switch

rel69n and rel69o ran in production but were never published on their own; this entry covers **rel69m -> rel69p**.
Source = b356 `abda029a` (the rel69p-rc3 build commit); the lab-only commits after it add files under `hs2-src/lab/`
only. envdef 111 -> 132 keys. No capability bit. Two optional additions on the wire, both ignored by older peers:
the edge's advert carries an optional per-path serving block (`HS2_B331_SPAREPATH`; an old exit drops it, an old
edge sends none and a new exit behaves as before), and the LB report carries an optional per-path `ok` field
(`HS2_B340_CUTCLEAR`, JSON `omitempty`). Upgrade the kharej (exit) first, then Iran.

Every key below is on by default unless noted; `=0` turns it off (on the side listed).

| change | key (default) | side |
|---|---|---|
| rel69p: user connections pick from a short per-path candidate list (best 16 per steering group + threshold, stripe holders always in) instead of scanning every link under the read lock | `HS2_B327_PICKLIST` (1); tuning `HS2_B327_K` (16), `HS2_B327_SLACK` (0) | both |
| rel69p: a link on a path that is stalled as a whole is not judged stuck before 20 s (stops the stuck-drain storm after a one-direction cut) | `HS2_B337_PATHSTALL` (1) | Iran |
| rel69p: stall hysteresis — once a path stalled whole, its links stay held from stuck verdicts for 20 s while part of the path answers again | `HS2_B339_PATHHYST` (1) | Iran |
| rel69p: cut grace for drain reclaim (no user gain measured while `HS2_LB_EVAC` is off) | `HS2_B339_CUTGRACE` (**0**, `=1` on) | Iran |
| rel69p: the exit dials one spare link for a recovered PATH that is >= 2 links under its share (and whose Iran IP is under the edge's target), 1/path/30 s, 6/min | `HS2_B331_SPAREPATH` (1) | both |
| rel69p: rescue-cut per-connection backoff — the first drop is taken, each further drop on the same connection within 30 s waits 0.5 s << n (max 16 s) | `HS2_B338_RC_GAP0_MS` (500; `=0` off) | both |
| rel69p: light / call / rescue placement skips links marked by rescue-cut, using them only when no unmarked link exists | `HS2_B340_SKIPCUT` (1) | both |
| rel69p: a cut path whose raw readings are clean again (3 in a row) stays lowered but is no longer cut-marked | `HS2_B340_CUTCLEAR` (1) | both |
| rel69p: a call whose lossiest copy rides a path >= 0.15 % worse than the best free path for 10 s opens one fresh sub there and gives the old copy up once the fresh sub read data (1/call/30 s, 20/min) | `HS2_B336_CALLMOVE` (1) | Iran |
| rel69p: darkcall switches a call's primary with bytes in flight and no ack progress for max(3 x srtt, 300 ms) to a live copy on another path with recent ack progress, without waiting for RTO backoff | `HS2_B342_DCFAST` (1) | both |
| rel69p: call path log (log only) | `HS2_CALLPATH_LOG` (**0**, `=1` on) | Iran |
| rel69o: an exit sub gets no watcher goroutine; the same cleanup runs once from the sub's close or the connection's end (fewer parked goroutines in reconnect storms); `/debug/hs2sched` counters on the pprof listener | `HS2_B322_EXITNOWATCH` (1) | kharej |
| rel69o: `auto6` lb address = this server's current global IPv6, re-resolved every 10 s and on `cannot assign requested address` (hosts that get a new IPv6 at each boot); a literal source IP missing from the server is cooled 15 s .. 5 min instead of redialed in a loop | config value `"ip": "auto6"` / `auto6:IFACE`; no key | both |
| rel69o: the data-dark rescue link index is sharded (64 locks): no lock convoy in reconnect storms | no key | Iran |
| rel69n: msub.close no longer waits behind the stream's FIN; heavy connections keep the synchronous close | `HS2_B307_ASYNCCLOSE` (1), `HS2_B311_HEAVYSYNC` (1) | both |
| rel69n: per-link copy-sub counter; exit conn set and mconn table sharded into 64 locks; smux `IsClosed` is one atomic load | `HS2_B302_COPYCNT` (1); shards/atomic: no key | both |
| rel69n: TUN dup second copy kept per flow, avoids the path the first copy left, scans other paths in rank order | `HS2_B303_DUPSTICK` (1), `HS2_B308_DUPAVOID` (1), `HS2_B309_DUPSCAN` (1) | both |
| rel69n: join/stripe sub opens without the connection lock; rescue-join placement memoised and placed under the read lock | `HS2_B312_OPENNOLOCK` (1), `HS2_B313_RJMEMO` (1), `HS2_B313_RJFAST` (1) | Iran |
| lab only, not in the table | `HS2_B325_STORMPICK` (off), `HS2_B335_FASTLOWER` (off), `HS2_B337_FL_LOGONLY`, `HS2_B337_UNCUT_DEBUG`, `HS2_B336_FL_DEBUG` | — |

Installer: the rel69m installer (live load-balancing screen) plus b333: the LB menus accept `auto6` (or `auto6:IFACE`)
as this server's own address. `install.sh` and `hs2-src/install/install.sh` are the same file.

Lab results (production-like harness: crowd of light users, calls, games, ping, heavy flows, path cuts, hourly-style
reconnect storms; A/B arms run in parallel on split CPU halves, swapped between runs):
- rel69n vs rel69m: heavy throughput +2.7 % (within noise), 1 h soak equal on every metric, 0 panics.
- rel69o: storm lock wait on Iran -92 %; exit run-queue peak -16 % and ~4.5k fewer parked goroutines in storms; auto6
  follows a live IPv6 change in 0.35 s with no ping or call hit.
- rel69p pick list: storm pick CPU -25 % to -76 %; PATHSTALL removed the mass stuck-drain after a down-direction cut.
- rel69p-rc3 vs production rel69o at 2x load (8 calls + 4 games): per-stream call p99 median 1571 -> 440 ms
  (p < 0.001), streams with >= 3 s call blackouts 3-7 -> 0-2 per run, crowd failures about 3x lower; ICMP and crowd
  p99 within a few ms (replicated on a second lab host: ICMP p99 equal or better in 3/3 pairs). 1 h soak: calls 8/8
  streams better (p99 433 vs 495 ms, 1 s+ samples -47 %), ICMP/crowd/heavy/reconnect equal, 0 panics, no leak.
  Reconnect-storm worst open time and heavy download: no difference beyond run-to-run noise.

Release gate on the build commit: gofmt and `go vet ./...` clean, `go test ./...` and `go test -race ./engine`
(and -race on every other package) passed, 0 DATA RACE.

Release binary (production tunnel 1 since 2026-11-18 22:46 UTC): `hs2-linux-amd64` sha256
`c0e040362f76c1ccc8b9fbf6705d8ad848715d6cea9693fa4c3e0946ee9dcb63`, `hs2 version` shows `[build rel69p-rc3]` (the
exact production bytes are published, so the stamp is the release candidate's name). Built from a `git archive` of
`abda029a` (LF) with `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTag=rel69p-rc3"`,
go1.27.3, static. The public tree differs from that commit only in redacted server addresses (tests and two
comments), so a rebuild from it differs only in the Go/GNU build-id bytes.

Rollback to rel69m behaviour: turn the keys above off (`HS2_B327_PICKLIST=0 HS2_B337_PATHSTALL=0 HS2_B339_PATHHYST=0
HS2_B331_SPAREPATH=0 HS2_B338_RC_GAP0_MS=0 HS2_B340_SKIPCUT=0 HS2_B340_CUTCLEAR=0 HS2_B336_CALLMOVE=0
HS2_B342_DCFAST=0 HS2_B322_EXITNOWATCH=0 HS2_B307_ASYNCCLOSE=0 HS2_B303_DUPSTICK=0 HS2_B308_DUPAVOID=0
HS2_B309_DUPSCAN=0 HS2_B312_OPENNOLOCK=0 HS2_B313_RJMEMO=0 HS2_B313_RJFAST=0 HS2_B302_COPYCNT=0`) or run the rel69m
binary (sha256 5e8fc8bc...). The shards, the atomic smux close flag and auto6 have no key.

## rel69m — GitHub release 2026-11-18: rel69l + gc295 allocation/GC cuts + b296 scale fixes + gc296 rescue ring pool + live LB screen (installer)

rel69m = rel69l (b291 814c68a0) + gc295 (b295 3264fb3a) + b296 (f7cf5926 a5acbcdc 798a7075 cfe2ade3), cherry-picked on
branch b299 with no conflict (the lab-only sc4x commits 138f1f54/d13c7199 are left out), + gc296 (b295 a28be0c8,
cherry-picked on branch b300, no conflict; lab-only gc297 harness left out). envdef 109 -> 111 keys
(`HS2_B296_DCSKIP`, `HS2_GC296_RINGPOOL`, both). No capability bit, no wire change; behaviour is identical by design except the darkcall idle skip.

| change | side | disable / old behaviour |
|---|---|---|
| gc295 (no key): hot-path allocation cuts — typed smux shaper heap (container/heap's exact up/down order, equivalence test), pooled smux request ctx (result chan + reqOwn per frame; a given-up request is left to the GC, rc134 unchanged), msub.writeLoop batch chunk list reused per writer, 64 KB batch buffer pooled when a writeLoop ends, alloc-free TCP_INFO (struct by value). lab2 gc295 storm harness: alloc ir 1415/1702 -> ~800/1260 MB/min steady/storm (vs bin291), kh more; 1 h parallel soak vs rel69l: GC cycles/min Iran -27 %, Kharej -55 %, heap/RSS/goroutines/fds flat, latency equal, 0 panics, 0 corruption signs | both | none (rebuild/roll back the binary) |
| b296 `HS2_B296_DCSKIP=1` (default on): the darkcall timer skips a non-call conn proven idle (nothing to do on its last visit, every live sub consumed by the peer, no write/new sub/call roll since: conn write sequence dcWr unchanged); calls never skipped; every 25th tick (5 s) visits all conns and only that tick takes proofs. The cs226 lite gate tests the link (cached meter, msub.dcMtr) before the stream's ack marks: same verdict (20k random sub mixes, TestB296LiteOrderSame) | both | `HS2_B296_DCSKIP=0` |
| gc296 `HS2_GC296_RINGPOOL=1` (default on): rescue ring buffers (raw/rescue rings, rrBig rings) come from a sync.Pool per power-of-two size class (4 KB-8 MB) and go back when the ring is dropped, instead of make/drop each time; every ring byte still counts against the existing budgets (rescue mem cap, `HS2_UL268_MEM_MB`, rrBig RAM-scaled) and pooled buffers live at most 2 GC cycles. lab2 5v5 storm pairs vs gc295: alloc -26..-50 %, GC cycles kh -32 %, STW max equal or lower; Iran RSS +137 MB (bounded); gc297 1-s storm-onset bins (5 more pairs): onset spike present in both arms, pool effect within noise; 0 panics, 0 fails | both | `HS2_GC296_RINGPOOL=0` |
| b296 (no key): exitTraffic.sample copies the conn set under t.mu (reused slice) and does the per-conn math outside it, so add/done of new exit streams no longer wait for a full scan every 2 s | both | none (rebuild/roll back the binary) |

lab3 sc4x (prod-like scale harness): 1x (crowd 3000), 7 interleaved b296 vs rel69l pairs: call p99 +2.7 % [CI -87, +119 ms],
crowd p99 -3.6 % [-55, +30], CPU ir+kh -1.6 %, all neutral; 1.5x (crowd 4500): b296 equal or better on call/crowd p99,
stall seconds and darkcall switches; 0 fails, 0 panics.

Rollback to rel69l behaviour: `HS2_B296_DCSKIP=0` and `HS2_GC296_RINGPOOL=0` (both sides); gc295 and the b296 exit-stats/gate-order changes have no
key, so a full rollback is the rel69l binary (sha256 5674c6bb...).

Installer (b304, `install.sh` 5ee7cf4b, no binary change): the classic "Load balancing — live status" table is replaced by a
live load-balancing screen — one row per Iran IP sorted by traffic (SHARE %, LINKS live/target, down/up Mbit/s, STREAMS,
LOSS % = the worse of down and up with `N↑` for upload loss, RTT, path MTU from mt135 with the lower direction and a red mark
under 1280, auto CUTS in the last 24 h from `hs2 lb history`), cut/idle IPs on one dim line, Kharej IPs, LB verdicts, keys
p/m/k/h/Enter, in-place redraw, narrow terminals drop columns, ASCII fallback. Reachable from the new menu (3 Load balancing
& IPs -> l Live view), the classic LB screen (9) and the shell: `hs2-menu lb [tunnel] [--once] [--view V]`; install/upgrade
write the wrapper `/usr/local/bin/hs2-menu-lb` (exec `hs2-menu lb`), uninstall removes it. Read-only; every column equals
`hs2 lb show --status` and its JSON (checked on a live 12-path + IPv6-pair lab tunnel). `hs2-src/install/tests/lb_menu_test.sh`
covers the layout, MTU, cuts, parks, the CLI and the wrapper.

Release binary (the one both reference production tunnels run since 2026-11-18): `hs2-linux-amd64` sha256
`5e8fc8bc562ec1e9f525db0888b1c6830ab9e870f7be763e24b4dd4038fd82a2`, `[build rel69m]`, from a `git archive` of b300 0882a109
(LF), `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTag=rel69m"`, go1.27.3, static.
gofmt and `go vet ./...` clean, full `go test ./...` and `go test -race ./engine` passed on that tree (0 DATA RACE). The
public `hs2-src/` adds only the b304 installer and its test on top of that tree (no Go change).

## rel69l — rel69k + ul268 data-dark raw rescue + b278 TUN ping stall fix + ul281 upload stripe slot + cs286 call copy apart (never published on its own; included in rel69m)

rel69l = rel69k (b276, incl. b277/b278 test race fixes) + ul268 from b268 (f8b18c7f ff88a84b f1080f1f 856182cf eed2720e
156c15c7 68306cc7 a7fb4446 99a9adb4 271e45e7 b6d3db1a f2154bfe ce858299, incl. lab/ul268s harness), cherry-picked on branch
b279 with no conflict; b278 (34868ab5 2ec4d1cf 7776d489 0a8e3987, TUN interactive flows off a stuck link, dup copy on another
path) cherry-picked on branch b280 with no conflict; ul281 (d6de38f9, extra stripe slot for a new fast upload) cherry-picked
on branch b285 with no conflict, default flipped on; cs286 (b286 174d998e c7e0eb66 1403c099 163e1f78, call copy sub never
on the path of the call's other sub) cherry-picked on branch b289 (docs rows merged by hand). envdef 105 -> 109 keys
(`HS2_UL268_RESCUE`, `HS2_UL281_SLOT`, Iran; `HS2_B278_L3STALL`, `HS2_CS286_CALLAPART`, both); `HS2_UL268_HOLD_MS` is an in-code default; `HS2_B278_TRACE` is a lab-only trace.
No capability bit, no wire change; ul268/ul281 act only on Iran, b278/cs286 on both sides. Calls are never touched by ul268 (dcIsCallLocked -> er92 set only; the
stripe evac skips calls; darkcall dc254/b262/b270/xs261 keep owning calls).

| change | side | disable / old behaviour |
|---|---|---|
| ul268 `HS2_UL268_RESCUE=1`: a link whose data is frozen (TCP_INFO unacked/bytes_acked unchanged >= max(1 s, 8 x srtt), or backoff >= 2) on 2 consecutive 250 ms ticks while another path still acks joins the rescue set; the Iran non-bulk raw conns on it are rescued when their ring covers SIOCOUTQ + 64 KB, a stripe sub on it is closed (stripe RESEND moves its bytes); a fast raw writer's ring grows to half a second of its rate (<= 8 MB/conn, <= min(256 MB, RAM/16)/process). lab2 pv270b (b268 vs bin264, parallel CPU-split A/B): pathdark crowd fails -62/round, no-fault icmp p99 -2..-5 ms, call tail neutral, 0 panics | Iran | `HS2_UL268_RESCUE=0` |
| b278 `HS2_B278_L3STALL=1`: a TUN interactive flow (ping/ICMP, small packets) leaves a link whose socket has data in flight with no ack progress (RTO backoff, or bytes_acked still for max(400 ms, 3 srtt); TCP_INFO read at most every 50 ms per link, only for links an interactive packet is about to use; a read older than the window restarts it, so a rarely read idle link with one packet just sent is not called stuck), and the HS2_L3_DUP second copy skips such a link; `HS2_B278_TRACE=1` (lab) logs every ICMP echo with its link paths | both | `HS2_B278_L3STALL=0` |
| ul281 `HS2_UL281_SLOT=1` (default on): a NEW fast upload (raw sample >= 80 Mbit/s, raw conn younger than 20 s, first growth only) whose growth finds all 8 stripe slots held may take one of the 4 dlfair extra stripe slots (shared with dlfair/dl241, bounded at 12 per process); before, every upload was refused and stayed on its raw first link. lab2 ul281d prod-like harness (12 paths, ~130 links, 3000 crowd, 30 bg users), 10 interleaved off/on pairs: upload 229 -> 327 Mbit/s (+98, 95 % CI [+72, +123], 10/10), light p99 diffs ~0 (crowd -0.4 [-9, +9], icmp +0.8 [-10, +11], echo -0.3, call -1.5 [-15, +12]), 0 fails, 0 panics. No wire change | Iran | `HS2_UL281_SLOT=0` |
| cs286 `HS2_CS286_CALLAPART=1` (default on): a call's copy/pair sub (call upgrade, pair subs, raw upgrade of a call, call rescue) whose pick rides a path one of the call's other subs rides is re-picked on another path — healthy first, then a lowered/cooled one, then a degraded one (never draining/retiring/suspect); covers the plain-pick fallback when the call placement finds no light link (lab1 sk280 SN4: after a late lb auto2 cut verdict the copy went beside the primary, cs229 never ran, the next cut of that path stalled the call 11 s); log "cs286 kept/moved (low)/nocand" in the mconn call 10 s line | both | `HS2_CS286_CALLAPART=0` |
| zs291 (0d98ccbb, bug fix, no key): `Close()` ends at once an mconn that never had a sub (nsubs==0). Before, a raw exit conn (no manager, no raw1) whose first sub lost the reserve race to a ul268 rescue join only set closing, end() never ran and the exit `mem: mconns` counter (obs.mconnOpen) crept up at each data-dark event (accounting only; the object was collected, RSS flat). `HS2_ZS291=1` is a diagnosis-only mem-line extension (live/zero-sub conns, neverended, reserve-lost by role), default off, not in envdef | both (seen on Kharej) | none (fix) |

Rollback to rel69k behaviour: `HS2_UL268_RESCUE=0` and `HS2_UL281_SLOT=0` (Iran), `HS2_B278_L3STALL=0` and `HS2_CS286_CALLAPART=0` (both sides).

## rel69k — GitHub release 2026-11-17: rel69j + b261 xs261 + b262 coverage fixes + b270/b271/b274

rel69k = rel69j (b264 c9d3d835) + b261/b262 (cherry-picked from b262 after its b254 base 5e2a7fb6: ca76db7a xs261 + cs224
left-mark fix, 6999c9ba claim/ring, b17514eb darkwait cover, 1f9f0f56 call pair, 54990528 wait stats, 46c439d2 ringlag
counter, 8e476bb5/a561eb82/9e7c4aa2 replace prototype default off). Conflict only in mconn_darkcall.go: a call visit runs
dc254's silence check, xs261's silent-receive check and the b262 replace check; a lite visit keeps b264's dc254 leave.
envdef 99 -> 102 keys (`HS2_XS261`, `HS2_B262_COVER`, `HS2_B262_REPLACE`); `HS2_B262_WAIT_MS` is an in-code default (500 since b266; was 0).
Wire: one new record (0x15, XS) behind a new capability bit (kindInfo ext bit 14); it is sent only on a link whose peer
announced the bit, so rel69i/rel69j and older (r54c) peers never receive it; XS is parsed with the key off too.

| change | side | disable / old behaviour |
|---|---|---|
| xs261 `HS2_XS261=1`: cross-side call switch (a one-direction cut seen by one side moves both sides' primaries); b258's cs224 re-chosen sub fix | both | `HS2_XS261=0` |
| b262 `HS2_B262_COVER=1`: darkcall switch coverage (provable held claim, ring short only for provably unconsumed misses, "ringlag" counter otherwise), never a chunk's only copy on a dark sub, call pair never leaves a chunk on dark subs only, darkwait episode stats | both | `HS2_B262_COVER=0` |
| b262 `HS2_B262_WAIT_MS` (default 500 ms since b266): caps one darkwait episode; darkwait stats add an episode histogram (<10/<100/<500/<1000/>=1000 ms), per-class long (>=500 ms) counts and up to 300 "b262 darkwait long" lines (sub flags at start and end) | both | `0` (unbounded) |
| b262 `HS2_B262_REPLACE` (default off): Iran replaces a stale call sub left dark >= 2 s with >= 16 KB queued | Iran | default off; `=1` on |
| b270 `HS2_B270_TXRES=1`: a 2-sub call's copies that went only on a half-dead non-primary sub (dc254 txDark, or its link receives nothing while the conn's receive advances) are resent on the live sub at the edge and every 300 ms while it lasts (lab3 ab270 data-dark: 7-10 s call stalls gone, 4/19 -> 0/16) | both | `HS2_B270_TXRES=0` |
| b274 `HS2_B270_RECENT_MS` (default 600000, in effect unbounded): a b270 resend takes only chunks sent within the window; 3000 neutered b270 on lab1 one-direction cuts, kept off until a lab3 data-dark check | both | (default = b270) |
| b274 `HS2_B274_REPAIR=1`: a call conn left with one live sub re-opens its second at the next manager tick when call-like within 5 s (not after the lite sampler's two steady windows) | Iran | `HS2_B274_REPAIR=0` |
| b266 `HS2_DL259_PATHQ` default now `0` (prod same-hour A/B halved single-stream DL with it on); code and key kept | exit | `=1` on |
| b271 `HS2_JM271_RST=1`: the exit answers a missed plain join with an RST record (500/s, burst 2000); lab3 rs271: after an exit restart 70-700 of 1000 long-lived conns hung for 85+ s and Iran re-joined dead conns ~300/s forever -> 0 hung, joinmissed flat after 10 s | kharej | `HS2_JM271_RST=0` |
| b271 logs: dc254 silent rx split into link-evidence vs app silence (txdark lines own budget); sub resend "no fresh target" split into lost vs survived; retiring/back-in-service lines: first 3 per minute verbatim, rest in one per-minute summary | both | (logs only) |
| b276: `HS2_B262_COVER` read through an atomic (b262CoverOn): the global darkcall ticker read a plain var tests flip (DATA RACE, fix from b272) | both | (no behaviour change) |

b276 (rel69k build base): b274 ee4d46b9 (c14fd959 + b270 + DL259 off + b274) + b271 (8cba0500, c062fa2b, 60659471, 1ea19806 + rs271 harness) + the b272 atomic b262CoverOn fix. envdef 102 -> 105 keys (`HS2_B270_TXRES`, `HS2_B274_REPAIR`, `HS2_JM271_RST`); `HS2_B270_RECENT_MS` is an in-code default. ul268 (b268) not included (separate cherry-pick).
Rollback to rel69j behaviour: `HS2_XS261=0 HS2_B262_COVER=0 HS2_B270_TXRES=0` (both), `HS2_B274_REPAIR=0` (Iran), `HS2_JM271_RST=0` (kharej).

b277 (eb049dd9) and b278 (8b2702b6), race fixes with no behaviour change: the env-derived feature bools that tests toggle
while mconn goroutines read them (cs223CallResend, cs221Darkcall, cs223DarkResend, cs224StaleSwitchOn, cs224CopyIdle,
cs225CallPath, b262RepOn, fx212SubResend, lbRescue, lbCallStuck) are atomics set once at init from the same key/default;
tests wait for every mConn manage/sub loop before they restore a switch. `go test -race ./engine` on 8b2702b6: 0 DATA RACE.

Release binary (the one production tunnels 1 and 2 run since 2026-11-17): `hs2-linux-amd64` sha256
`eb3373ffc18ab159cf18b34c7403e0e180b4a7e0da97dced14a39b2f600a403f`, `[build rel69k]`, from a `git archive` of 8b2702b6
(LF), `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTag=rel69k"`, go1.27.3,
static. Full `go test ./...` and `go test -race ./engine` passed on that tree. ul268 (b268) is not in rel69k.

## rel69j — rel69i + b255 + dc254 + dl259 + ul257 (never published on its own; included in rel69k)

rel69j = rel69j-pre (8b682c68, section kept below) + ul257 from b257 (87f87440, b0ad00cb, 4d2b488e + dr257/dr258
lab harness) + dl259 from b259 (1421a9bf, fda8c0bd, c8400dc0, 7c6a3b3a). Conflicts only in this file and in
lab/dr257/dlab.sh (b259's LOSSB form kept). Then the pre-release audit's fixes (branch b264: M1-M3, L1-L4). One more env key (envdef 98 -> 99, `HS2_DL259_PATHQ`, exit);
`HS2_UL257_STUCK` is an in-code default (Iran). No capability bit, no wire change; each side acts alone.

| change | side | disable / old behaviour |
|---|---|---|
| dl259 `HS2_DL259_PATHQ=1` default: the download stripe's 3 ms queue gate is judged per path; a path whose other busy links queue over the target is cut for that download (no chunk, no width slot) until under half the target after 2 s, and the width is judged only on the uncut stripe paths, so a download whose primary loads its own path can still widen onto unloaded paths. lab3 dr259 (same binary, key off/on): light 4+4 single-stream DL median 527 -> 894 Mbit/s, crowd p99 82.3 -> 82.7 ms, per-path light-link rtt p99 flat; heavy 3+3 (3000 crowd, 2-3 unlimited bg downloads, one 3 % loss path) DL median 207-295 -> 292-307, crowd p99 97.3 -> 97.7, icmp p99 94.2 -> 86.7, call p99 125 -> 119, concurrent bg downloads not starved, 0 panics, kh RSS +10-15 MB | exit | `HS2_DL259_PATHQ=0`, or `HS2_DEFAULTS=legacy` |
| ul257 `HS2_UL257_STUCK` (default on): a raw upload whose writer sat blocked in the first sub's Write for >= 60 % of a full 1 s sample, under 30 Mbit/s, after >= 1 MB, opens its second stripe sub at once (logs "ul257 stuck raw upload"); a sample that moved nothing counts as an upload. Cause found in lab3 dr257 (first sub on a lossy path in RTO, stripe growth needed 4 good samples). lab: no harm, no false trigger with the full-sample guard, but no stall reproduced in the A/B, so the gain is not shown | Iran | `HS2_UL257_STUCK=0` |
| rel69j-pre (dc254 + b255, below) | see below | see below |
| b264 audit M1: `HS2_DEFAULTS=legacy` now also turns dc254 off (unless `HS2_DC254=1`) | both | `HS2_DC254=1` |
| b264 audit M2: dc254's txdark/silent/txstall lines have their own budget (10 lines/min, drops counted as "log drop" in the dc254 part) and no longer use darkcall's 3000-line lifetime cap | both | (logs only) |
| b264 audit M3: dc254's txDark switch and its no-candidate records rescue run on call visits only; a cs226 lite (non-call) visit keeps rel69i's rule (switch/rescue only for a DARK primary) | both | `HS2_DC254=0` |
| b264 audit L1: a hang-up on a still-open conn closes its silent-rx episode as "silent gone" instead of counting a stall | both | (counters only) |
| b264 audit L2/L3: ul257's first full sample counts only the writer's blocked time inside its window; "ul257 stuck raw upload" is limited to 10 lines/min with a running #n | Iran | `HS2_UL257_STUCK=0` |
| b264 audit L4: dl259's per-path queues are sorted once per stripe measurement (100 ms) and per link track/untrack and shared by every gate decision (same decisions, proven against the old code on random inputs); one decision at 4x scale (400 links, 10 paths, 4 stripe subs) 44 us / 6.3 KB / 67 allocs -> 2.8 us / 0 allocs on lab2 (a rebuild 30 us, 0 allocs) | exit | `HS2_DL259_PATHQ=0` |

Rollback to rel69i behaviour (one at a time works too): `HS2_DL259_PATHQ=0` (kharej), `HS2_UL257_STUCK=0` (Iran),
`HS2_DC254=0` and `HS2_HALF_CLOSE_MAX=4096` (both sides).

## rel69j-pre — rel69i + b255 + b254/dc254 (folded into rel69j above)

rel69j-pre = rel69i (739e8677) + merge b255 (9c8292c9, audit246 lows, section below) + merge b254 (5e2a7fb6, dc254).
One textual conflict (dcConn fields: b255's liteWalks next to dc254's silent/silentFor/txStall, both kept); dc254's
darkcall-visit changes land in b255's darkcallLiteLocked split unchanged. One new env key (envdef 97 -> 98,
`HS2_DC254`), no new capability bits, no wire change. Each side acts alone.

| change | side | disable / old behaviour |
|---|---|---|
| dc254: a call primary whose sends are frozen in RTO backoff (in flight, no ack for darkcallAfter, backoff >= 2 or rt >= 2) is switched like a dark one even while its link still receives; a txDark sub is never a switch target | both | `HS2_DC254=0` |
| dc254: a copying call keeps its second copy while it was call-like within callFreeTTL and under mCallMaxRate (no drop to one copy on the first stalled window) | both | `HS2_DC254=0` |
| dc254 detector: silent-rx >= 800 ms, primary tx-stall >= 1 s, darkcall ticks >= 800 ms late; counters in the darkcall log line, runs with the key on or off | both | (counters only) |
| b255 L3/L5/L6/L7 (below) | see below | see below |
| lab/probe: the samplelog writer no longer blocks the probe (the ~10 s periodic all-class lab stall was this) | lab only | — |

Lab (dc254, lab1 one-direction cut ABAB 3+3): call samples >= 1 s 8322 -> 6196 (-26 %), 0 false switches, crowd/heavy
equal, 0 panics. Known remainder: a switch cannot resend the frozen sub's in-flight beyond the 32 KB records ring
("resent 0 covered false"), so multi-second call stalls remain until the old link's next RTO (follow-up b258).

## b255 — audit246 lows L3/L5/L6/L7 (on rel69i; not yet released)

- L6 (edge/exit queue ctl): `measure` copies the link set under r.mu and reads TCP_INFO / runs the once-per-link
  `ip route get` without it; new links' `track` (lm.OnLink) no longer waits behind a redial storm's serial execs.
- L3 (darkcall timer): a visit that needs no cs228 prefetch reuses the cs226 lite verdict (one walk of the subs per
  conn per 200 ms instead of two); a prefetching visit (m.mu released) still looks again.
- L5 (shaper, HS2_QUEUE_CTL_KEEP on): a first install under a cap holds its root graft (mq handle-0 re-add, stale
  root htb -> mq, root-mode htb) up to 60 s for park's light tick; a graft held in mqReady keeps mqchild mode (it no
  longer falls to a root-mode htb on the next tick — also for the fx247 lost-tree case).
- L7: `HS2_HALF_CLOSE_MAX` default 4096 -> 16384 (4x scale; over the cap a relay now ends with a reset).

## rel69i — deploy candidate: rel69h + grouped shaper filters (audit246 M4) (not yet released)

rel69i = rel69h (0ec46bad) + m4f251 (a1ed2745, cherry-picked without textual conflicts; one semantic fix: rel69h's
restart test now passes `qDsts(peer)` to `park`, and a gofmt comment alignment in stream_reverse.go). No new env key
(envdef 97), no new capability bits, no wire change. Each side acts alone.

| change | side | disable / old behaviour |
|---|---|---|
| m4f251 queue-ctl shaper filters for links this side ACCEPTED (Iran edge in reverse mode, exit in direct mode): one u32 key per (peer IP, our wire source port, TCP) instead of one per link (peer IP + ephemeral port) — 600 links from 16 peer IPs = 16 filters; link churn no longer changes the key set (no generation swap). Our wire port is the local port, or the SO_ORIGINAL_DST port when netfilter DNATed the connection to the listener. Dialed links keep the dst + dport keys (same strings as rel69g) | both (accepting side) | no knob |
| restart adoption (rel69h) works unchanged with the new keys: filters are dropped by prio, the class and an identical cake leaf are kept | both | no knob |

lab1 pk238 harness (reverse l3mtcp, Iran edge accepts the links, Iran c3vi mq 8 queues -> mqchild shaper, 45 ms netem,
500 Mbit/s, crowd 200 + 80 kbit call probe, phases u1a u1b d1a d1b u8 d8), 2+2 R = rel69g (0e28de81) vs I = rel69i, order
R I I R: Iran egress u32 filters while capped 37-64 (= links) with rel69g, **1** (peer IP + proto 6 + sport 2096) with
rel69i in every sample; kharej (dialer) 1 as before. Heavy per phase I vs R -3.6 .. +1.1 % (noise), crowd p50 91-92 ms
both, call max gap R 23-60 / I 24-66 ms, cake drops Iran 64/84 vs 60/73, kharej 22/20 vs 16/10; 1 cap switch, 0 panics.
Restart test (rel69i, SIGKILL then start, twice): htb 1fe1:, class 1fe1:2 and cake d649 kept, the class re-capped in
place (r252.md).

## rel69h — deploy candidate: rel69g + storm upgrade pacing (edge) (not yet released)

rel69h = rel69g (607c4081) + st244 (b35e58f3, fd599130, 98b534ec) + st246 (756660da) cherry-picked without conflicts
+ rel69h commits. One new env key in the envdef table (96 -> 97, `HS2_ST244_UPG_RATE`), no new capability bits, no
wire change. Edge only (Iran decides upgrades); the exit is unchanged.

| change | side | disable / old behaviour |
|---|---|---|
| st244 `HS2_ST244_UPG_RATE=auto` default (envdef): lite raw->dual upgrades (a conn's second sub) take a token from one global bucket, `auto` = max(500, 100 x GOMAXPROCS) a second (6 cores 600/s, 20 cores 2000/s), burst rate/10; an empty bucket leaves the conn raw until its next poll. Call upgrades and stripe opens are not paced | edge | `HS2_ST244_UPG_RATE=0`, or `HS2_DEFAULTS=legacy`; a number sets the rate |
| st246 `HS2_ST246_INTERACTIVE_BPS=1000` (in-code, acts only while the pacing is on): a lite conn whose last 1 s sample moved >= 1000 B/s in its quieter direction (low-rate voice, games) skips the bucket | edge | `HS2_ST246_INTERACTIVE_BPS=0` |
| st244 `HS2_ST244_GOGC=N` (code only): sets the GC target | both | default off (no gain, +RSS: st246) |
| queue-ctl shaper restart (r248 follow-up): a start adopts the first stale hs2 class (its process gone, e.g. a stop by signal that skipped remove): that class's filters are dropped, the class is changed in place, and its cake leaf is kept when its parameters are ours (besteffort + flow mode; its drop counter is the new base) — a restart re-creates nothing; any other stale class is dropped as before | both | no knob |
| tests: TestLMFastPickEquivalence walks every random choice of the old picker (tie-breaks, steering draws; test-only hook `pickRandHook`, nil in production) instead of 300 samples (r248 -race flake) | — | — |

lab3 isolated 6-core storm harness (st245/st246; crowd 3000, storm 6000 x2): UPG_RATE only (n=4) vs rel69e (n=3): crowd
p99 m1/m2 313/317 vs 451/447 ms, storm open p99 ~980 vs 1937/1503 ms, stall seconds 3.0 vs 6.3, edge storm CPU peak
4.7-5.0 vs 5.2-5.7 cores, RSS equal. rel69g vs rel69h on the same harness, 3+3 (E H H E H E, auto = 600/s at 6
edge cores): storm-2 minute crowd p99 457 -> 377 ms, storm-bin s2 1025 -> 697, p999 m2 1039 -> 706, open p99 s2
2027 -> 1518; storm-1 minute equal (m1 379 vs 386); call99 m2 748 -> 594; edge storm CPU peak 5.7 -> 5.3 cores;
RSS equal or lower; 0 panics, 0 fails (r250.md).

## rel69g — deploy candidate: rel69e-pre + dl243 rescue (exit) + audit246 fixes (not yet released)

rel69g = rel69f-pre (5c5c4329) + fx247 (1246d8be, merged without conflicts). Sum of rel69e-pre -> rel69g: one new env
key in the envdef table (95 -> 96, `HS2_DL243_RESCUE`), no new capability bits, no wire change. Each side acts alone.

| change | side | disable / old behaviour |
|---|---|---|
| dl243 `HS2_DL243_RESCUE=1` default (see rel69f-pre below): a young stripe sub's first hole is re-sent on an older sub when the young sub's TCP link stalls | exit | `HS2_DL243_RESCUE=0`, or `HS2_DEFAULTS=legacy` |
| dl240 `HS2_DL240_YOUNG`, dl241 `HS2_DL241_SLOWSLOT` (code only) | exit / edge | default off |
| fx247 M1 queue-ctl shaper ownership / self-heal: a root-mode start never replaces a root `mq` that holds a live hs2 mqchild tree (it joins it as an mq child); an installed shaper checks its tree every 10 s and after any failed tc call and reinstalls a lost tree on the same tick (a reinstall needing a ROOT graft waits for a light/parked tick, hl233); rate/filter state is stored only after tc succeeds; tc error logs 1 line/min per kind | both | no knob (shaper itself: `HS2_QUEUE_CTL_SHAPER=root`, or queue-ctl off) |
| fx247 M2 every tc/ip exec has a 5 s timeout (+1 s WaitDelay); the shaper tree lock has a 5 s deadline (tick skipped, retried, nothing committed) | both | no knob |
| fx247 M3 cs228 darkcall prefetch: sub socket/meter resolved under m.mu, only TCP_INFO unlocked | both | no knob (cs228 knob as before) |
| fx247 L1 cc-refresh: a socket left on reno after a failed put-back is retried every tick until it takes | both | `HS2_LINK_CC_REFRESH=0` (whole feature) |
| fx247 L2 smux Close: abandon frames share one 30 s deadline (3 s when the writer is known stuck), stop at the first failure (was up to 3 x 30 s) | both | no knob |
| fx247 L4 an ended mconn leaves the sub sweeper at once (was next gc round, ~10 s) | both | no knob |

Deployment rule (unchanged from rel69e): never run a pre-fx247 root-mode hs2 on the same device as a mqchild one
(its root replace re-grafts our htb as the root: caps keep working but all egress goes through one htb).
Lab1 checks (tests, pk238 harness 1+1 vs rel69e, restart test): r248.md.

## rel69f-pre — deploy candidate: rel69e-pre + dl240..dl243, HS2_DL243_RESCUE default on (exit) (not yet released)

rel69f-pre = rel69e-pre (b4b8315d) + dl240/dl241/dl242/dl243 cherry-picked (no conflicts) + rel69f commits. One new
env key in the envdef table (95 -> 96), no new capability bits, no wire change. Exit only (kharej, the sender of
downloads); the edge is unchanged.

| change | side | disable / old behaviour |
|---|---|---|
| dl243 `HS2_DL243_RESCUE=1` default (envdef): a young stripe sub's first hole (<= 256 KiB, 1 per sub, 2 per conn per 10 s) is re-sent on an older sub when the young sub's own TCP link has acked nothing for max(300 ms, 2 x srtt) while data is unacked (or the link is in RTO loss) | exit | `HS2_DL243_RESCUE=0`, or `HS2_DEFAULTS=legacy` |
| dl240 `HS2_DL240_YOUNG`, dl241 `HS2_DL241_SLOWSLOT` | exit / edge | stay default off (not proven) |

lab2 (bs201 crowd, transient plan soak242b, TT=390), off vs on, 3 pairs (A2 R2, then ABBA A3 R3 R4 A4), pooled:
stalls 22 vs 14; crowd p99 avg 712.5 vs 670.9 ms (max 966.4 vs 909.1); uploads mean 141 vs 216; Iran ticks with
oooB >= 8 MB 1351 vs 1070; young-HOL >= 8 MB stripes 16 vs 13; 0 fails, 0 panics, 0 gap resets in every arm.
Downloads mean 269 vs 238 and seconds < 100 after the first 123 vs 162 are worse, both carried by R4 alone (its
lab ping p99 613 ms, the noisiest arm); R2/R3 alone are 244/272 vs A 269. Rescues: 36 in 3 arms (21 true, 15 fast),
6.7 MB re-sent in 3 x 390 s. Static plan (almost nothing triggers): A1/A1b 304/502 vs R1/R1b 302/390 mean, crowd
p99 253/234 vs 226/253. Notes: dl243.md, dl244.md.

## rel69e-pre — deploy candidate: rel69d-pre + pk239 mqchild queue-ctl shaper, default on (not yet released)

rel69e-pre = rel69d-pre (e519912e = rel69c + hl235 + pk236, the build in production) + pk237/pk238/pk239
cherry-picked (no conflicts) + two rel69e commits. One new env key (envdef table 94 -> 95), no new capability bits,
no wire change. Each side acts alone.

| change | side | disable / old behaviour |
|---|---|---|
| pk239 `HS2_QUEUE_CTL_SHAPER=mqchild` default (envdef) | both (the exit, 1 TX queue, falls back to root mode by itself, logged) | `HS2_QUEUE_CTL_SHAPER=root` (rel69d), or `HS2_DEFAULTS=legacy` |
| rel69e mqchild takes over a root-mode htb left by a stopped process | both (only on a multi-queue NIC) | `HS2_QUEUE_CTL_SHAPER=root` |
| pk237 `HS2_QUEUE_CTL_SHAPER=ifb` (lab only: breaks TSQ, pk238) | - | not a default |

What it does:
- pk239 (engine/queueshape_linux.go): at the first start, while parked, under the tree lock: kernel >= 5.18, root
  qdisc mq, >= 2 TX queues, clsact once, a probe skbedit filter (act_skbedit), then htb 1fe1: (default class 40 Gbit +
  fq_codel, same tree as root mode) grafted once on the LAST mq child. Parked: no filters (native per-queue path).
  Under a cap: clsact egress u32 per link -> `skbedit queue_mapping N-1 priority 1fe1:M` (no mirred: TSQ kept). Stop
  keeps the child tree and clsact; a restart reuses them and grafts nothing. Any failure -> log + root mode (rel69c).
- pk239 07db71d3: the kernel's default mq has handle 0 and cannot parent: mqReady re-adds the same mq with handle
  `1fe0:` once (one root graft at the first-ever start, parked; owner-accepted 2026-11-16).
- rel69e fe9276c3: lab1 showed that a stop by SIGTERM exits before the queue loop's remove, so a root-mode `htb 1fe1:`
  root outlives its process. On Iran (rel69d root mode) the first rel69e start would have logged "root qdisc is not
  mq" and stayed in root mode for good. mqReady now replaces such an htb root by `mq 1fe0:` (children =
  net.core.default_qdisc) once, parked, if every hs2 class on it belongs to a gone process (slot lock free) and sysfs
  does not say 1 TX queue; while a live root-mode process holds a class there it shares that tree (root mode), as
  before. Operator-set mq children (e.g. fq_codel per queue) are not restored: the kernel default comes back.
- qShaperMode is now read by loadEnvKeys (envdef), not at package init.
- Prod notes: Iran kernel 6.18, 20 TX queues: rel69d's root htb -> mq 1fe0: + htb on 1fe0:14, both parked, in one
  start (do it in the light hour, not HH:01). ~1/20 of Iran's other traffic (XPS onto queue 19) passes the 40 Gbit
  default class + fq_codel. Needs act_skbedit (else root mode, logged). Kharej (1 queue) stays root mode.

Checks (lab1, /opt/hs2lab/r241; git archive of fe9276c3, CGO_ENABLED=0 -trimpath -s -w, bin ee2403e3 [build rel69epre]):
vet ok, gofmt ok; `go test ./... -timeout 60m` all ok (engine 906 s); full `go test -race ./engine -timeout 80m` ok
(1314 s, 0 DATA RACE); new TestQShaperMqChildTakesOverRootLeftover passes.
Restart check (pk238 netns, c3vi 8 TX queues): rel69d (root mode) start -> stop (htb root left) -> rel69e start: log
"root-mode htb ... replaced by mq with handle 1fe0:", htb 1fe1: on 1fe0:8 -> stop -> rel69e start: no graft (root and
htb unchanged, htb Sent 53 KB -> 108 KB -> 147 KB keeps counting), 0 panics.
Real harness (pk238, R = rel69d-pre bin 5490774d, E = rel69e-pre defaults, R E E R): numbers in the table below; E: Iran
"shaper on c3vi as mq child 1:8", kharej falls back to root (fq_codel 1-queue root), root mq 1: untouched.

Cells: crowd p50/p99 ms, call p99 / max gap ms, heavy Mbit/s (lab1 16:26-16:45 UTC).

| phase | rR1 | rE1 | rE2 | rR2 | E vs R heavy |
|---|---|---|---|---|---|
| u1a | 93/136, 135/42, 341 | 94/165, 166/37, 430 | 92/165, 166/41, 434 | 94/164, 165/45, 425 | +12.8 % (rR1 341 outlier; vs rR2 +1.6 %) |
| u1b | 95/164, 165/38, 429 | 95/115, 114/40, 428 | 93/116, 115/24, 416 | 94/108, 108/34, 415 | 0.0 % |
| d1a | 94/201, 200/62, 420 | 93/201, 201/48, 421 | 92/200, 201/57, 420 | 95/200, 201/59, 425 | -0.5 % |
| d1b | 94/109, 108/34, 418 | 99/113, 114/36, 413 | 96/124, 125/25, 423 | 94/120, 120/35, 419 | -0.1 % |
| u8 | 98/126, 121/43, 458 | 95/125, 124/41, 459 | 92/128, 113/44, 456 | 96/123, 117/38, 456 | +0.1 % |
| d8 | 95/110, 108/38, 445 | 97/119, 119/40, 440 | 95/118, 123/77, 435 | 98/114, 112/42, 449 | -2.1 % |

Call max gap: R 34-62, E 24-77 (77 = rE2 d8, single). Cake drops Iran R 56/145, E 47/84; kharej 8-12 all. TCP
retransmits Iran R 439/625, E 447/380 (of ~3.6-3.9 M segs). tc_final: E root `mq 1:` untouched, `htb 1fe1: parent
1:8`; R root htb. 0 panics.

## rel69c — deploy candidate: rel69b + hl234 queue-ctl shaper fixes (not yet released)

rel69c = rel69b (1771b450) + b118 branch hl234 (d3e5a0c4, a92bedae, bc345f8c) cherry-picked; no conflicts.
No new env keys (envdef table stays at 94), no new capability bits, no wire change. Each side acts alone.

| change | side | disable / old behaviour |
|---|---|---|
| hl234 shaper filters make-before-break in one `tc -force -batch` | both | no own switch; the shaper exists only while a queue-ctl cap does: `HS2_QUEUE_CTL_KEEP=0` (rel68e controller) / `HS2_QUEUE_CTL_UP=0` (edge upload) |
| hl234 own-shaper backlog is not a path queue | both | `HS2_QUEUE_CTL_KEEP=0` |
| non-Linux build: qWire defined off-linux (build fix only) | - | - |

What it does:
- a92bedae (engine/queueshape_linux.go): a peer-set change used to `filter del` the class's prios and re-add one tc exec
  per link (~4.6 ms/exec, ~185 ms for 40 links): all links went through the default UNSHAPED class in that window, on
  every tick the link set changed (at a burst's onset: every tick). The new set now goes in under the other generation's
  prios (qFilterPriosGen: 0x2000 x gen + minor) in ONE `tc -force -batch -`, then the old generation's prios are deleted;
  qDropClass removes both generations. Lab hl234 u8: unshaped bytes 46 MB -> 1-3 MB per phase, u8 -3.3 % -> +0.5 %.
- d3e5a0c4 + bc345f8c (engine/queuectl.go, queuerun.go, queueshape_linux.go): the lane reads its leaf's backlog with the
  drops (same `tc -s`), own = backlog / (cap x qWire); a binding queue tick that is mostly our own shaper's backlog
  (own >= delay/2 AND delay - own < qHigh) keeps the cap: no cut, no hold, no stuck count. queue-dbg prints own=.
  Rarely engages (cake holds ~2-3 ms); tightened in bc345f8c after it engaged on 15 ms exit ticks.
- Known open (owner-approved rel69 tradeoff): u1b start ramp under a kept cap (lab hl234 -5.7 %), single-upload rate
  start 11-12 s.

Checks (lab2, units rel69ct/rel69cv, /opt/hs2lab/rel69c/tests.log; built from a git clone, bin 5cdc2be8
[build rel69c e009f0012990]): vet ok, gofmt ok; `go test ./... -timeout 60m` all ok (engine 902 s); targeted
`-race ./engine -run 'QueueCtl|QShaper|Shape|Stripe|Fx212|SubResend|Cs22|QBacklog'` 241 pass, 0 DATA RACE (the 3
kernel shaper tests incl. TestQShaperFilterSwapOnKernel pass under -race in their own netns). Full -race not run.
GOOS=darwin build of engine: qWire error gone; tun.Device/tun.Open undefined remains (pre-existing, also rel69b).
Scale (lab2 sc234 harness, crowd 3000 / drop 6000 / storm 6000 at 90+140 s; A = rel67b 9ae05cc8, B = rel69b, C =
rel69c), crowd p99 storm m1/m2: SA 179/187, SC1 219/256, SC2 247/240, SA2 176/175; same-hour pair SB3 (rel69b) 277/197
vs SC3 233/214 (rel69c = rel69b within noise; this hour's lab is slower for every arm, SA 179 vs 154 at 09:34).
Storm open p50/p99 @0: SA 1595/3079, SA2 1493/2753 vs SC1 377/600, SC2 466/850 (SB3 592/2246, SC3 616/2524).
CPU steady edge/exit: SA 509/453, SA2 496/444 vs SC1 555/463, SC2 556/470, SC3 548/463 (SB3 559/472).
Shaper: parked on all 11 lanes, qcaps 0 under this load; filters sampled every 2 s never fell to 0 (min 7-8, max 30
during a batch), default (unshaped) class 5.1 / 4.4 MB vs 3391 / 3575 MB through the shaped class (0.15 / 0.12 %);
no `tc -batch` errors; 0 panics, 0 fails in SC1-SC3.

## rel69b — release candidate: rel69a + sw235 sweeper CPU fix + hl233 queue-ctl fixes (not yet released)

rel69b = rel69a (84f22c3f) merged with b118 branches sw235 (13a08812) and hl233 (1f5573f6); no merge conflicts.
No new env keys (envdef table stays at 94), no new capability bits, no wire change. Each side acts alone.

| change | side | disable / old behaviour |
|---|---|---|
| sw235 sub-resend sweeper (per-link TCP_INFO cache, 10 buckets, quiet-resident hint) | both | no own switch; `HS2_FX212_SUB_RESEND=0` turns the whole sub-resend sweeper off (rel68c path) |
| hl233 onset after a burst probe goes back to the kept cap | both | `HS2_QUEUE_CTL_KEEP=0` (rel68e controller) |
| hl233 shaper installed parked at start, a cap that ends parks it | both | `HS2_QUEUE_CTL_KEEP=0` |

What it does:
- sw235 (engine/mconn_fx212.go, mconn_sub_resend.go, mconn_stripe_resend.go, mconn_stripe_keepstall.go, mconn.go): the
  fx212 SUB_RESEND sweeper reads TCP_INFO once per link socket per bucket tick (a cache shared by every sub on that link;
  the socket is resolved under m.mu, which also closes the old sweeper's unlocked s.rw read); a conn whose last look kept
  nothing is skipped without m.mu until a keep or gap sets its atomic hint again (GC round every 10 s drops quiet/ended
  conns; no sync.Map churn); the set is split into 10 buckets, one per 100 ms, so each conn is still looked at once per
  second; keepSentLocked on a swept conn only sets the hint. Keep and resend rules are unchanged.
  Numbers (lab2 sc234 harness, 9k conns, 3+3 vs rel69a): sweeper 3.34/2.91 -> 1.94/1.62 s per 30 s edge/exit
  (TCP_INFO 1.7 -> 0.4 s), sweeper + keepSent -0.06 core edge / -0.025 exit; whole-process CPU and crowd p99 within
  noise. Bench 10k conns x 2 subs: 39.2 -> 13.1 ms per round, 10001 -> 150 allocs.
- hl233 (engine/qseen.go onsetTarget, queuerun.go, queueshape_linux.go): (1) an onset tick under qFastDelay within 2
  ticks of a burst probe goes back to the cap the probe stepped from (<= qKeepSure x seen) instead of 0.975 x seen
  (hl232 d8 cut 470 -> 465 and ~6 s creep); a >= 40 ms onset still takes 0.975 x seen. (2) the shaper is installed
  PARKED (htb class at 40 Gbit/s, no cap) at tunnel start while the lane is light, and a cap that ends parks it instead
  of `qdisc del root`, so the root qdisc is never swapped under load (the swap's noop-qdisc window dropped packets: the
  hl232 fS2 320 ms call gap); a later cap is a class change; leaf drops while parked are not cap loss; shutdown still
  removes it. Log: "shaper on X ... parked with no cap".
  Numbers (lab1 ABAB 3+3 vs rel69a keys off): u1a -2.7 %, u1b -5.7 %, d1a +6.5 %, d1b -0.5 %, u8 -3.3 %, d8 -3.3 %
  (was -4.0 %); call max gap 25-65 ms in all 18 phases (hl232 had one 320 ms); light p50 92-105 ms; rate gRS1 d1hi 366
  (9 s) u1hi 378 (6 s). Open: u1b (one run 397), u8 big-onset follow-down + hold (pre-existing rel69a behaviour).

Checks (lab2, unit rel69bt, /opt/hs2lab/rel69b/tests.log): vet ok; gofmt ok after f036277e (sw235 field alignment);
`go test ./... -timeout 60m` all ok except tlscarrier TestRealityForeignSpliceLimits (load flake while -race ran in
parallel: "stranger got 65537 B in 2 s"; tlscarrier unchanged since rel52; passes 3/3 alone and whole package re-run);
engine 898 s; `go test -race ./engine` ok 1302 s, 0 DATA RACE (first -race run for hl233).
Scale (lab2 sc234 harness, unit rel69bv, crowd 3000 / drop 6000 / storm 6000 at 90+140 s; A = rel67b 9ae05cc8, B =
rel69b 1a7c42f3): crowd p99 storm m1/m2 A 154/159, 144/152 vs B 161/159, 162/170 (+13/+9 ms); storm-bin p999 A 211/299,
204/204 vs B 221/231, 240/253; storm open p50/p99 @0 A 572/722, 433/586 vs B 225/441, 218/362 (2x faster); CPU
steady edge/exit A 427/376, 444/396 vs B 492/411, 500/422 (+61/+31 %-of-core); 0 panics, 0 fails.

## rel69a — release candidate: rel68e + queue control hl204..hl232 + HS2_LINK_CC_REFRESH (not yet released)

rel69a = rel68e (7b054640) merged with b114 branch hl204 (fb2bfe49: us200, cc203, hl204..hl232; no merge conflicts),
plus the rel69a defaults commit. envdef table 90 -> 94 keys. No new capability bits, no wire change. Each side acts
alone (the controllers shape only this server's own egress; tc shapers exist only while a cap does).

Defaults (through the envdef table, reverse l3mtcp; `HS2_QUEUE_CTL_FAST` / `_KEEP` are now read in loadEnvKeys):

| key | rel69a | side | disable / old behaviour |
|---|---|---|---|
| `HS2_QUEUE_CTL_UP` (edge upload controller) | `1` (was opt-in) | Iran | `=0` |
| `HS2_QUEUE_CTL_FAST` (cap a big queue after qFastTicks) | `1` | both | `=0` |
| `HS2_QUEUE_CTL_KEEP` (kept caps, seen, onset, probe, stripe gate) | `1` | both | `=0` (rel68e controller) |
| `HS2_LINK_CC_REFRESH` (us200/cc203 BBR re-init) | `1` | both | `=0` |
| `HS2_QUEUE_CTL_SEEN`, `HS2_QUEUE_CTL_STRIPE`, `HS2_QUEUE_CTL_STRIPE_SPIKE` (sub-switches of _KEEP) | on in code | both / Iran | `=0` each |

What it does:
- us200/cc203 `HS2_LINK_CC_REFRESH`: a light link whose BBR left STARTUP with a collapsed bandwidth estimate is re-inited
  (reno -> bbr toggle), on the edge's and the exit's links.
- hl204..hl219 `HS2_QUEUE_CTL_KEEP`: a cap whose demand went idle is kept at the path's fresh rate (not removed), never
  above the last cap that fit; htb burst/cburst of 4 ms of the cap; a queue under a non-binding cap follows the path down
  only on 2 queue ticks in a row or a queue >= qFastDelay.
- hl220/hl222: Iran's upload stripe gate takes the edge controller's target (qLow 8 ms) while an edge cap is on; one
  stripe halving per queue spike.
- hl223..hl232 (qseen.go): the path's delivery ceiling ("seen") is the most the links delivered on a queued binding tick;
  the kept cap is min(cap, 0.975 x seen); a growth probe past 1.02 x seen; a new burst's first binding queue tick is the
  onset (cap to 0.975 x seen, no hold); after growth with no queue the kept cap is at most 0.985 x what the links
  delivered on loaded clear ticks; the edge stripe gate under a probe step cuts once, then leaves a queue under
  qFastDelay to the controller.

Lab (lab1 hl232, ABAB 3+3 B first, S = hl232 code with the four keys on, B = hl205 binary with the four keys off; production-like lab):
| phase | heavy S vs B | crowd p50 S / B (ms) |
|---|---|---|
| u1a | -0.9 % (427 / 431 Mbit/s) | 93-94 / 156-158 |
| u1b | +10.3 % (438 / 397) | 94-96 / 92-159 |
| d1a | +6.0 % (429 / 405) | 94-95 / 133-238 |
| d1b | -0.5 % (429 / 431) | 94-96 / 148-189 |
| u8 | +0.1 % (458 / 458) | 93-98 / 285-312 |
| d8 | -4.0 % (442 / 460) | 92-97 / 257-377 |
Light p50 92-98 ms in every S phase. Open items: d8 -4.0 % (exit controller, borderline), one d1a call max gap 320 ms in
fS2 (other 17 S phases 28-56 ms).
Rate test (path 500 -> 250 -> 500 Mbit/s while caps are kept; target hi mean >= 340, 3 s in a row past 260 within 10 s):
fRS1 d1hi 354 Mbit/s start 11 s, u1hi 346 start 12 s, d1lo 197, u1lo 214; fRS2 d1hi 367 start 9 s, u1hi 375 start 9 s,
d1lo 203, u1lo 213 (hl231 eRS2: d1hi 360/12 s, u1hi 359/7 s). Means pass; the regrow start is 9-12 s (at/over the 10 s
target in fRS1). Crowd in fRS1: p50 92 ms, p99 179 ms, 0 failed.

Rollback to rel68e behaviour without a binary change: see README "before rel69a".

## rel68e — release candidate: rel68d + dlp217 + cs221..cs229 (not yet released)

rel68e = rel68d (a0e5691b) + dlp217 + cs221, cs223, cs224, cs225, cs226, cs228, cs229 (branch chain, no merges), plus
the rel68e defaults commit. envdef table 80 -> 90 keys. No new capability bits, no wire change. Each side acts alone.

Defaults (the cs rows are read in code with an in-code default, every carrier, so their table rows document it;
`HS2_FX212_DL_PRIM` is read through the table: `2` on the edge for reverse l3mtcp, mode 1 elsewhere):

| key | rel68e | side | disable / old behaviour |
|---|---|---|---|
| `HS2_FX212_DL_PRIM` (dlp217 rank mode) | `2` | Iran | `=1` rel68d refuse rule, `=0` rel68c |
| `HS2_CS221_DARKCALL` | `1` | both | `=0` |
| `HS2_CS223_CALLRESEND` | `1` | both | `=0` (cs221) |
| `HS2_CS223_DARKRESEND` | `1` | both | `=0` |
| `HS2_CS224_COPYIDLE` | `1` | both | `=0` |
| `HS2_CS224_STALESWITCH` | `1` | both | `=0` |
| `HS2_CS225_CALLPATH` | `1` (inactive while MINPLACE is on) | both | `=0` |
| `HS2_CS226_LITEPATH` (master of the lite rules) | `1` | both | `=0` (cs225) |
| `HS2_CS228_LITEVISIT` | `1` | both | `=0` |
| `HS2_CS228_LITEPLACE` | `0` (was `1` on cs228) | both | `=1` with `HS2_CS229_MINPLACE=0` |
| `HS2_CS229_MINPLACE` | `1` | both | `=0` (then the LITEPLACE / CALLPATH rules) |
| `HS2_FX212_DL_SHAREQ_MS` (dlp217, exit) | `0` (off) | kharej | - |

Fixes and keys:
- dlp217 `HS2_FX212_DL_PRIM=2` (Iran): under DL_COPIES a download grades the copies' links instead of refusing every
  link with a primary (mode 1 left 0 eligible links in a 3000-conn crowd, losing the DL_COPIES gain): tier 0 no open
  primary, tier 1 primaries idle for `HS2_FX212_DL_PRIM_QUIET_MS` (1000), tier 2 hot, at most `HS2_FX212_DL_PRIM_HOTK`
  (1) sub there. Counters on the stripe stat line, pickwhy `dlq/dlhot/dlhotref`. `HS2_FX212_DL_SHAREQ_MS` (exit,
  a stripe sub on a shared link takes chunks only under a queue bound) stays off.
- cs221 `HS2_CS221_DARKCALL`: the darkcall switch resends only `[from, min(qStart, to.missEnd))` (what the target lacks);
  a call pair never puts both copies of a chunk on two subs of one path while another path has room (10.1 s / 6.2 s
  call stalls); the rx rescue waits while a non-dark live sub read a record within darkcallAfter.
- cs223 `HS2_CS223_CALLRESEND`: a given-up call sub's chunks go to the front of a live sub at give-up (not ~8 s later at
  its death); resends skip chunks the target delivered with proof; a FIN is never left out of a resend.
  `HS2_CS223_DARKRESEND`: a call sub that goes dark while not the primary resends at once what the others lack
  (21 s call stall in lab2 D1).
- cs224 `HS2_CS224_COPYIDLE`: a moving copy-only sub is not idle for the drain reclaim. `HS2_CS224_STALESWITCH`: no ring
  resend off a sub an earlier switch already left (266 KB resend of long-read bytes, 1.4-2.6 s stalls).
- cs225 `HS2_CS225_CALLPATH`: a call's copy/pair sub goes to another healthy path first (both subs dark together).
- cs226/cs228 `HS2_CS226_LITEPATH` (master), `HS2_CS228_LITEVISIT`: darkcall moves a non-call lite connection off a
  silent (pending write unanswered, nothing read for darkcallAfter) or LB lowered/cut link while it has unconsumed
  bytes; its TCP_INFO reads run without m.mu. `HS2_CS228_LITEPLACE` (RTT/loss-gated lite placement) is off: it cost
  light p99 in cs228's lab2 A/B.
- cs229 `HS2_CS229_MINPLACE`: a lite second sub / a call's extra sub keeps the original pick and is re-picked on another
  healthy path only when it shares a path with the connection's live subs (no download link; no candidate: original
  kept). Counters `cs229 lite kept/repicked/nocand call kept/repicked/nocand` on `mconn darkcall:`.
- test-only race fixes on the way: cs228 darkcallAfter/Gap and rrRingCap read through atomics.

Rollback to rel68d behaviour without a binary change: see README "before rel68e".

## rel68d — release candidate: rel68c + fx212 + hc213 (not yet released)

rel68d = rel68c (cdc74b71) merged with fx212 (f89098ae, 522a7016, 19cab67a) and hc213 (9a0128f5); no merge conflicts
(the two branches touch disjoint files). envdef table 76 -> 80 keys (the four `HS2_FX212_*` kill switches, default on;
they are read in code with an in-code default of on, so the rows document it). No new capability bits; the only wire
addition is hc213's empty smux PSH "abandoned" mark, which any smux peer ignores and which goes only to half-open peers.

rel68c (cdc74b71, already built): `HS2_MCONN_SUB_RESEND` default on (both, envdef; r68fix2..r68fix4) — a small light
connection keeps its subs' unacked chunks and resends them on a fresh live sub when one dies.

fx212 (an210 + audit211 H2/M2; `=0` gives back rel68c for each key):
- audit211 H2 (no key, leak fix): kept chunks of abandoned (`rrAband`) subs are released (`abandonSentLocked` at every
  abandon site and in `subGone`; connection end also walks `rrOld`/`rscAband`; no keeps on an abandoned sub). The
  global keep gauges no longer only grow, so SUB_RESEND no longer turns itself off after ~32 MiB of rescued bytes.
- `HS2_FX212_DARKCALL` (both): the DARKCALL in-flight cap needs a stalled link (bytes in flight, no ack progress for
  half the dark time); a call with 2+ live subs never has all of them capped (`uncapall` counter); a switch clears the
  target's cap; cap = min(DARKCALL_CAPKB, 3/4 of the resend ring). Fixes 1.4-3 s call stalls on healthy links.
- `HS2_FX212_DL_PRIM` (edge): under DL_COPIES a download takes a link carrying lite copy-only subs only when every open
  user stream on it is a copy (`dlprim=` in pickwhy). Targets the +5 ms light p99 seen with DL_COPIES.
- `HS2_FX212_SUB_RESEND` (both): SUB/CALL_RESEND light-path cost — a given-up sub resends only with undelivered bytes,
  one shared 1 s keep sweeper (TCP_INFO read without m.mu), keptMax noted per tick instead of a CAS per write, the
  resend skips chunks the target sub already holds (`dup-skipped`).
- `HS2_FX212_AGE_GAP` (both; audit211 M2): an age trim on the write path without TCP proof opens the sub's gap, so a
  death in that window takes the fast reset instead of a resend with a hole (a 30 s stall).

hc213 (audit211 H1/M1/L1/L3; no switch):
- H1: smux `Close()` on a half-open stream without a prior CloseWrite is an abandon: empty PSH mark + FIN + FIN; the
  receiver's `PeerAbandoned()` fires before EOF and the relay turns that EOF into an RST (user abort mid-download: exit
  releases the target in ~0.1 s instead of up to 3 min; aborted upload reaches the target as RST). `halfCloseAborts`.
- M1: no raw half-close while the raw->records upgrade is in progress (`rawHCUpgradeSkips`); a late UPGRADE after the
  exit's own raw half-close closes only the joining sub (`rawHCUpgradeLate`), not the connection.
- L1: raw half-close FIN has a 3 s write deadline (`CloseWriteTimeout`); on failure full close + abandoned mark.
- L3: status JSON `half_closes, half_close_idles, half_close_over, half_close_gone, half_close_aborts, half_open`;
  `/debug/hs2halfclose` adds the three new counters.

Tests (lab1, rel68d tree): go vet ./... clean; gofmt clean on our packages (only the vendored third_party/utls pair, unchanged since rel68c); full `go test ./...` 12 packages ok (engine 905 s); smux suite ok x3 and `-race` ok (one TestStallUnreadStreamEvicted live-count flake in the first run under parallel load: 30/30 alone on rel68d and on rel68c, 3 full runs ok); engine `-race` full run: one test-only race (TestBigRingTiersKeepRoomForCalls wrote rrBigMax outside rescueKnobMu while the test rescue sweeper logged it; fixed with `setRRBigMax`), every other test ok (1421 s); after the fix `-race -count=3` BigRing|CD152|Rescue|Darkcall|Fx212|HalfClose ok (294 s).

Not yet: no lab A/B of the fx212 gains; no tunnel-level soak of hc213.

## rel68b — release candidate: rel68 + r68fix + cs193/cs194 + ul197, five defaults on (both sides; not yet released)

rel68b = rel68 (1c0d491d) merged with r68fix (42ce25c6), cs193 (66bc70be; cs193 + cs194 1a06dcbb) and ul197
(ba2399b0); all three merged without conflicts. envdef table 71 -> 75 keys. Not included: SUB_RESEND (r68fix2),
cd195, pipe19x, cpu190 direct, mt153 clamp. No new capability bits, no wire change (ext bits 0-4, 8-13 as in rel68).

New vs production rel67b (`=0` turns each off):
- rel68b: `HS2_MCONN_CALL_RESEND` (both, in-code, r68fix): a call cut 10-30 s after the stuck-call give-up on a dark
  path (old bug, rel67b too) — a lite call's chunks are kept until acked and a dead sub's are resent on a live sub.
- rel68b: `HS2_MCONN_DARKCALL` (both, envdef; `HS2_MCONN_DARKCALL_MS` in-code 1000) (cs193/cs194): timer-driven
  dark-path detection for call connections, primary switch to a live sub on another socket/path + ring resend,
  Iran records rescue when no candidate; in-flight cap. `HS2_MCONN_DARKCALL_TRACE` off.
- rel68b: `HS2_LB_STRIPE_STEER` (both, envdef; st149/st150): stripes keep off cut paths. In production by drop-in
  since 2026-11-14 (acted correctly in a real cut); the drop-in may stay or go.
- rel68b: `HS2_MCONN_STRIPE_DL_COPIES` (edge, envdef; dl186/dl188) with `HS2_MCONN_STRIPE_DL_COPIES_MAX=4` (in-code).
- rel68b: `HS2_MCONN_STRIPE_UP_RAW_COPIES` (edge, envdef; ul197): an upload's raw upgrade takes the share-copies hint
  (lab2: 11 of 16 heavy uploads never got a stripe sub without it).
- rel68: `HS2_LM_FASTPICK` (edge), `HS2_LB_SPARE_EDGE` (exit), `HS2_AP_SHRINK_HYST` (both) — envdef;
  `HS2_LB_REC_RAMP`, `HS2_LB_LOWER_EVID` (both), `HS2_LB_CALL_STUCK_GAP_STRICT` (edge), `HS2_HALF_CLOSE` (both,
  ext bit13, `_MAX=4096`) — in-code; ou180 behaviour changes without a key (see rel68 below).

Still off: `HS2_MCONN_STRIPE_UPGATE_HYST` (ul197), `HS2_MCONN_RESCUE_RST` (canary later), `HS2_SMUX_SENDBATCH`,
`HS2_MCONN_STRIPE_DL_SHARE`, `HS2_MCONN_STRIPE_HEADROOM`, `HS2_LB_UPLOSS`, `HS2_QUEUE_CTL_UP`.

Rollback: `HS2_MCONN_DARKCALL=0 HS2_MCONN_CALL_RESEND=0 HS2_LB_STRIPE_STEER=0` (both),
`HS2_MCONN_STRIPE_DL_COPIES=0 HS2_MCONN_STRIPE_UP_RAW_COPIES=0` (Iran).

Tests (lab2, rel68b tree): gofmt clean (vendored utls aside); go vet ./... ok; go test ./... -timeout 40m ok
(engine 892 s, incl. the updated TestApplyDefaults and envdef 75-key count); smux go test ./... -count=1 ok.
go test -race ./engine: two failures, neither in shipped code — TestLBOutageScoutFindsWorkingPath (pool 7 of 8 in
15 s under 3 parallel race jobs; 5/5 pass alone: load flake) and a race in r68fix's new TestMConnCallResendDarkPath
(the test reassigned the global callStuckBusy hook while the manage goroutine read it; fixed with an atomic flag,
5/5 race-clean alone). Targeted -race (CallResend|DarkCall|Darkcall|Stripe|DlCopies|UpRaw|Steer|HalfClose|Pick|
LMFast, -count=2) after that fix: one test-only race left — cs193's setDarkcall writes rrRingCap while the
process-wide rescue sweeper (started by an earlier rescue test in the same process) reads it for its log line;
production never writes rrRingCap after init. Darkcall tests alone -race -count=3 ok. Open: make that helper
race-safe. Binary (git archive, CGO_ENABLED=0 -trimpath -s -w; non-test source identical to this commit): sha256
c5c784e71d99e55f4d3ec90efb7df16644f1a2f9b4b73581b651b5154115e4e5 (<work>\bin68b).

## rel68 — release candidate: rel67b + fp185 + sp176 + wj178 + ou180 + hc183 + rr166 + dl188 (both sides; not yet released)

rel68 = rel67b (94056b33) merged with, in order: fp185 (91e965e8; lk162 + lk165 + lk179 SENDBATCH), sp176 (bc8e6861),
wj178 (8057925b), ou180 (9505beb4), hc183 (41a4a837), rr166 (659228ad; rescue-rst rr155..rr158 on fn164, not rr159)
and dl188 (917ff3aa; dl184 + dl186 + dl188). envdef table 68 -> 71 keys.

Default ON (new vs rel67b; `=0` turns each off):
- `HS2_LM_FASTPICK` (edge, envdef): new connections and mconn subs placed under the pool's read lock (fp185).
- `HS2_LB_SPARE_EDGE` (exit, envdef): spares asked only where the edge would serve them; backoff (sp176).
- `HS2_AP_SHRINK_HYST` (both, envdef): autopilot shrinks a link's worth under the grow line (sp176).
- `HS2_LB_REC_RAMP`, `HS2_LB_LOWER_EVID` (both, in-code): recovery ramp to 1.0, noise dead-band (wj178).
- `HS2_LB_CALL_STUCK_GAP_STRICT` (edge, in-code): now on by default (wj178).
- `HS2_HALF_CLOSE` (both, in-code, ext bit13) with `HS2_HALF_CLOSE_MAX=4096`: TCP half-close through the
  tunnel; a mixed pair falls back to the old full close (hc183).

Default OFF: `HS2_SMUX_SENDBATCH` (lk179), `HS2_MCONN_RESCUE_RST` (rr166; ext bit11 + bit12, roles 5/6),
`HS2_MCONN_STRIPE_DL_SHARE`, `HS2_MCONN_STRIPE_HEADROOM`, `HS2_MCONN_STRIPE_DL_COPIES` (+`_MAX`, default 4) (dl18x).

Behaviour changes without a key (ou180): the outage scout rotates over every path and a failing primary backs off
while others hold links; the STRIPE_STEER pool ask has its own dedupe; `hs2 lb show` shows the mode in effect,
cuts per Iran row and labelled users; the warm pool size is kept up to 1 h after an outage (half up to 3 h).

Capability ext bits after the merge: 0 stripe, 1 UDP fast (reserved), 2 stripe up, 3 resend, 4 overlap,
8/9 rescue/records, 10 keepstall, 11/12 rescue-rst, 13 half-close; no collision. mConn roles 0-6 unique.

Merge conflicts: envdef_test count (69 vs 70 -> 71), mconn_rescue.go log line (rs169 + rescue-rst counters both
printed), defaults22.go (FASTPICK + dl18x loads both kept), mconn_raw.go (hc183's no-upgrade-once-half-closed
condition kept together with dl188's trLightOp trace).

Tests (lab1, rel68 3ca298b3): gofmt clean (only vendored third_party/utls, as before); go vet ./... ok (smux's
upstream session_test.go:301 vet note unchanged since v1.5.24); go test ./... ok (engine 868 s); go test -race
./engine ok (1269 s); smux go test ./... -count=1 ok; targeted -race (HalfClose|Rescue|RS169|Pick|LMFast|Steer|
Spare|Hyst|Ramp|Deadband|CallStuck|Stripe|DlCopies|Close|SlowTail|Trunc, -count=2) ok (1150 s). No failure, no
flake. Binary (from 3ca298b3, git archive, CGO_ENABLED=0 -trimpath -s -w): sha256
b280ac393867b4744faebba29d3ae3210924e32e4bef3bb2c6fc140359855771 (<work>\bin68).

## dl188 — HS2_MCONN_STRIPE_DL_COPIES fixed (raw upgrade) + fair cap DL_COPIES_MAX; pick-none histogram

Binding constraint (lab2, crowd 3000, max_links 200, 9 background downloads): after a stripe pick finds no
free link, the shared pick (`pickStripeSharedWhyLocked`, mconn_stripe_share.go:166-169) refuses every link
that carries lite's copy-only subs unless the open has the copies hint. With the crowd, EVERY serving link
carries copies (new `stripe pickwhy` histogram, all refusing conditions per link: per failed raw-download
pick ~182 links `copies+light`, ~11 `stripe+copies+light`, ~3 `heavy+...`, 0 links free of copies). dl186's
DL_COPIES never reached the test downloads: the raw upgrade's open (mconn_raw.go) built its avoid list
without `stripeCopyUpHintLocked`, so dc1/dc2 had copy-ok=0 on 1347/1349 and 1255/1256 pick-none lines, while
the background stripes' growth (which had the hint) took the copies' links (173/159 shared picks; each
background download then held ~7 stripe subs for its whole life vs 1-7 without the key) — hoarding with no
gain for new downloads, and light p99 +12 ms.

Fix (Iran, key off = unchanged): with HS2_MCONN_STRIPE_DL_COPIES=1 the raw upgrade's open gets the hint too,
and a download takes the copies' rule only while it has fewer than HS2_MCONN_STRIPE_DL_COPIES_MAX subs
(default 4; 0 = no bound): new downloads' first subs, never a big stripe's growth (fair share). Uploads keep
SHARE_COPIES as before. `stripe pickwhy raw=.. copyhint=..: combo=N ...` (HS2_MCONN_STRIPE_LOG) after each
pick-none.

Lab2 A/B (same harness as dl186, 480 s arms, sequential whole lab):
| arm | median | mean | p10 | <100 Mbit | tests never striped | ping p50/p99 | bg down/up achieved | pool mean/max | Iran RSS max |
|---|---|---|---|---|---|---|---|---|---|
| k1 dl188 bin, keys off | 196 | 239 | 41 | 21 % | 16/19 | 93.8/136.2 | 1.00/1.00 | 189/207 | 455 MB |
| x1 DL_COPIES=1 | 477 | 440 | 77 | 16 % | 0/19 | 94.1/136.5 | 1.00/1.00 | 188/206 | 590 MB (peak once; 470 steady) |
| x2 DL_COPIES=1 | 364 | 444 | 174 | 5 % | 0/19 | 93.9/139.9 | 0.95/1.00 | 188/210 | 528 MB |
| c1 dl186 bin (15dea90e), keys off | 425 | 333 | 30 | 26 % | 16/19 | 94.4/136.0 | 0.97/1.00 | 190/213 | 454 MB |
| (dl186 b1/b2 keys off) | 261/227 | 231/249 | 26/32 | 39/37 % | 16/- | 93.9-94.2/134.4-134.7 | 0.99 | 190 | 452 MB |

Verdict: the fix removes the "never striped" mode (0/38 test downloads vs 32/38) — mean 440/444 vs 239/333, p10 77/174 vs 41/30, <100 Mbit 16/5 % vs 21/26 %; background heavy users keep their rate (achieved/asked 0.95-1.00 in all arms). Light ping p50 unchanged; p99 136.5/139.9 vs 136.2/136.0 (+0.3 and +3.9 ms, mean +2 ms: a tiny worst-case cost). Iran RSS: steady ~470 MB vs ~450, peaks 528-590 MB during 500-900 Mbit/s tests. Pool size unchanged. Off by default.

## dl186 — HS2_MCONN_STRIPE_HEADROOM (stripe-only links above max_links, off by default)

New key HS2_MCONN_STRIPE_HEADROOM=PCT (both sides; unset/0 = off, as before; bounded at 50): while a stripe
holds, has set aside or still wants links (stripeExtra), the edge's pool target may go up to PCT % of
max_links (at least 1) above max_links, and the reverse exit follows it (exitPool.bounds widened by the same
headroom). Everything else keeps max_links as its ceiling (autopilot target, drain/refill, light users). The
extra links are set aside for the stripe on arrival and are the first retired when the stripe lets go, so
max_links stays the configured value and becomes a soft cap only for temporary stripe-only links.
`stripe headroom: N stripe-only link(s) over max_links M (peak)` logs each new peak.

Lab2 A/B (crowd 3000, max_links 200, 9 background downloads, 12 s test download every 20 s, 480 s per arm,
sequential whole-lab arms): 
| arm (binary, keys) | tests | median | mean | p10 | <100 Mbit | ping p50/p99 | pool mean/max | Iran RSS max |
|---|---|---|---|---|---|---|---|---|
| b1 base (dl184) | 18 | 261 | 231 | 26 | 39 % | 93.9/134.4 | 190/207 | 452 MB |
| b2 base (dl184) | 19 | 227 | 249 | 32 | 37 % | 94.2/134.7 | 190/211 | 448 MB |
| h1 HEADROOM=10 | 19 | 154 | 197 | 43 | 42 % | 94.1/134.0 | 205/227 | 486 MB |
| h2 HEADROOM=10 | 19 | 168 | 256 | 28 | 37 % | 93.9/133.7 | 206/233 | 495 MB |
| hs1 HEADROOM=10 + DL_SHARE (one run; dl184's DL_SHARE alone s1/s2: median 343/175, <100 25/36 %) | 19 | 416 | 365 | 38 | 16 % | 94.0/133.7 | 206/229 | 525 MB |

HEADROOM alone does not help: the 20 extra links come at once (peak 20 over 200 in the first second) and
are taken by the 9 background downloads' stripes, which keep them; a test download's growth still finds
"pick: none" (1552/1646 vs 1827/2096 base). Light ping unchanged. Off by default.

Why no link is free: in the pick-none lines 202-210 of ~220 serving links are skipped ONLY because lite's
copy-only subs ride them (`skip[... copies=209 ...]`): HS2_MCONN_STRIPE_SHARE_COPIES (share62) admits such
links for uploads only. New key HS2_MCONN_STRIPE_DL_COPIES=1 (Iran, off; NOT yet measured when committed):
downloads take the same rule (call and lane links keep theirs). Lab arms dc1/dc2 queued on lab2
(/opt/hs2lab/dl186/r/dc1, dc2; binary 15dea90e).


## dl184 — why some single downloads never stripe (diagnosis + HS2_MCONN_STRIPE_DL_SHARE, off by default)

Lab2 soak (crowd 3000, pool at max_links 200, 9 background users, a 12 s test download every 20 s), new
trace (HS2_MCONN_STRIPE_LOG=1: `stripe nostripe <id>: ... [reason=count ...]` for every heavy connection that
ended without a stripe, mconn_stripe_dltrace.go). rel67b logic, 40 tests: 38 never striped. The growth was
DECIDED 3-5 times in every one of them (`grow=3..5`), and every open failed (`openfail` = `grow`):
`stripe pick: none (free=0 empty=0 ...)` 3839 times — every serving link carries a light user or a stripe,
and the pool, already at max_links, cannot dial the link the stripe asks for. The bimodality is only which
link the raw first sub landed on (lossy: 20-60 Mbit/s, below DL_FAST's 80 Mbit/s, `dlcold`; good: 400-600).
Not the cause: stripe slots (`slot=0`), DL_FAIR lease (`dlsolo` only delays fast ones), steer hold, calls.

HS2_MCONN_STRIPE_DL_SHARE=1 (Iran): when nothing else is found, a download's growth while it has fewer than 4
subs may share a stripe link that carries exactly one other stripe sub and no light user's primary (lite
copy-only subs allowed, never a call's link); the lowest rate10 first. No wire change. One 600 s run each
(window-matched to the 900 s baseline): slow (<100 Mbit/s) 48 % -> 25 % / 36 %, median 136 -> 343 / 175,
p10 30 -> 57 / 33; bg throughput, RSS unchanged; light p99 median 136.8 -> 135.6 / 139.4 ms. It engages only
32-40 times a run (most stripe links carry light primaries): too few runs to call — the key stays off.

## rel67b — release candidate: rel67 + ul173 + mb174 + smart big-ring budget (both sides; not yet released)

rel67b = rel67 (b85 90f81fc2 = rel66b + fx171 + fx170 + rs169) + ul173 (b86 e891ab96) + mb174 (b87 edf2b480)
+ doc175 (b88 8d983b37) + the RAM-sized records big-ring budget below. No envdef change vs rel66b (table 68
keys; only the KEEPSTALL row's comment); the two new switches below are in-code defaults (on, `=0` off, every
mode). `HS2_MCONN_RESCUE_REC_BIGMB` no longer needs setting: unset, the budget is sized from RAM.

- **fx170 — a truncated download reaches the user as a reset** (edge; the mirror of fx168's exit fix).
  `engine/wedge.go` relayStream records how each direction ended: when stream→user ends with an error, or the
  stream dies before its EOF was relayed, the user socket is closed with RST (`SetLinger(0)`), not FIN; when the
  user or target side ends with an error on an mConn, the mConn is aborted so the other end is reset too.
  `third_party/smux`: a link's EOF (FIN / close_notify) is reported to its streams as `io.ErrUnexpectedEOF`
  (before: `io.EOF`, the same as the stream's own FIN, so the one-stream and raw lite paths delivered a cut stream
  as complete); `Stream.ClosedHere` keeps intentional local closes (idle reclaim on retiring links, evac) a clean
  FIN. `engine/stream.go` relay (dgforward / dgports): a side whose read ended with a reset resets the other.
  New status-file counters (`/run/hs2/<config>.status.json`): `mconn_target_rst`, `mconn_user_rst`, `stream_rst`. Test
  `engine/dltrunc_test.go` TestDownloadTruncationResetsUser: fx168 9/9 cut cases ended with a clean EOF
  (262144 of 524288 bytes); fixed 7/9 reset, full transfers intact. Known gap: a *target's* reset on the
  one-stream path and on raw lite still arrives as FIN (smux has no reset frame; needs a capability bit).
- **fx171** (test only): `testHookUserMConnClosed` is an `atomic.Pointer` (a -race report);
  TestMConnCloseRightAfterTail reports where a missing upload went.
- **rs169 — idempotent rescue under RETRY**, `HS2_MCONN_RESCUE_IDEM` (on, both sides, no wire change; audit
  rel66 H1, M1, M3, M5). Exit: a RESCUE repeated by Iran's retry (same Bi, Ri) on a newer join is answered again
  (the resent bytes kept 60 s, later records from the records ring) instead of refused (reset); an overtaken
  older RESCUE is ignored; the er94 fresh (role-4) RESCUE's answer is kept too and turns the conn's records
  ring on. Iran: the answer of a raw join a retry replaced still counts, a second answer is a no-op; the
  replaced join is retired and the retry join made primary (M1); no raw retry when no records ring covers
  [Bi, sendOff) (M3); a retry that cannot go on keeps a live join. Both: a peer that read past this side's raw
  count (cut write sent whole, counted short) needs nothing resent (M5). Log line `mconn rescue169:`. Lab
  (lab/rs169/rt, retry-heavy, same binary IDEM=1 vs 0, 2 rotations × 420 s): exit refused 0 vs 16+10, repeats
  answered 12+9, CORRUPT 0 in 30 byte-checked calls per run, crowd fails 6541 vs 7301, RSS within noise.
- **ul173 — a whole short upload could be lost** (exit). `serveMConnSub` published a new connection before
  attaching its first sub, so Iran's second (empty, closed at once because the user had already closed) sub
  could join first, become the only sub, end the conn ("every sub died", target reset), and the first sub
  carrying the whole upload then met an ended conn. Now the exit reserves the id, attaches the first sub, then
  publishes; joins wait for the publish. TestMConnCloseRightAfterTail, -race under load: 3/160 failed before,
  0/200 after. Exit-only fix.
- **mb174 — KEEPSTALL quiet-connection release + records-ring big budget** (both sides).
  `HS2_MCONN_STRIPE_KEEPSTALL_IDLE` (on; audit M2): a striped conn's kept chunks were freed only by a write on
  the same sub, the ack-stall poll or the conn's end, so quiet keep-alive conns pinned their in-flight chunks,
  filled the 320 MiB global cap, and an active conn then dropped its own dark-path hole chunk (gapCheck reset
  after 30 s). Now a conn that keeps chunks is polled every 1 s (acked chunks freed) and one quiet for 30 s
  (10 s while the process keeps over half the cap) drops its unacked tail; active conns are never touched.
  `keepstall:` log adds `idle_polls`, `idle_drop`, `idle_drop_kb`. Records rings (cd152 budget
  `HS2_MCONN_RESCUE_REC_BIGMB`): a ring whose rate wants at most half its size for 3 s shrinks back (newest bytes
  kept); growth past 32 KB is tiered — calls up to 100 % of the budget, sustained writers 60 %, bursts 40 %.
  `mconn rescue-rec:` log adds `call` KB, `bigno`, `shrunk`. Production (rel66b, ~4k conns) had the 64 MB budget
  at its cap (see the RAM-sized budget below). Tests `mconn_stripe_keepidle_test.go` (TestKeepIdleM2Repro),
  `mconn_rescue_bigring_test.go`; lab: no regression (the new paths were not exercised under load).
- **Records big-ring budget sized from RAM** (both sides; `engine/mconn_rescue_budget.go`). The cd152 budget
  for records-ring growth past 32 KB was a fixed 64 MB; production hit it at ~4.2k conns while a 2 Mbit/s call
  ring costs ~0.75 MB and the exit has 23 GB. With `HS2_MCONN_RESCUE_REC_BIGMB` unset the budget is now
  max(min(256 MB, 10 % of RAM), 2 % of RAM), capped at 1 GB, RAM = MemTotal (`/proc/meminfo`, read once at
  start) or the process's cgroup memory limit (v2 `memory.max`, v1 `memory.limit_in_bytes`, smallest along
  the cgroup path) when smaller: 23 GB → ~470 MB, 4 GB → 256 MB, 1 GB → ~102 MB; no MemTotal → 64 MB. An
  explicit key (MB) wins as before. mb174's tiers (calls 100 %, sustained 60 %, bursts 40 %) are shares of
  this budget. Startup log `mconn rescue-rec: big ring budget N MB (source; tiers), ring …, records budget …`;
  the counters line now reads `big X KB of N MB`. A cap, not an allocation: the exit's RSS grows only by
  what call/stream rings actually use. Tests `mconn_rescue_budget_test.go` (formula at 512 MB..64 GB, cgroup
  v1/v2/unlimited/parent slice, env override, invalid env).
- rel67 binary (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" ./cmd/hs2`, git archive
  of 74c17b17): sha256 `af08c664ccc56f9b720b76e9a060b8ca41bce6b4512a4f88e41e95472abe3e50`; lab harness
  `hs2-src/lab/rel67` (tests67.sh, soak67.sh). The rel67b binary is built after this merge.

## rel66b — rel66 + fx168: slow-upload tail and smux update lock (both sides)

rel66b = rel66 (90fab61d) + fx168 (b0821a28); lab harness `hs2-src/lab/rel66b`. No envdef change.
- **H2 (audit rel66)**: fn164 waited at most 1 s for a closed connection's end, shorter than the 5 s close
  linger, so a slow link's upload tail was still cut. Iran now waits up to linger + RST wait (6.5 s; at most
  8192 waiters) and then aborts; the exit closes the target with **RST** (`SetLinger(0)`) on any abnormal end
  (before: always a clean FIN, so a truncation looked like success). Slow-tail tests: rel66 cut 400 KB / 2 MB
  uploads, fixed. (Counted as `mconn_target_rst` in the status file from rel67b.)
- **M6**: a smux Read waiting on aw163's update lock (`updMu`) could block past its deadline under a one-way
  blackhole with a full shaper; the lock is now a semaphore (`updSem`) that a deadline or close aborts.
- Binary (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" ./cmd/hs2`, git archive of
  b0821a28): sha256 `bf3a642c5b77fddab01efcda817d474b9f877ebed6a34421728eac5cbbc2630f`. Tests (tests66b.sh):
  fmt/vet, full suite, `-race ./engine`, smux full + race `AutoWin|UPD|Stale|Deadline` ×3,
  `CloseRightAfterTail|CloseSlowTail` ×5. Soak r66bsoak (lab2, default env): clean, ping on par, RSS below rel66,
  `obs_smux_stale_upd` 0.

## rel66 — rel65 + aw163 (smux autowin stale window update) + fn164 (truncated uploads) + sm160 (both sides)

rel66 = rel65 + sm160 (b71) + aw163 (b74 f424f6a8) + fn164 (b75 c7adaaf5); release audit `docs/audit-rel66.md`.
No envdef change (table 68 keys).
- **aw163 — a heavy stream could stall forever** (production bug, both sides). The autowin shrink sent its
  window update (UPD) from a new goroutine while Read sent its own; under load an older UPD overtook a newer
  one, the sender's limit went backwards and, with everything read, the receiver never sent another: the
  stream waited for window with healthy links (no watchdog saw it). Fix, either half alone prevents it:
  the receiver numbers every UPD and sends them in order; the sender ignores a UPD older than the last
  (status file `obs_smux_stale_upd`). TestAutoWindowBudgetBounds stress: 3/12 hung → 0/24.
- **fn164 — silent upload truncation** (production bug, Iran). `serveUserMConn` closed every sub right after
  `mc.Close()` while the user's last bytes and FIN were still queued; the exit saw the subs die without a FIN
  and closed the target cleanly with bytes missing. Now Iran waits (≤ 1 s, extended in rel66b) for the
  connection's end. TestMConnCloseRightAfterTail: rel65 23-24/48 truncated → 48/48 intact.
- **sm160**: `hs2 doctor` warns when `net.ipv4.tcp_mtu_probing` is not 1 (see mt153); smux fork `go.mod`
  go 1.21 → 1.24 (its real floor), so the smux tests build in place; dg tests use their own loopback addresses.
- Binary (`-trimpath -ldflags="-s -w"`, git archive of a2a4f31b): sha256
  `63036530d92438b71b43bad49cd43ad099b84b2ea90bac2bd74dda9e1e81a725` (supersedes 20565430, which lacked fn164).
  Lab harness `hs2-src/lab/rel66` (1 h soak, default env, tests66.sh).
- Not merged into rel66 (lab branches only, keys absent from the build): rr155..rr166 `HS2_MCONN_RESCUE_RST` (RST-death rescue, cap bit11/12;
  call deaths 30 → 16 in rr166), lk162/lk165 `HS2_LM_FASTPICK` (pick lock, scale work),
  st167 `HS2_MCONN_RESCUE_AGAIN`.

## rel65 — rel64 + race147 + sg154 + cd152 + sd151 (both sides)

rel65 = rel64 (e63d5864) + race147 (b56) + sg154 (b63) + cd152 (b61) + the stripe-steer line up148/st149/st150/sd151
(b57-b60). No envdef change (table 68 keys). Deployed on tunnel 1 2026-11-14 (kharej 16:24, Iran 16:26 UTC).
- **cd152 — records rescue for calls**: a fixed 32 KB records ring never covered a 2 Mbit/s call (every RREC
  of a call answered short, the call ended). A records ring now holds 3 s of its connection's write rate
  (`HS2_MCONN_RESCUE_REC_SECS`), 32 KB to 1 MB (`HS2_MCONN_RESCUE_REC_MAXKB`), growth above 32 KB from its own
  budget `HS2_MCONN_RESCUE_REC_BIGMB` (64 MB). Lab: call deaths 26 → 14, flap 15 → 7; ~+25 MB ring memory.
- **sg154**: a torn interface read in the per-sub trace (`HS2_MCONN_TRACE` only) crashed the exit in the lab;
  read under `m.mu` now. Production never ran the trace.
- **race147**: deterministic stripe-grace and stats tests (the race it found was rc134, fixed since rel61h2).
- **up148 `HS2_LB_UPLOSS`** (Iran, off): a second judge on Iran's own upload retransmits lowers or cuts upload-only lossy
  paths. **st149/st150/sd151 `HS2_LB_STRIPE_STEER`** (Iran and exit, off): stripes put no new sub or chunk on a
  cut path while a healthy one exists, a cut primary takes no width slot (nil-primary guard), and a refused
  pick asks the pool once and waits (no retry spin: asks 2-9/s → 0.06/s). Both keys are opt-in; tunnel 1 runs
  STRIPE_STEER as a canary through `Environment=`.
- Binary sha256 `5285ba8bd3bdbc28a4deee297bccb36fac305ea97a2dd24674c6d0d1a5ca3566`; 4 h soak `hs2-src/lab/rel65`.

## rel64 (branch rel64, base rs146) — HS2_MCONN_RESCUE_RETRY default on (edge)
- envdef: `{"HS2_MCONN_RESCUE_RETRY","1",Edge}` (table 68 keys). With JOINPICK (rs146 arm Y vs B): records lostjoin
  ~106 -> ~19 a run, acked raw+rec 0.966 -> ~0.978, retryfail 0. Exit side unchanged (the joins start on the edge).
- Restore the rs146 behaviour: `Environment=HS2_MCONN_RESCUE_RETRY=0` (Iran).
- 4 h soak, both sides rel64, production topology (lab/rel64): 0 panics / races, rescue answered 100 % / 99.7 %
  (see hs2-src/lab/rel64). Binary sha256 prefix 86459b95. Deployed on tunnel 1 2026-11-14 (kharej 11:18, Iran 11:20 UTC).
- Found later: under RETRY a repeated or late rescue answer could reset a rescued connection (audit rel66 H1);
  fixed by rs169 (`HS2_MCONN_RESCUE_IDEM`) in rel67b.

## rs146 (branch rs146, base rs145) — production-topology rescue lab, silent-join retry, JOINPICK + SCALE default on
- envdef: `{"HS2_MCONN_RESCUE_JOINPICK","1",Edge}`, `{"HS2_MCONN_RESCUE_SCALE","1",Both}` (table 67 keys). `HS2_MCONN_RESCUE_RETRY` stays opt-in.
- RETRY fix: a rescue join whose own link turns up in the rescue watch's silent set (its path died right after the join)
  is abandoned and replaced once, as a closed one (`rescueRetrySilentLocked`, counter `retrysilent`). Before, a join on a
  blackholed path was only noticed when its link closed at the health timeout: every lostjoin was an endwait and RETRY
  fired 0 times in rs145 and in the rs146 R arms. Test `TestRescueRetryRawSilentJoin`.
- Lab (`lab/rs146`): run.sh `TOPO146=1` (patch `run-topo146.patch`) = production topology, 6 Iran IPv4 × 2 kharej IPv4 + the
  IPv6 pair = 13 paths; a whole-Iran-IP outage darkens 2 paths. Scenario: t40 Iran IP1 dark, t42 cascade IP5+IP6 dark (the
  fresh other-IP links the rescue joins go to), back t80; t95 IPs 2-4 dark (6 paths) to t135. Crowd 6000, call, game, heavy.
  Per-key arms interleaved n=3 (two runs at once on lab1, cores 0-9 / 10-19: absolute p99 and crowd fails are CPU-inflated
  and too noisy to rank arms; counters are not).
- Results (means, A base → key): acked raw+rec A 0.869, J 0.934, S 0.907, R 0.866, B 0.966; records lostjoin A 297 → J 68;
  exit NAK A 95 → S 2.3; R alone = A (retry 0.3/run). Silent-retry binary: X (=R) retries 270/run, retryfail 0, records
  lostjoin 294 → 80 but no ack gain alone (retries fail to open without JOINPICK); Y (=B) records lostjoin 106 → 23, acked
  0.966 → 0.977 (n=3). JOINPICK found a fresh other-IP link every time (freshsameip 0, old 0). RSS max kharej +140 MB under SCALE
  (1.60 → 1.74 GB); Iran unchanged. Mixed runs fine: new edge + rel63c exit acked 0.946 (JOINPICK on), rel63c edge +
  new exit NAK 4 (SCALE on the exit); exit refused 0 in both. Details hs2-src/lab/rs146/RESULTS.txt.
- Binary bin146 (`CGO_ENABLED=0 -trimpath -ldflags="-s -w"`, lab1): sha256
  `71b1f003035c451b1cf1295b91ed2541b4ebe5b5f15328690e5876c923d397b5` (<work>\bin146). Tests (lab1, under lab load):
  gofmt clean, vet clean, `go test ./...` ok except 3 timing tests (TestCPUMeter, TestMConnLitePingsDualAndStall,
  TestMeteredConnBlockedTime) that pass 3/3 rerun on the idle lab; `-race ./engine` 0 races, 2 timing failures
  (TestDgDialsInFlightCount, TestL3DupEndToEndStuckLink) pass 3/3 rerun idle with -race, as do the rescue145/retry tests.

## rs145 (branch rs145, base rel63c) — rescue join pick, lost-join retry, ring scaling (keys default off)
- HS2_MCONN_RESCUE_JOINPICK=1: rescue join only on a link with fresh progress (read < HS2_MCONN_RESCUE_FRESH_MS=1000, or nothing pending), preferring another Iran IP than every left path; falls back to the old pick.
- HS2_MCONN_RESCUE_RETRY=1: a raw/records rescue join lost before its answer is replaced once on another path (RESCUE/RREC sent again, records since Bi resent from the records ring).
- HS2_MCONN_RESCUE_SCALE=1: records/raw ring budgets = one full ring per ring conn, bounded by HS2_MCONN_RESCUE_RAMPCT=4 % of MemTotal, never below the fixed budgets.
- Lab A/B n=4 (13 paths, crowd 6000, 1-IP and 2-IP outages): acked 90.9 % -> 99.1 %, exit NAK 80 -> 0, crowd fails -8..13 %, p99 flat, kharej RSS +270 MB. hs2-src/lab/rs145/RESULTS.txt. Binary <work>\bin145\hs2-linux-amd64 (sha 620c4f01).

## rel63c — rel62k3 + `HS2_MCONN_STRIPE_KEEPSTALL` default on (both sides) + per-path MTU view

rel63c = rel62k3 (f7431c6a: rel62 DL_FAST + RETIRE_KEEP_IDLE + DL_FAIR + rc134 smux ownership fix + raw137
raw boundary fix) + the rel63 keepstall branch (b46 7fdc4a46, squashed) + mt135 (per-path MTU view,
`HS2_LB_PMTU_HINT` default off). The only new default is KEEPSTALL (Both); envdef table 65 keys.
Unlike rel62k*, rel63c is meant for both sides (dark survival needs both on rel63c).
- Binary (`CGO_ENABLED=0 -trimpath -ldflags="-s -w"`, lab2 go toolchain, tree d4a3023b): sha256
  `f0d48eaef4152dc3279bc693dc2cf9ae985f94468b103de508bf8a38ea63b081` (<work>\bin63c).
- Tests (lab2, r63c): gofmt clean, `go vet ./...` clean, `go test ./...` ok, `-race ./engine` 0 races,
  `HS2_RC134_STRESS=1` x10 30/30, smux ok, installer sh tests ok after syncing `hs2-src/install/install.sh`
  with the root `install.sh` (mt135 had changed only the root copy; release_files_test).
- Lab A/B (lab1, `hs2-src/lab/r63c/results63c.txt`): dark download rel63c both 2/2 survive vs production pairing
  2/2 stall; rel62k3 edge + rel63c exit 2/2 survive, RSS bounded; quiet d8/u8 >= production, d1/u1 within noise
  (u1 n=18/19 p 0.28), crowd p99 equal, 0 panics. >= 4 h soak rel63c both on lab2 (unit rel63csoak).
- smux ownership fix (rc134b) and raw boundary fix (raw137) come from the cs141 lineage once; the b46 branch
  never carried them, so the raw137 patch was not re-applied.


- hs133 (rel63 fix): the receiver's hole rooms open only toward a peer whose sender keeps and resends
  (`capExtMConnKeepStall`, kindInfo ext bit10, announced with the key on), and the out-of-order bytes over the
  normal reorder bound are capped per connection (`HS2_MCONN_HOLE_ROOM_MB`, default 256 MiB) and per process
  (`HS2_MCONN_HOLE_ROOM_GLOBAL_MB`, default 512 MiB); past a cap the connection waits on the hole as before (gapCheck
  reset). rel63 edge + rel61 exit, dark download: edge RSS bounded (hs133 lab: max 675 MB, was 2672 MB). A 64 MiB
  per-connection cap made a both-new dark download die (hs133 dA), so the defaults are 256/512.

- envdef row `{"HS2_MCONN_STRIPE_KEEPSTALL","1",Both}` (table 65 keys with rel62k3's DL_FAIR); `HS2_MCONN_STRIPE_KEEPSTALL=0` turns it off
  (per side; no wire change, an older peer is unaffected).
- A striped heavy stream keeps each sub's chunks until the peer's smux consumed count passes them, closes a sub
  whose link went dark (ack stall checked against its TCP state; lazy poll, hs128) and resends on a live sub; the
  receiver's hole rooms (gap room, gap fill, hole sub) open only past 1.5 s without progress (hs131, was 500 ms, so
  ordinary sub-second lossy stalls no longer open them).
- Evidence: lab hs126..hs132 blackhole 60 s: heavy download/upload survive; quiet d8/d1/u8 interleaved key on vs off
  (lab/hs131/results131.txt, lab/hs132/results132.txt).
- Rollout: kharej (exit) first, then Iran (edge). Dark survival needs both sides on rel63; rel61 edge + rel63 exit
  behaves like rel61; rel63 edge + rel61 exit in a dark download grew the edge to 2.7 GB RSS (hs132, n=1) before
  the hs133 fix; with it either order is memory-safe (a mixed pair behaves like rel61 in the dark).
- rel63 (b46) = rel62k + hs128 lazy ack-stall poll + hs131 gapRoomAfter 1.5 s + hs133.

## rel62k3 — `HS2_MCONN_STRIPE_DL_FAIR` default on (Iran / edge release; the exit stays rel61h2)

- envdef row `{"HS2_MCONN_STRIPE_DL_FAIR","1",Edge}` (table 64 keys); `HS2_MCONN_STRIPE_DL_FAIR=0` on Iran turns it off.
- The download fast-stripe slot (`HS2_MCONN_STRIPE_DL_FAST`) is a 3 s lease taken only by a download's first growth
  (before: one long background download held it for its whole life), and a download at >= 80 Mbit/s may take one
  of 4 stripe slots past `stripeMaxConns` (8); uploads and slow streams never. Logged as `dlfair:`.
- Evidence: production single test downloads bimodal at night (one link 329-380 vs striped 807-1054 Mbit/s);
  lab cs141, a test download among 3-10 background users: 427 -> 752 Mbit/s mean, p10 217 -> 393, n=10 paired,
  9/10 wins, background throughput and light p99 unchanged (lab/cs141/results141.txt); df142 soak + A/B
  (lab/df142/results142.txt).
- rel62k3 = rel62k2 (5d95d5c7) + cs141 (6c8fc4d2) + this. No wire change; rel62k3 edge + rel61h2 exit.

## rel62k2 — rel62k + the two rel61h2 data-corruption fixes (Iran / edge release; the exit stays rel61h2)

rel62k2 = rel62k-on (`HS2_MCONN_STRIPE_DL_FAST` + `HS2_RETIRE_KEEP_IDLE` edge defaults on) + the rc134
smux buffer-ownership fix (8f7b45ce, 01b24ccb: `third_party/smux` is byte-identical to rel61h2's) + the
raw137 raw-reader boundary fix (`engine/mconn_raw.go`: an uncapped raw Read holds `unkMu`, `setReadLimit`
waits for it before the ack; see rel61h2 below). The rc134 opt-in stress test moved to
`engine/raw137_test.go` (DL_FAST off and on). No wire or format change: rel62k2 edge + rel61h2 exit is the
planned pairing.
- Binary (`CGO_ENABLED=0 -trimpath -ldflags="-s -w"`, go1.27.3, tree 95e68a2d; later commits are lab
  scripts, gofmt whitespace and this entry): sha256 `5d95d5c743e4d1353eff7e0d307c7efb62cad73023e6634a3f291df9593634d3`.
- Tests (lab2, rk139): `go test ./...` rc 1 with one failure, `TestStripeAllDieGraceRejoins` ("no grace
  or no rejoin counted"), a pre-existing flake: 2/12 on rel62k2 and 2/12 on the pre-patch tree 01b24ccb,
  also seen in the rel59 and mem67 runs; every other package ok; `go vet ./...` clean. `-race ./engine`
  rc 0, 0 DATA RACE (788 s). Stress `HS2_RC134_STRESS=1` ×10, run twice (20 runs, 1440 pairs):
  TestMConnRawSwitchChecksumStress dlfast-off and dlfast-on 20/20 each (36/36 pairs upgraded and checked
  per run, 0 mismatches), TestRawReaderNeverReadsPastLimit and TestSmuxWriteGiveUpNeverSendsReusedBuffer
  20/20. smux module `go test -vet=off` ok (vet's stdversion check on livecount.go, as before). Installer
  sh tests 24/24 (release_files on the full tree after the gofmt fix). The lab A/B +
  30-min soak (rel62k2 edge + rel61h2 exit vs rel61h2 both): `hs2-src/lab/rk139`, results on lab2
  `/opt/hs2lab/rk139/res` and `/opt/hs2lab/rk139/soak`.

## rel62k — `HS2_RETIRE_KEEP_IDLE` default on (Iran / edge)

- envdef row `{"HS2_RETIRE_KEEP_IDLE","1",Edge}` (table 63 keys); `HS2_RETIRE_KEEP_IDLE=0` on Iran turns it off.
- A pool shrink keeps live idle user connections on retiring links: idle reclaim waits `HS2_RETIRE_KEEP_IDLE_MIN`
  (60 m) instead of drain_idle (310 s) and the 20 m trickle force-close is skipped. Above max_links the surplus
  retiring links with the fewest open connections follow the old rules; cut-path drains and lb evac unchanged.
- Evidence: production Iran edge closed 8728 idle user connections in 24 h on retiring links; lab ec125 0 vs 34 EOF
  on 900 idle conns, RSS same; rk127 interleaved 65 min pairs (key off/on) with idle conns aged 5-55 min, storms,
  crowd churn and heavy, plus a max_links-pressure run (lab/rk127/results127.txt).
- rel62k = rel62 (675f6e0e) + id125 F4 (cherry-pick of 321102c3; `HS2_STORM_DEFER` not taken) + this.

## rel61h2 — two data-corruption fixes (rel61 + smux buffer ownership + raw reader boundary; both sides)

Deployed on tunnel 1, both sides (22:57 / 22:59 UTC). Every later release carries both fixes.

rel61h2 = rel61 (cfe30280) + the rel61h smux fix (below) + the raw137 raw-reader fix. Both bugs could put
wrong bytes into a user's stream; nothing else changes.

1. **smux buffer ownership** (rel61h, `third_party/smux/session.go`): a write that gave up while its frame
   was still queued let `sendLoop` send a reused buffer (another record's bytes). See rel61h below.
2. **raw reader past the boundary** (raw137, `engine/mconn_raw.go`): `readRaw` loaded `rLimit` (unknown)
   and then issued an uncapped Read. If the boundary arrived in between and this side's ack (Iran: UPACK,
   Kharej: its UPGRADE) let the peer write its first records on the first sub before that Read ran, the
   Read (smux returns buffered bytes even past the deadline) took up to 16 KB of record bytes as raw: the
   user got record headers in the stream and the reassembly lost them (seen in a lab trace). Now the reader
   holds `unkMu` across a Read issued while the limit is unknown, and `setReadLimit` waits for it before
   the ack goes out, so no uncapped Read can still run when the peer's records may come. Capped Reads do
   not take the lock.
- Tests: `engine/raw137_test.go` TestRawReaderNeverReadsPastLimit (deterministic: rel61 returns 1500
  bytes for a 1000-byte limit, rel61h2 1000) and the opt-in checksum stress TestMConnRawSwitchChecksumStress
  (`HS2_RC134_STRESS=1`): rel61 13 mismatches in 1440 pairs (20x), rel61h2 0 in 50x + 10x (-race 5x: 0 races).
- **No wire or format change**: rel61h2 and rel61 interoperate in both mixes (lab hv138); either side
  can be upgraded first.
- Lab (hv138, lab1): A/B rel61 vs rel61h2 both sides, d8/d1/u1/u8 + light p99; 30-min soak with link
  kills, RST bursts, blackholes and crowd churn; mixed both ways with a blackhole. Results:
  heavy Mbit/s medians rel61 / rel61h2: d8 1550 / 1516, d1 391 / 290, u1 426 / 383 (all noisy, ranges
  overlap), u8 1086 (n=5) / 1133 (n=6); light crowd p99 165-181 ms in every run, 0 crowd fails, no panics.
  Mixed d8 + 12 s blackhole: rel61 edge + rel61h2 kharej 1414, rel61h2 edge + rel61 kharej 1508, 1 crowd
  fail each (same blackhole, rel61h2 both 3 fails, rel61 both 7), no panics. Soak (30 min, rel61h2 both sides): 0 panics/alarms, one pid per side, 599 crowd
  fails, all inside event windows (rel61h soak: 696, 2 outside), RSS max ir 424 / kh 781 MB, links
  200-210, heavy 5/6 ok (the 6th: iperf3 3.16 died at 38 s of 40, harness). Lab scripts: `lab/hv138`.

## rel61h — smux buffer ownership hotfix

- `third_party/smux/session.go`: `writeFrameCut` could return early (write deadline, `WriteCut`, session
  die, socket error) while its frame was still queued for `sendLoop`, and `sendLoop` read `frame.data`
  later. hs2 reuses the caller's buffer (pooled chunk) as soon as `Write` returns, so the frame could go
  out carrying another record's bytes (data corruption on the stream). Now a write request has an owner
  state (CAS): if the writer gives up before `sendLoop` took the frame, it hands `sendLoop` a private copy
  of the bytes it had when `Write` returned; the frame still goes out (control.go's queued pings rely on it),
  and the caller's buffer is never read after `Write` returned.
- Same code as rel62k-on 8f7b45ce + 01b24ccb (rc134), applied to rel61 (cfe30280) with no other change.
  Regression test `engine/rc134_smux_test.go` (TestSmuxWriteGiveUpNeverSendsReusedBuffer, smux v1 and v2):
  rel61 sends 1000 corrupted bytes deterministically, rel61h none.
- **No wire or format change**: frame layout, versions, capabilities and env keys are unchanged, so rel61h
  and rel61 interoperate; either side can be upgraded first.

## rel61 — `HS2_MCONN_RESCUE` default on (both sides)

- envdef row `{"HS2_MCONN_RESCUE","1",Both}` (table 61 keys); records rescue (`HS2_MCONN_RESCUE_REC`) follows it.
  `HS2_MCONN_RESCUE=0` on either side turns it off (the capability is negotiated, so one side off = no rescue).
- Evidence: er97 blackhole lab failed connections -74 %; soak99 6.7 h with rescue on, RSS flat, 0 nocover / 0 failed rescues.
- Costs: kharej +~80 MB in the bounded rescue pools (64 MB raw + 48 MB records caps); crowd p99 +25 ms only inside
  blackhole windows, quiet runs unchanged.
- rel61 = rel60 (231f4753) + fl106..fl108 (`HS2_LB_FLAP_FAST` removed, `HS2_LB_CUT_NONEW` default on, Iran) + this.

## fl108 — `HS2_LB_CUT_NONEW` default on (Iran / edge)

- envdef row `{"HS2_LB_CUT_NONEW","1",Edge}` (table 60 keys); `HS2_LB_CUT_NONEW=0` on Iran turns it off.
- Proof (lab1, hs2-n 2bbf7a4b, interleaved, N = key on, A = off): flap dark phases 2-5 failed crowd connections
  N 0/0/0/0 vs A 97/93/111/73; single blackhole (30..150 s, n=5+5) N fails mean 145.8, back to >=5 links mean 168.6 s
  vs A 145.6 / 169.4 s, cut at ~41-42 s in both, 0 fails after the clear in every run; quiet runs
  crowd p99 unchanged (see `hs2-src/lab/fl107/results107.txt`, fl108 section).
- `HS2_LB_DEAD_HYST` stays off (see results107.txt fl108).

## fl107 — a cut path gets no new connections: `HS2_LB_CUT_NONEW` (Iran / edge, lab key, off; default on since fl108)

- fl106 found a flapping path (dark 25 s / clean 25 s) still cost ~16-30 failed crowd connections per later
  dark phase while the dialing side held it cut. Two leaks: the steering draw weighted a cut path by its
  reported share (floor 0.01), and `pickKey` ordered "pressed" before the path rank, so under a crowd's churn
  the cut path's 2 empty probe links were the only unpressed links and won new connections anyway.
- `HS2_LB_CUT_NONEW=1` (edge): a cut path gets 0 in the draw AND its links sort after every other link,
  before pressure (`pickKey.cutNew`). With every path cut, nothing changes. The path keeps its probe links and
  is drawn again as soon as the dialing side reports it uncut (unit test `TestCutNoNewReadmit`).
- Lab (lab1, 10 paths, crowd 3000, interleaved): flap, fails in dark phases 2-5: rel60 77-97 per run,
  draw-only key 63-74, full key 0 (see `hs2-src/lab/fl107/results107.txt`). Default stays OFF until the
  single-blackhole recovery (n>=4), quiet runs and the test suite are in (queued on lab1, `fl107/resb`).
- `HS2_LB_FLAP_FAST` (fl106) removed: measured no gain (the flap hold already engages at the second cut).

## rel60 — integration candidate (rel59 + lb89 + bh93..bh101 + er91..er97, base rel59 7e2cb19b)

- **lb89** `HS2_LB_SLOW_SKIP_COPY` default **on** (Iran / edge): the force close of trickling
  streams on retiring / cut-drained links skips mconn copy subs.
- **bh93..bh101** `HS2_LB_SILENT` default **on** (kharej / exit): a blackholed path is read as
  100 % loss and cut, held out while dark, probed every 2 s and back 2-8 s after it clears.
  `HS2_LB_SILENT_REGROW` stays a lab key (off).
- **er91..er97** RESCUE (raw + records mode, RREC/ACK/NAK 0x10-0x12, capExt bit9) carried with
  `HS2_MCONN_RESCUE` **off** by default; soak99 scripts.
- Not carried: cg86 multi-confirm, fc88/fc90 burst guard (all default off on their branches),
  rg104 cap/regrow. The two lb_silent tests that used cg86 helpers (`labMesh`, `cutSet`) carry
  their own copies. Two `TestDgPoolRoundTrip` race fixes met (er94 / bh95): er94's kept.
- envdef table: 59 keys (rel59 57 + SLOW_SKIP_COPY + LB_SILENT).
- Lab: `hs2-src/lab/rel60/` — soak60.sh (soak99 design, heavy harness fixed: fresh iperf3
  server per step, client timeouts, hang dumps), d60/seq60/j60 (A/B rel59 vs rel60 vs mixed).

## rel59 — integration candidate (base dl79 c41593e6; production behaviour unchanged)

Built on dl79 (rel58 → prod73 → gate74 → dl79), plus:

- **rt80** (CLI only): `-u` resolves classic tunnel names; the `join --update` /
  `tunnel code` endpoint is the tunnel IP (section below).
- **lbv81**: `HS2_LB_DEAD_HYST`, `HS2_LB_SELFQ`, `HS2_LB_SLOW_SKIP_COPY`, all **off** by default.
- **lb84**: `HS2_LB_PATH_SILENT=MS`, **off** by default, plus lab tools (`hs2-src/lab/lb84`).
- **hv83**: `HS2_LB_COMMON_GUARD=1`, **off** by default, plus lab tools (`hs2-src/lab/hv83`).

None of the new keys is in the default table (`envdef`), so the table is the same as
dl79's and, with no keys set, the data path runs exactly as dl79's does. sp82/cq85 are
not included (lab only).

## rt80 — tunnel names of classic installs, code endpoint = tunnel IP (CLI only; branch from gate74)

Two CLI bugs seen on tunnel 2 (classic install: unit `hs2-out.service`, config
`/etc/hs2/hs2-out.json`, listener on a secondary IP) with rel54c. The data path
(`hs2 run`) is unchanged.

1. **`-u` resolution.** `configForUnit("hs2-out")` looked for `/etc/hs2/out.json`;
   `-u hs2-hs2-out` found the config but then restarted the non-existent
   `hs2-hs2-out.service` (apply failed, rollback restart failed too). Now every
   `-u` flag resolves through `canonicalUnit` (unit_resolve.go): an installed
   unit of that exact name wins, else the tunnel configs are matched by unit name
   (as `tunnel list` shows it), config base name, `hs2-`+base, or the name
   without `hs2-` — so `hs2-out`, `hs2-out.service`, `hs2-hs2-out` and `out` all
   mean unit `hs2-out`. `configForUnit` takes the unit's real `ExecStart … run -c
   PATH` (drop-ins included, last wins) when that file exists, then the first
   existing of `<n>.json` / `hs2-<n>.json`. `join --update` canonicalises the
   code's name too; "no tunnel X" lists the tunnels that exist.
2. **Code endpoint.** The `join --update` / `tunnel code` code took the server's
   first public IP (default route, 203.0.113.41) as the listener endpoint even
   when `addr` binds a specific public IP (203.0.113.43) — running it on the peer
   would have moved the dialer to the wrong IP. `codeIP`: the bound public IP
   (addr for a listener, `bind_local_ip` for a dialer) > this side's first lb IP
   > the server's first public IP (wildcard bind, or a private bind behind 1:1
   NAT). `tunnel code --public-ip` still overrides. `join --update` warns when
   it moves the dialer's peer address.

Tests: `unit_resolve_test.go` (classic layout with/without unit file, drop-in
ExecStart, modern layout unchanged, endpoint for bind/wildcard+lb/wildcard/NAT
and the dialer side, peer-move warning).

Lab (lab1, netns slot 30, tunnel-2 shape: Iran `hs2-out` on secondary
198.51.100.157 with default-route IP .149, kharej with `bind_local_ip`): rel54c
CLI reproduced both bugs; rt80 CLI: `lb add-ip` (single IP, weight 100) ->
`join --update` on kharej (addr stays .157) -> `reality front both` (reload) ->
dialer learns the key, kharej restart: `now reality, reality ok/fail 11/0`;
iperf through the tunnel 12.9 Gbit/s (-P4, both directions), UDP 200M 0% loss;
Iran restart recovers on reality (40/0); target down -> `reality failed 3 times
… dialing tls for 10m0s`, tls 17/0, traffic passes; rollback `--to 1` on both
sides restores the original files byte-identical. Mixed runtimes all on reality:
rt80/rel54c, rel54c/rt80, and rel54c/rel54c driven by the rt80 CLI.

## bh101 — `HS2_LB_SILENT` ON by default on the kharej (exit) side (binary, kharej)

A blackholed path — its links have data waiting and get nothing acked while
the tunnel's other paths read normally — is read as 100 % loss and cut (bh93),
held out while it stays dark: only a byte acked on it ends the silence, so a
dark path is never re-admitted (bh98), and a path cooled for dials that got no
SYN-ACK is probed every 2 s, gets its share back step by step once a probe
connects, and the pool makes a spare dial for its due probe (bh100). The
dialing side decides and probes, so the default is exit-only (`envdef`:
`HS2_LB_SILENT=1`, Exit; 58 keys); an older or newer Iran side is unaffected.

Interleaved A/B on the production-like lab (lab1, N=20 heavy + crowd 3000,
260 s runs, blackhole 25..145 s; S1 = key on both sides, S0 = off; same
binary for every run): see `hs2-src/lab/bh101/results101.txt`.
- whole Iran IP (kh->ir dropped) and single path (49.2->53.1): S1 cut in
  6.5-7.3 s / 12.0-12.5 s, held 8/8, **0 re-admissions while dark** (0 in 21
  S1 runs since bh100, asymmetric included), back **2.3-7.6 s** after the
  clear (median 4.9 s / 5.0 s); S0 back 12.3-66.9 s (median 32.6 / 22.2 s),
  and key off an Iran->kharej-only blackhole was re-admitted while dark in
  1 of 2 runs. Crowd failures in the 30 s after the clear: 0 in every run,
  both arms; in the 30 s after the event no significant difference
  (p 0.40 / 0.21). Silent readings on paths that were not blackholed: 0.
- 1800 s lossy-alive soak with the key on both sides (bh100): 0 silent
  readings, 0 false cuts.
Off: `Environment=HS2_LB_SILENT=0` on the kharej.

`HS2_LB_SILENT_REGROW` (lab key, off): a path back from a blackhole regains
its links faster: a rebalance round may retire up to 4 idle links (one per
donor path) and 24 a minute (lb2DrainBatch 2 / lb2DrainPerMin 12 without it)
while a path is 4+ links under its share. Lab n=1 per scenario: it retired
more links (19 vs 12, 17 vs 8) but the blackholed path's links did not grow
back faster (clear+60 s: 6 vs 10, 3 vs 5), so it stays off.

## gate74 — the upload stripe gate: grow on the median, cut on the latest sample (binary, Iran)

prod73 reproduced production's upload gate in the lab (4 paths with netem
normal jitter 4 ms, paths c/d 1 % and 2 % loss, crowd 3000 on 40 links,
calls, game, one upload): with the 2 ms base cap the healthy path's own
standing queue (~4–5 ms) read as the stripe's, the gate cut ~120 times a run
and grew ~once. A 10 ms cap with a median of 3 for both decisions doubled the
upload but delayed the cut and cost echo p99 +29–31 ms.

`HS2_MCONN_STRIPE_UP_SPLIT=1` (**default on, Iran**) splits the two decisions:

- **grow**: the median of the last 3 samples (100 ms each) of the excess over
  the path's learned base — the 30 s lower decile, capped at 10 ms
  (`HS2_MCONN_STRIPE_UP_GROW_BASE_MAX`) — under half the target;
- **cut**: the latest sample alone, over the base capped at 10 ms
  (`HS2_MCONN_STRIPE_UP_CUT_BASE_MAX`), above 67 % of the target
  (`HS2_MCONN_STRIPE_UP_CUT_PCT`) — stricter than the band, so a spike still
  cuts at once (and holds growth 2 s as before).

Two variants, both labs (`labslot.sh --all`), arms interleaved; A = rel58:

| u1 (one upload) | n | upload mean / median / p10 Mbit/s | crowd p99 | call p99 | game p99 | echo p99 | ping p99 |
|---|---|---|---|---|---|---|---|
| A rel58 | 16 | 384 / 380 / 249 | 170.6 | 207.3 | 143.8 | 110.5 | 127.8 |
| V1: cut on the strict 2 ms base (100 %) | 7 | 370 / 365 / 170 | −0.7 | −42 | −13 | +12.7 (p 0.44) | +3.8 (p 0.61) |
| **W: cut on 10 ms at 67 % (shipped)** | 14 | **503 / 515 / 386** (p < 0.01) | −0.2 (p 0.55) | −17.8 | −19.7 | +4.0 (p 0.69) | +2.7 (p 0.95) |

V1 left the gate as it was (cut 107, grow 5 per run): with the 2 ms base the
healthy queue still crossed the cut line and every cut held growth 2 s, so it
was dropped after n=7. W: cuts 53, grows 12.5 per run (A: 121 / 1).

Regression check (u8 = one upload 8 streams; d1/d8 downloads). Second batch
(gate74b): the exact binaries — A = rel58 (5d87000d), W = gate74 (ab482840,
defaults), S = the same gate74 binary with `HS2_MCONN_STRIPE_UP_SPLIT=0` on
Iran; lab1 and lab2 `--all`, arms rotated every round. p = Mann-Whitney /
Welch.

d1 (one download), second batch alone, n A 9 / S 10 / W 10 (one A run lost
to an iperf3 "address already in use"):

| d1 | thr Mbit/s | crowd p99 | call p99 | game p99 | echo p99 | ping p99 |
|---|---|---|---|---|---|---|
| A rel58 | 277 | 171.4 | 181.4 | 116.0 | 137.3 | 127.0 |
| S gate74, split off | −37 (p 0.15) | −0.5 | −15.7 (p 0.60 / 0.58) | +24 (p 0.11) | −29.5 (p 0.21) | −4.2 |
| W gate74 | −45 (p 0.31) | −1.3 | +30.8 (p 0.21 / 0.30) | +6.3 | −10.6 | +3.3 |
| W − S | −7 | −0.9 | +46.5 (p 0.12 / 0.14) | −18 | +19 | +7.4 |

The first batch's d1 call p99 (+68–78 ms, p < 0.01, n 6/7) does not come
back: W − A is +31 ms (p 0.21). It is not the split: Iran's upload gate never
ran in any d1 run (`stripe_gate_*` 0 in all 30 status files, both keys), so W
and S run the same code in d1 — and still differ by +46 ms (p 0.12), which is
the size of this probe's noise. The call p99 per run is two-valued (~95 ms
clean, 190–285 ms when one stall hits the 2 Mbit/s call; runs over 150 ms:
A 6/9, S 6/10, W 8/10). Nor is it prod73: its diff from rel58 is counters
and logs plus two opt-in keys (`HS2_MCONN_STRIPE_UP_MED` default 1 = the old
path, `HS2_LB_CALL_STUCK_GAP_STRICT` off); with both off the decisions are
rel58's. The same noise turned the other way in d8 (call p99 W −98 ms,
p < 0.01), also a path the change does not run.

u8 / d8 (A / W), both batches pooled (the first batch's W was the gate74 tree
built on the lab with the keys set by env, the same settings):

| | n A/W | A thr | W thr | crowd p99 | call p99 | game p99 | echo p99 | ping p99 |
|---|---|---|---|---|---|---|---|---|
| u8 | 16/13 | 1339 | 1308 (p 0.44) | −0.9 | −35 | +1.0 | +13.0 (p 0.17) | −5.9 |
| u8, second batch only | 10/8 | 1414 | 1314 (p 0.14 / 0.25) | −0.5 | −19 | −9.6 | +26.3 (p 0.17 / 0.11) | −18 |
| d8 | 8/9 | 1017 | 1097 (p 0.27) | −4.2 (p < 0.01) | −98 (p < 0.01) | −5.9 | −5.9 | +4.4 |

In u8 the split runs (Iran gate cuts per run 494 -> 215, grows 0 -> 2) and
neither throughput nor a light user's p99 moved past the noise; the second
batch's u8 echo p99 +26 ms (p 0.11–0.17, runs over 150 ms A 1/10, W 4/8) is
the one number to watch in production.

Mixed versions (one u1 run each): Iran rel58 / kharej gate74 — upload 242
Mbit/s, crowd p99 174 ms, 0 fails; Iran gate74 / kharej rel58 — 411 Mbit/s,
crowd p99 175 ms, 0 fails (the gate is Iran's alone).

## wr72 — writeReady taken back out of rel58 (binary)

Does writeReady (29de93b5) cost download when paths are in trouble? F = rel58
as shipped (a3b768be, writeReady in), NW = the same tree with 29de93b5
reverted (5d87000d). Arms alternated every run, same slots for both.

- Scenario P (as rel58v, lab3, 3 slots, n=20 per arm): download NW 797, F 746
  Mbit/s (**−6.4 %**, p 0.23); paired by rep (same loss schedule) median
  ratio 0.950, F lower in 12 of 20 (signed-rank p 0.42). call p99 median NW
  248, F 273 ms; crowd p99 202 vs 203; ping p99 median 193 vs 167.
- u8 (lab1 `--all`, n=11 NW / 12 F; one NW run lost to an iperf3 segfault):
  throughput NW 2404, F 2363 (−1.7 %, p 0.78); call p99 **+24.8 ms**
  (107 → 132, p 0.07); echo p99 +4.4 ms (97.4 → 101.8, p 0.88); crowd p99
  +3.8 ms; ping p99 −6.8 ms. writeReady's +4.1 % u8 gain did not reproduce.
- u1/d1 (lab1 `--all` n=6, lab2 `--all` n=5–6): u1 throughput F −4.4 %
  (lab1) / −4.8 % (lab2), d1 +7.4 % / −1.0 %, all p ≥ 0.58; light p99 within
  ±2 ms except call p99 (noisy, ±10–24 ms both ways).
- **Decision: revert writeReady** (rule: ship F only if download within −5 %
  or p > 0.2 **and** light p99 within +3 ms; F failed the light p99 part,
  download was borderline, and it bought nothing in throughput). rel58v had
  already flagged W −17.7 % in P (p 0.06). `HS2_RELAY_LAZY` stays on.
- The shipped binary is exactly the measured NW build: sha256
  `5d87000d…a8fd46` (built from 3b12b25d, same tree as the revert commit).
  Checks on that tree (lab3): gofmt 0 (utls excluded), vet 0,
  `go test ./engine/... -run 'Relay|Copy'` ok, `go test ./envdef` ok.
  Previous binary kept as `bin58/hs2-linux-amd64.prev2` (a3b768be).

## rel58v — rel58 checked before production: ablation of the new keys, writeReady in, `HS2_QUEUE_CTL_UP` off (binary)

From rel58 (dc3a9582) + u8lazy's writeReady (1b802756, cherry-picked as
29de93b5; the relay writes ready mConn records with one writev, only in the
lazy relay, so `HS2_RELAY_LAZY=0` turns it off too). Arms: R = rel54c,
N = rel58, W = rel58 + writeReady, and N minus one key: q (`HS2_QUEUE_CTL_UP=0`
Iran + `HS2_QUEUE_CTL=0` kharej), c (`HS2_LB_RESCUE_CUT=0`), s
(`HS2_MCONN_STRIPE_SHARE_COPIES=0`), e (`HS2_LB_EDGE_SYNC=0`), l
(`HS2_RELAY_LAZY=0`). Interleaved, Mann-Whitney p.

- Scenario P (4 paths, 3 bad and flapping, one download, 90 s; lab3 + lab2
  slots, n=10–11 per arm; mean, p vs R): download R 783, N 718 (−8.4 %,
  p 0.25), W 645 (−17.7 %, p 0.06), q 689, c 690, s 614, e 690, l 773
  (−1.3 %). call p99 median R 410, N 244, W 260 ms (mean R 893, N 497) —
  not worse. crowd p99 R 239 → N 216 (−9.5 %, p 0.08), ping p99 454 → 283
  (−38 %). **No single key explains the download drop**: every one-key
  arm except l stays 12–22 % under R; rel58's n=4 "−20 % / call 766 ms" was
  mostly noise (per-run call p99 200 ms–2.9 s in every arm). Open: W in P
  (p 0.06) and the l arm sitting at R suggest the relay path, not an LB key.
- u8 (lab1 whole lab, n=8–12): echo p99 R 107.7, **N 116.7 (+9.0 ms,
  p 0.05)**, q 103.9 (p 0.05 vs N), l 103.7 (p 0.05 vs N), s 109.0, W 111.6
  (+3.9, p 0.20). The edge controller capped in 8 of 9 N runs
  (`queue-up … capped`), kharej's in none, so the cost is
  `HS2_QUEUE_CTL_UP`. Throughput N −2.2 %, q −1.9 %; W −10.9 % (p 0.01) but
  its runs came in a block at the end: against the R runs interleaved with
  it (4+4) W was equal (2261 vs 2265 Mbit/s).
- **Decision: `HS2_QUEUE_CTL_UP` defaults to off** (Iran, opt-in with `=1`);
  kharej's `HS2_QUEUE_CTL` stays on (it never capped in u8, d8 gain from
  rel58 kept). Table 55 → 54 keys; Iran "14 keys from defaults". The rest of
  rel58's defaults stay. Not re-measured after the change (out of time):
  the q arm is that build's u8 behaviour for uploads.
- Checks from a clean bundle (521df2f3, lab3): gofmt 0 (utls excluded),
  vet, `-race` envdef + engine defaults/qrunner/relay tests,
  `go test ./... -timeout 40m` rc 0. Binary sha256 `a3b768be…0424a`.
  writeReady tree (29de93b5, lab2): gofmt 0, vet 0, relay `-race` ok,
  `go test ./...` rc 0.

## rel58 — release candidate for tunnel 1: share63 + stuck61 + qctl63 + judge58 + mem70 on rel54c (binary)

From rel54c (5aebf590). Merged in this order: share63, stuck61, qctl63,
judge58, mem70 (every merge commit builds, vets and passes the engine tests
it touches, plus envdef and the smux module). Default table 50 → 55 keys.
What changes against production (rel54 + FAST) — every one off with `=0`
in that side's drop-in (details: README "Restore the old behaviour"):

- Iran: `HS2_MCONN_STRIPE_SHARE_COPIES=1` (share63, with the lane order),
  `HS2_LB_RESCUE_CUT=1` (stuck61; `_L3` stays off), `HS2_QUEUE_CTL_UP=1`
  (qctl63, with the qctl61 stuck-cap fixes and the qFastMinRate guard; the
  FAST queue stays off), `HS2_LB_EDGE_SYNC=1` (judge58). The edge log says
  "15 keys from defaults" (rel54c: 11, with FAST set explicitly).
- kharej: `HS2_QUEUE_CTL=1` (qctl63); "8 keys from defaults" (rel54c: 7).
- Both (mem70, no table key): the memory fixes (subs list, raw1 and onClose
  release, avoid-by-ID, deadKeys weak), `HS2_RELAY_LAZY` on (`=0` = old
  copy), one mem counters line a minute.
- Carried in, all OFF: `HS2_MCONN_STRIPE_EVAC`, `_EVAC_RAW`, `_EVAC_DOWN`,
  `_UPG_IDLE`, `_OVERLAP`, `_ADOPT_BACK`, XDIR, `HS2_QUEUE_CTL_FAST`,
  `HS2_SMUX_SMALL_FIRST`, `HS2_LB_RESCUE_CUT_L3`. **`HS2_SMUX_STALL_S`
  (mem67's frozen-session guard) defaults to 0 = off** here: its send-path
  close shut live but slow lossy links in the lab, and its receive evict
  shares the same timer and was never tested alone (`=15` turns both on).

Merge conflicts: CHANGELOG (all entries kept), README (restore bullets),
`envdef.go`/`envdef_test.go` (all keys, count 55), `defaults22_test.go`
(every branch's check), `mconn_stripe.go` (`evacHS` and `edge` both set on
the stripe sub), `mtcp_link.go` (AutoWindow, SmallFirst, StallTimeout),
smux `mux.go` (SmallFirst and StallTimeout fields). go.mod replaces intact.

Verification (lab1 + lab3; lab2 left to u8lazy):
- Clean clone 4d8c55da: gofmt 0 files (third_party/utls excluded), `go vet
  ./...` clean, `go test ./... -timeout 40m` rc 0 (lab1, a soak running
  beside it); `-race` on engine, envdef, cmd/hs2: 0 DATA RACE, one failure
  `TestStatsRoundTripSlowVsFastReader` (known under `-race` since rel53;
  passes without `-race` ×3). The smux module alone: tests pass with
  `-vet=off` (mem70's livecount.go uses go1.24 APIs while that go.mod says
  go 1.21, so go vet's stdversion check fails it; the build is fine and the
  go directive was left alone so the loop semantics do not change); under
  `-race` + load `TestAutoWindowDecaysWhenIdle` (timing) failed once, 0 DATA
  RACE. 24 installer sh tests rc 0 (`tm_edit_test.sh` real-vim `:wq` once
  under load; alone 27/0); `release_files` on the final tree: binary and
  install.sh hashes match. multi_tunnel not re-run (installer untouched
  since rel54c, where it was 57/0).
- Defaults seen in the logs: Iran 15 / kharej 8 keys from defaults.
- A/B lab3 whole lab, R = rel54c vs N = rel58, production-like keys (reverse
  l3mtcp, LB 3 paths, 100–150 links, crowd 3000, call, game, FAST on), n=6
  per arm (3 runs lost to iperf3 harness errors): u1 thr −10 % (p 0.38),
  light p99 within ±3 %; u8 thr −1.7 %, crowd p99 −16 % (p 0.04), call −6 %,
  **echo p99 +12 % (93.6 → 105.1 ms, p 0.01)**; d1 thr −3.5 %, echo +10 %
  (p 0.23); d8 thr −2.6 %, call −15 %, game −12 % (p 0.11); link kill
  (75 % at 12 s, n=2) thr +23 %, p99 ±4 %. 0 panic, 0 crowd/conn failures.
- Scenario P (4 paths, 3 bad and flapping, one download, 90 s, lab3 slots,
  n=4 per arm): download 814 → 648 Mbit/s (−20 %, high variance), crowd p99
  243 → 210 ms (−13 %), ping p99 301 → 198 ms (−34 %), call p99 median
  361 → 766 ms (one N run 2.9 s).
- Memory soak 31 min (lab1, 8 downloads + churn + 30 % kill every 3 min,
  SIGHUP every 10 min): post-GC HeapAlloc Iran 201 → 215 MB (+0.5 MB/min),
  kharej 414 → 424 MB (+0.2 MB/min after 10 min), 0 panic.
- Upgrade (lab1, rel54 classic units, drop-in `ExecStart=/usr/local/bin/hs2-t1.r54`,
  34 HS2_ keys, LB with an IPv6 path, alert timers; `install.sh upgrade`
  without GitHub): both units run `/usr/local/bin/hs2`, one `run -c` per
  config, 34 keys kept, alerts still oneshot timers, doctor 0 BAD, backup
  has the drop-ins.
- Mixed versions (lab3, 130 s upload + light users, FAST on): rel58 Iran /
  rel54 kharej 864 Mbit/s, rel54 / rel58 911, rel58 / rel54c 831, rel54c /
  rel58 831 — 0 crowd/conn/echo/call/game failures, 0 panic.
- Binary 4d8c55da, sha256 `8190ce6b…74d32`; `<work>\bin58\`.
- Open for the operator: u8 echo p99 +12 % (p 0.01) and the P download
  −20 % (n=4, noisy); the u8 cost may be mem70's relay change (agent u8lazy
  is testing `HS2_RELAY_LAZY`).

## rel54c — fast upload on by default again (operator's decision) (binary)

From rel54b (16897189, tag v-rel54). One change: `HS2_MCONN_STRIPE_FAST=1`
is back in the Iran (edge) default table. The operator accepted the measured
cost of rel54b's acceptance (whole lab, u1 n=12, during one heavy upload):
light p99 crowd +3.0 ms, echo +7.7 ms, ping +2.0, game +1.6 ms, in exchange
for +35–60 % single-connection upload. `HS2_MCONN_STRIPE_FAST_EARLY_MS=200`,
`HS2_MCONN_STRIPE_FAST_SOLO=1`, `HS2_MCONN_STRIPE_UP_PATHQ=1` and the reserve
(`HS2_MCONN_STRIPE_FAST_RESERVE`, code default 3) are unchanged. The edge log
now says "12 keys from defaults" (rel54b: 11). Turn it off with
`Environment=HS2_MCONN_STRIPE_FAST=0` on Iran.

- **No protocol change:** the diff from rel54b is `envdef.go` (one table row)
  and its test; the fast rule only changes how the Iran sender widens its own
  stripe, so an old kharej (r31, rel53) is unaffected (mixed runs below).
- **Confirmation** (whole lab per run via `labslot.sh --all`, all three hosts,
  arms interleaved; Y = rel54b binary 70cc33e4, Z = rel54c 19f63b6b, both with
  the production defaults; Mann-Whitney):
  | mode | n Y/Z | Mbit/s Y → Z | s1–5 | crowd p99 | call | game | echo | ping |
  |---|---|---|---|---|---|---|---|---|
  | u1 | 12/13 | 590 → 725 (+23 %, p=0.12) | 519 → 654 (p=0.068) | +0.7 (p=0.57) | −9.8 (0.46) | +2.8 (0.40) | +1.2 (0.27) | +0.2 (0.77) |
  | u8 | 6/5 | 2316 → 2442 (p=0.12) | 2044 → 2273 (p=0.055) | +13.5 (p=0.055) | −0.6 (1.0) | −13.5 (0.17) | −11.3 (0.52) | −8.4 (0.17) |
  | c (link kill at 6 s) | 6/6 | 304 → 702 (p=0.005) | 540 → 772 (p=0.020) | +4.1 (p=0.30) | +2.0 (0.17) | +1.3 (0.69) | +1.0 (0.17) | +1.4 (0.47) |

  The defaults engage: every Z run logs "edge side, 12 keys from defaults"
  with `HS2_MCONN_STRIPE_FAST=1` (Y: 11 keys, no FAST), and the Iran status
  shows the fast path at work (`stripe_fast_chained` > 0 in 13/13 u1, 6/6 c,
  5/6 u8 runs; median chained 3, early 1, reserved 3, bumps 7–9; Y: all 0).
  Against rel54b's FAST=1 build (574c2599, arm XF, lab3, n=4 each, interleaved)
  rel54c is the same: u1 752 vs 807 Mbit/s (p=0.89), crowd −0.1, echo −0.1.
  This short confirmation shows a smaller u1 gain than rel54b's acceptance
  (+23 % here, +35 % there at n=12) and no significant light cost on u1; the
  u8 crowd +13.5 ms (p=0.055, n=6/5) is in the range rel54b's isolation saw
  for FAST with eight uploads (B vs F +9.5, p=0.11; X vs A +5.3, p=0.69).
  Two runs lost their iperf3 heavy flow to "Address already in use" (one Y,
  one Z; harness) and are left out.
- Final checks: clean tree of e241d5dd (lab2): gofmt 0 files
  (third_party/utls excluded), `go vet ./...` clean, `go test ./... -timeout
  40m` all ok (rc 0, TestStripeAllDieGraceRejoins included), `-race` on
  envdef and the 66 engine `Stripe*` tests: ok, no DATA RACE; 24 installer sh
  tests incl. release_files: all rc 0, 0 FAIL. `multi_tunnel_test.py`: lab1
  55/2 (the known step 10 uninstall (ir) pty prompt timeout; another agent's
  whole-lab runs were going on there), then alone on lab3 (no mtir/mtkh
  netns, 2.6 GB free in /tmp) 57 PASS / 0 FAIL. Mixed versions (lab3, 130 s
  upload + light users, 150 links, no panic, 0 crowd fail, 0 call/echo/game
  /ping loss): rel54c Iran / r31 kharej 773 Mbit/s (crowd p99 122, echo 160
  in that run; repeat 910, crowd 81.9, echo 79.8; rel54b control 1006, 87.8,
  81.6), r31 Iran / rel54c kharej 1401 Mbit/s (crowd 84.0, echo 80.8).
- Binary 584e2279, sha256 `19f63b6b…ad343`; `<work>\bin54c\`.

## share63 — `HS2_MCONN_STRIPE_SHARE_COPIES` ON by default on the Iran (edge) side (binary)

From share62 (aa953369). The rest of the share62 gate, lab 2 only, same
production-like pool and harness (whole lab per run, interleaved, n=6 per
arm; A = key off, NV = key + LANEQ; mean of each run's p99, Mann-Whitney p):

| | multi upload (4 streams) | single download | multi download (4 streams) |
|---|---|---|---|
| Mbit/s mean A / NV | 1685 / 1700 (+0.9 %, p=0.63) | 422 / 369 (-12.6 %, p=0.47) | 1458 / 1396 (-4.2 %, p=0.63) |
| Mbit/s median A / NV | 1602 / 1718 | 434 / 387 | 1513 / 1368 |
| key's picks per run (NV) | 0-7 | 0 (all 6) | 0 (all 6) |
| crowd p99 delta | +4.5 (p=0.75) | +0.3 (p=0.52) | +329 (p=0.87) |
| call p99 delta | -13.4 (p=0.42) | +9.5 (p=0.52) | -1793 (p=0.15) |
| game p99 delta | +0.0 (p=0.75) | -1.1 (p=0.42) | -537 (p=0.87) |
| echo / ping p99 delta | -0.9 / +0.3 | -0.1 / -0.9 | +2.2 (p=0.20) / +324 (p=0.87) |

- No delta past the gate (-5 % throughput, +3 ms light p99) has p <= 0.2.
  The single-download gap is noise: the key never engaged in a download
  (0 share-copies picks and 0 stripe subs in all 12 runs of both arms; the
  key serves an upload's open only), and runs spread 251-583 Mbit/s in
  both arms. The multi-download light p99 swings are single runs of
  seconds in both arms (A call 9.6 s / game 3.3 s, NV crowd 1.4 s / ping
  2.0 s). One NV multi-upload run lost its measurement to an iperf3 segfault
  (client and server crashed; hs2 logged no error) and was re-run.
- With share62's single upload (n=20: median 920 -> 1193 Mbit/s, p=0.016;
  light p99 <= +2.0 ms): **default ON** for the edge (`envdef`,
  `loadEnvKeys`); the exit and `HS2_DEFAULTS=legacy` keep it off;
  `HS2_MCONN_STRIPE_SHARE_COPIES=0` turns it off, `..._LANEQ=0` the lane
  order. The NV arm also set `HS2_MCONN_STRIPE_ADOPT_BACK=1` (not a
  default); it set nothing aside in the uploads (0 in the j3 runs).
- Mixed versions (whole lab, 110 s with path 1 at 15 % loss from 7 s,
  crowd 3000 + call + game; the new side with no key set, so its defaults
  apply — the new Iran logs the key among its 12 defaults):

  | Iran × exit | dir | Mbit/s | crowd fail | call lost | panics / protocol errors |
  |---|---|---|---|---|---|
  | new × r31 | up | 456 | 0 | 0 | 0 |
  | new × rel54 | up | 87 | 0 | 0 | 0 |
  | r31 × new | up | 1268 | 3 | 89 | 0 |
  | rel54 × new | up | 525 | 0 | 0 | 0 |
  | new × rel54 | down | 531 | 0 | 0 | 0 |
  | rel54 × new | down | 641 | 0 | 0 | 0 |

  No errors. The r31 Iran's losses are on the old side (the key is
  Iran-only, so an r31 Iran runs without it). The 87 Mbit/s run was checked
  with a control on the same pairing (60 s, same loss, interleaved): key
  off 996 / 507, default 489 / 792 Mbit/s, call p99 227 / 422 against
  77 / 79 ms — uploads through a lossy path swing this widely in both arms
  (e59's mixed runs: 31-1069), so it is not taken as a regression. Worth
  watching on the first production upload with a lossy path.

## share62 — `HS2_MCONN_STRIPE_SHARE_COPIES` for uploads only, game lanes last (binary, default still OFF)

From adopt61 (4ac39c06). Lab 2 only, the adopt61 production-like pool
(min=max=40 links, crowd 3000, call, game, FAST on Iran); the harness now
really starts the upload after 10 s (the direct iperf3 control path skipped
`HEAVY_DELAY`) and picks a free `--cport` block per run (iperf3 "Address
already in use").

- **Uploads only**: Iran marks an upload's stripe open (`stripeCopyUpHint`);
  a download's shared pick keeps the copies rule and ignores the call marks,
  so downloads behave as with the key off (adopt61 saw them stripe too).
- **`HS2_MCONN_STRIPE_SHARE_COPIES_LANEQ`** (on with the key, `0` = off):
  links the TUN lane sent an interactive packet over in the last 10 s go
  last; never refuses. In the lab 2 lane links per pick were passed over.
- Interleaved, single upload, A = key off, NV = key + LANEQ (mean of p99s):

  | | A n=20 | NV n=20 |
  |---|---|---|
  | stripe subs | 2 | 8 |
  | Mbit/s median / p10 | 920 / 590 | 1193 / 828 (p=0.016) |
  | crowd / call / game p99 | 79.8 / 82.0 / 80.4 | -0.6 / +2.0 (p=0.083) / -1.2 |
  | echo / ping p99 | 81.7 / 80.5 | -3.4 / -1.9 |

- **Decision: still OFF.** The single-upload gate passes, but the download
  (single, multi) and multi-upload runs (n>=6 each) did not fit the session
  (one multi-upload A run only); they are the next step before the default.

## adopt61 — why the shared stripe pick failed in production, and `HS2_MCONN_STRIPE_SHARE_COPIES` (binary, default OFF)

From diag60 (de72239c). Lab 2 only (198.51.100.234, whole lab per run).
Production-like pool: rel54 production env, reverse l3mtcp, 3 paths, the
pool AT max (min=max=40 links), crowd 3000 (75 light users per link, no
empty link), call 2 Mbit/s, game; the upload starts after 10 s so the light
users' copies are in place (as in production).

- **Root cause of "stripe pick: none"**: the diag60 reason log, reproduced:
  `share: serving=40 busy=3 cand=0 skip[... copies=37 ...] refused=no-candidate`.
  The shared pick skipped every link with a copy-only stream
  (`linkCopies > 0`). Those are not only the calls' copies: lite gives every
  proven-light connection a second copy-only sub, so with thousands of light
  users every link has one. A single upload got the 2 subs it found before
  the copies spread and never grew (the production log: 0-1 shared subs).
  `HS2_MCONN_STRIPE_ADOPT_BACK` never engaged (0 "set aside": a pool at max
  has no idle retiring link; production's retiring links carry open conns).
- **`HS2_MCONN_STRIPE_SHARE_COPIES=1`** (Iran): copy-only streams no longer
  keep a link out of the shared pick (fewest copies first after the fewest
  flowing streams); the links a call rides are marked (as CALLFREE marks
  them) and skipped. `HS2_MCONN_STRIPE_SHARE_COPIES_K` (default 0) can cap
  the shared subs per connection on such links.
- Lab, interleaved, mean of each run's p99 against the key off (A):

  | single upload | A n=10 | N (K=8) n=10 | NK (K=4) n=6 |
  |---|---|---|---|
  | stripe subs | 2 | 8 | 4 |
  | Mbit/s median / p10 | 808 / 705 | 1163 / 933 (p=0.007) | 912 / 848 |
  | crowd p99 | 79.0 | +0.9 | +0.6 |
  | call p99 | 83.4 | +0.7 | +10.8 (p=0.018) |
  | game p99 | 78.3 | +5.1 (p=0.08, one 121 ms run) | +2.0 |
  | echo / ping p99 | 82.5 / 77.8 | +1.6 / +0.5 | -3.8 / +0.3 |

  Multi-stream (4 streams, n=2): A 1497/1511, N 2006/1528 Mbit/s. Download
  (n=2): with the key off it never striped (0 subs, 458/338 Mbit/s); with it
  8 subs, 830 Mbit/s, but call 97 ms and game 137 ms p99 (one valid run).
  Two runs (one per arm) lost the iperf3 to a lab port clash and are left out.
- **Not accepted**: the upload gain is clear, but game (N) or call (NK)
  went past the +3 ms budget and the download is not unchanged. Default OFF.
## stuck61 — `HS2_LB_RESCUE_CUT` on by default (Iran); `HS2_LB_RESCUE_CUT_L3` stays off (binary)

From stuck60 (0f54f11b).

- envdef: `HS2_LB_RESCUE_CUT=1` for the edge (Iran). `HS2_LB_RESCUE_CUT_L3`
  is not in the table (off unless set). Table: 50 keys. Tests: `TestSides`,
  `TestTableUnique`, `TestApplyDefaults`. README rows and the "restore"
  line (`HS2_LB_RESCUE_CUT=0` on Iran) added.

Lab 3, scenario P, whole lab (`labslot.sh --all`, one run at a time), same
binary as stuck59/60 (47bffa54). Reps 1–4 from stuck60, reps 5–8 from stuck61,
arms rotated in each rep. The loss pattern is seeded by the rep, so every arm
of a rep sees the same pattern: the table gives medians/means and the paired
difference (arm − base, same rep) with an exact sign-flip p over 8 pairs.

| arm | n | heavy Mbit/s med / mean | ping p99 med / mean | ping >120 ms % (med) | crowd p95 med | crowd p99 med / mean | crowd max med | call p99 med |
|---|---|---|---|---|---|---|---|---|
| base | 8 | 731 / 715 | 162 / 485 | 2.9 | 126 | 232 / 236 | 4218 | 326 |
| CUT only | 8 | 760 / 754 | 472 / 414 | 7.6 | **96** | **193 / 191** | 2588 | 297 |
| both | 8 | 681 / 676 | **86 / 99** | **0.7** | **107** | **192 / 194** | 2754 | 189 |

Paired, arm − base (8 pairs):

| arm | heavy (mean) | ping p99 | ping >120 % | crowd p95 | crowd p99 | crowd max |
|---|---|---|---|---|---|---|
| CUT only | +39 (+5%), p 0.31 | +30 med / −71 mean, p 0.78 | +0.4, p 0.91 | −30 (−24%), **p 0.008** | −45 (−19%), **p 0.008** | −37%, p 0.20 |
| both | −39 (**−5.5%**), p 0.53; median pair −13 | −50 med / −386 mean, **p 0.023** | −6.3, **p 0.031** | −22 (−17%), **p 0.008** | −41 (−18%), **p 0.008** | −35%, p 0.023 |

stuck59's slotted P (7 cores a slot, n=8 paired, both vs base): heavy −15%,
p 0.016.

Decision (the acceptance rule):
- both: ping and crowd clearly better, but the download is −5.5% on the mean
  (p 0.53): over the 5% bar, and stuck59's slotted run saw −15% (p 0.016).
  Not accepted. Neither key alone costs the download (CUT +5%, CUT_L3 −6%,
  n=4, p 0.25); the cause of the combined cost was not found in the time.
  The per-path samples show the download rides every path (60–300 Mbit/s on
  each cut path) and gets less of the good path in some "both" runs. It is
  not the lane or dup moves: they are 0–1 flows and ≤100 copies a run.
- CUT only: crowd p95/p99 −24%/−19% (p 0.008 each), download +5%, ping p99
  not better and not significantly worse. Accepted: on by default, Iran.

S/E/N for CUT only against stuck59's base runs (same harness, binary and
seeds; three lab slots of 7 cores, as stuck59 ran them). Medians, exact
Mann-Whitney p against base:

| sc | arm | n | heavy | ping p99 | crowd p95 | crowd p99 | crowd max | call p99 | crowd fails (sum) |
|---|---|---|---|---|---|---|---|---|---|
| S | base | 6 | 1054 | 95.9 | 92.2 | 159.8 | 804 | 237 | 0 |
| S | CUT | 6 | 1060 (p 0.70) | 97.5 (p 0.31) | 90.1 | **137.2 (p 0.002)** | 902 | 169 | 10 (one run 9) |
| E | base | 6 | 1451 | 101.3 | 90.4 | 128.8 | 328 | 156 | 0 |
| E | CUT | 6 | 1393 (p 0.49) | 99.5 (p 1.0) | 91.3 | 132.3 (p 0.09) | 342 | 159 | 0 |
| N | base | 4 | 1447 | 97.6 | 89.9 | 115.1 | 300 | 153 | 0 |
| N | CUT | 4 | 1510 (p 0.69) | 95.2 | 89.3 | 111.8 | 310 | 136 | 0 |

- S: crowd p99 −14% (paired p 0.031); download and ping unchanged.
- E: paired crowd p99 +4 ms (+3%, p 0.031), download mean −8% (p 0.44,
  MW p 0.49). Small; no cut is wanted in E, so the key
  should rarely act there. Watch it in production.
- N: no loss, no cut: the key never acts; nothing changes. N has n=4 (time).
- S rep 2 had 9 crowd failures with CUT (base 0); the other five CUT runs
  had 0–1. Not explained in the time.

Mixed versions, mode c (heavy upload plus a 75% path kill at 6 s), 2 min
each, Iran/kharej: stuck61/r31, r31/stuck61, stuck61/rel54, rel54/stuck61.
All four completed, with 0 panic/fatal/error lines in either log. Ping p99
98 / 122 / 98 / 79 ms.

Checks (clean clone of a5ae042c on lab 3): gofmt clean (without
third_party/utls), `go vet ./...` OK. `go test ./... -timeout 40m` ran
niced while the slotted A/B runs used all cores: two load-timing tests
failed (`TestMeteredConnBlockedTime`, `TestLabBursty26`), and the race step
hit a full disk (`no space left on device`; caches freed). Those were rerun
on the quiet lab: both tests pass, `envdef`, `cmd/hs2`, `core`, `encap`, `fec`
pass, and `go test -race` on engine (L3Cut, RescueCut, ApplyDefaults) and
envdef passes. The full suite was not rerun on the quiet lab (time).

Binary: `hs2-linux-amd64` sha256
fa818c26da5d20794eafd368b3ab386c21a7c6b98656140cc0c835cbbfe66c5e (built from
a5ae042c, CGO_ENABLED=0 -trimpath -ldflags="-s -w").

## stuck60 — per-tunnel cut-path set for `HS2_LB_RESCUE_CUT_L3`; P isolation A/B (binary, both keys still default OFF)

From stuck59 (22809e4b).

- `HS2_LB_RESCUE_CUT_L3`: the cut-path set is now per tunnel, not one set for
  the whole process. Each TUN link set (`l3Set`) owns an `l3CutSet`. Its links
  point at that set when they are added. The tunnel's judge writes into it:
  the exit's `LB` (`setL3Cut` in `runKharejReverse`) or the edge's
  `LinkManager` steer (`setL3Cut` in the Iran runner). Before this, two tunnels
  in one process shared the set, and a tunnel publishing "nothing cut" erased
  the other tunnel's cuts. Test: `TestL3CutPathsPerTunnel`. No wire change.

Lab 3, scenario P only (stuck59 harness), whole lab (`labslot.sh --all`, one
run at a time, ~2 min a run), interleaved in rotated order. n=4 per arm
because of the time budget; b200 (`HS2_LB_RESCUE_CUT_PER_SEC=200`) was
dropped after n=2. Means, with exact two-sided Mann-Whitney p against base:

| arm | n | heavy Mbit/s (mean / median) | ping p99 (mean / median) | ping >120 ms % (median) | crowd p95 (median) | crowd p99 (mean) | crowd max (median) | call p99 (median) |
|---|---|---|---|---|---|---|---|---|
| base | 4 | 704 / 751 | 383 / 144 | 2.8 | 128 | 244 | 4403 | 326 |
| CUT only | 4 | 742 / 777 (p 0.89) | 341 / 329 (p 0.69) | 4.2 | **95** | **187 (p 0.029)** | 3222 | 472 |
| CUT_L3 only | 4 | 660 / 724 (p 0.69) | 135 / **82** (p 0.20) | **0.2** | 126 | 242 (p 1.0) | 5334 | 1002 |
| both | 4 | 605 / 653 (p 0.34) | 93 / **85** (p 0.23) | **0.7** | **101** | **191 (p 0.029)** | **1917** | 197 |
| both, 200/s | 2 | 503 / 503 | 141 / 141 | 3.3 | 115 | 251 | 6654 | 598 |

Per run, the download changes a lot in every arm (base 418–895, l3 396–795).
Rep 4 had a low download in base and in l3 as well.

- Each key helps a different metric. CUT fixes the crowd (p99 −23%, p=0.029)
  and does not cost the download. CUT_L3 fixes ping (median p99 144 → 82 ms)
  and does not move the crowd.
- The download cost shows up only with both keys on: mean −14%, median −13%,
  p=0.34 here; stuck59 saw −17%. Neither key alone shows it. 200/s did not
  help (n=2).

Decision: both keys stay OFF by default. The acceptance rule is not met: ping
p99 is not significant at n=4, and the download is not shown to be within 5%
(it is −14%). S/E/N were not rerun because no config was chosen. Next steps:
run both against CUT only at n≥8, and find where the combined cost comes from.
Suspected: lane flows and the crowd's copies both crowd onto the one good
path's light links, and the stripe shares those links.

## stuck59 — light users off cut paths (`HS2_LB_RESCUE_CUT`, `HS2_LB_RESCUE_CUT_L3`) (binary, both default OFF)

From evac58 (d9b91834). Lab 3 only (whole lab, judge58 harness: reverse
l3mtcp, 4 paths, crowd 1500, call 2 Mbit/s, game, one download; P = paths 1-3
8-30% loss flapping, S = path 1 alone, E = 3 s blips, N = steady).

Root cause (census line `lb rescue census`, new): in P about 2,800 crowd
connections sit on links of lowered/cut paths; ~95% of them are DUAL with a
good copy elsewhere, only ~1,700 conn-seconds a run are single and raw ones
almost none (`budget-capped 0`: the raw/budget rescue was never the limit).
They stayed for the whole run (1,900 still there at the end) because nothing
ever removed a sub from a cut path while a copy existed, and the
`rescueDual` move (all subs marked) is 100/s. Interactive TUN flows (ping)
also stayed: the L3 lane keeps a flow on its link while alive, and the
HS2_L3_DUP copy is a rendezvous pick, as likely on a cut path.

- `HS2_LB_RESCUE_CUT=1` (Iran, with HS2_LB_RESCUE + HS2_MCONN_CLOSE_HS): a
  light dual connection (never striped) with a sub on a CUT path and an
  established copy off every marked link gives up the bad sub via
  CLOSE-REQ, the copy becomes primary (never the last sub); raw/single
  rescues and dual moves on cut paths take their own bound
  (`HS2_LB_RESCUE_CUT_PER_SEC`, 600/s).
- `HS2_LB_RESCUE_CUT_L3=1` (both sides, with HS2_L3_LANE): each side's judge
  publishes its cut paths (never all); a lane flow on a cut path moves at once,
  the dup copy is placed off cut paths.

Lab 3, interleaved, median (n in the table; heavy = download Mbit/s):

| scenario | arm | n | ping p99 | ping >120 ms % | crowd p95 | crowd p99 | crowd max | call p99 | game p99 | heavy Mbit/s | conns on marked links at end | bad subs given up |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| P (3 of 4 bad) | base | 8 | 156 | 2.7 | 127 | 235 | 7568 | 456 | 165 | 788 | 1906 | 0 |
| P | cut | 8 | **96** | **0.6** | **108** | **186** | **2176** | 678 | 145 | 657 | **22** | 2120 |
| S (1 bad) | base | 6 | 96 | 0.3 | 92 | 160 | 804 | 237 | 105 | 1054 | 891 | 0 |
| S | cut | 6 | 97 | 0.3 | 90 | **137** | 1123 | 160 | 122 | 1026 | **2** | 689 |
| E (blips) | base | 4 | 97 | 0.3 | 90 | 128 | 317 | 151 | 131 | 1451 | — | — |
| E | cut | 4 | 105 | 0.6 | 91 | 128 | 309 | 157 | 123 | 1418 | — | — |
| N (steady) | base | 4 | 98 | 0.3 | 90 | 115 | 300 | 153 | 116 | 1447 | — | — |
| N | cut | 4 | 97 | 0.4 | 90 | 115 | 287 | 145 | 110 | 1410 | — | — |

Time off: conns on marked links fall under 10% of their peak ~52 s after
onset in P (the cut itself comes ~30 s after onset) and ~32 s in S; without
the key they never do. Crowd failures P: base 23 (two runs), cut 1.

Left OFF: P's light metrics are clearly better and E/N/S unchanged, but the
download in P is lower by more than 5% (median, high variance) and call p99 is
noisy in both arms; needs n>=8 and a heavy-only A/B before default on.

## evac58 — download evacuation (`HS2_MCONN_STRIPE_EVAC_DOWN`) and the upload upgrade's link (`HS2_MCONN_STRIPE_UPG_IDLE`) (binary, both default OFF)

From upov57 (06ef8062). Lab 2 only (198.51.100.234, whole lab per run,
rel54 production env: reverse l3mtcp, 3 paths, 100-150 links, crowd 3000,
call 2 Mbit/s, game; path 1 goes lossy at 7 s).

- **`HS2_MCONN_STRIPE_EVAC_DOWN=1`** (Iran, with `HS2_MCONN_STRIPE_EVAC=1`):
  a download's stripe sub on a lowered path is moved through the close
  handshake: Iran sends CLOSE-REQ (never on the primary), the exit retires the
  sub and queues again, at the front of its live sub with the shortest queue,
  everything that sub holds or wrote within 2 s; CLOSE-ACK/CLOSE-DONE then
  close it cleanly; growth opens the replacement off the lowered path. Stripe
  subs now carry `evacHS` (the exit announced `flagMConnCloseHS`), kept apart
  from the lite subs' `closeHS`. Lab d1, 15 % loss: engages (17 moves, 15
  exit re-queues, no broken download), but D vs O (key off) n=8/9: ratio
  1.56/2.28 (p=0.086), post 767/897 Mbit/s (p=0.29), game p99 146/119 ms
  (p=0.11). The lab's download is not hurt by the loss (O post > pre), and the
  2 s re-queue is a burst of duplicates on one sub. **Not accepted: OFF.**
- **Upload evac** (`HS2_MCONN_STRIPE_EVAC`, evac56) at 30 % loss, E vs B
  n=6/6: post 573/254 Mbit/s (p=0.11), ratio 0.75/0.38, runs stuck at 0 for
  the rest of the upload 1/6 vs 4/6, game p99 89/174 ms (p=0.004), call p99
  152/316 ms (p=0.055). Promising, but n=12 per level (5/15/30 %) was not
  reached in this wave: stays OFF.
- **`HS2_MCONN_STRIPE_UPG_IDLE=1`** (Iran): an upload's raw upgrade puts its
  sub on the reserve, else any free link (even past the keep-free rule; the
  pool is asked for it again), else the shared light link, else the
  cross-direction link (last resort). Lab repro (a 22 s download striping,
  an upload starting at 8 s, `HS2_MCONN_STRIPE_LOG=1`): the raw handshake is
  76-112 ms in both arms — the FAST reserve already serves the upgrade; the
  multi-second production handshake is not reproduced, also with
  `HS2_MCONN_STRIPE_FAST_RESERVE=0` (75-143 ms, n=2/2). Upload under download
  Q vs P n=5/4: the upload's seconds 1-5 452/340 Mbit/s (p=0.14), the
  download 951/1130 (p=0.086), light p99 within noise. Speedtest-like (5
  uploads 0.5 s apart, 9 s each) Q vs P n=4/3: total 1935/1866 Mbit/s
  (p=1.0); single 10 s upload n=3/3: 1045/896 (p=0.28); light p99 within
  ±10 ms, none significant. Not accepted (no clear gain): **OFF.**
- Mixed versions (all new keys on): new Iran × r31 / rel54 / upov57 exits
  (d1 with degrade) and r31 / rel54 / upov57 Iran × new exit (u1 with
  degrade): no failed connection, no protocol error; CLOSE-REQ toward an
  upov57 exit (no re-queue) drained and the download continued.
## qctl63 — queue controller default ON for reverse l3mtcp

The path-queue controller is now part of the production defaults (envdef):
`HS2_QUEUE_CTL_UP=1` on Iran (edge), `HS2_QUEUE_CTL=1` on kharej (exit),
for the l3mtcp carrier in reverse only, like every other default. The
engine now reads both keys in `loadEnvKeys` (they were package-level
`os.Getenv` reads that never saw the table); tests: envdef table 49 -> 51
keys and per-side values, `TestApplyDefaults` checks both runners per side,
explicit `=0` and legacy. Evidence (docs §11.16; labs 1 + 3, T vs Q):
8 uploads n=13/arm call p99 -34% (p=0.033), game p99 -11% (p=0.022), all
light p50 better (p<0.001), heavy -2%; 8 downloads n=12/13 no cap, heavy
-4.9% (p=0.17); 1 upload (8/8), 1 download (12/12) and idle (7/7): no
significant p99 change. Mixed versions with r31 and rel54, both
directions, 2 min each: no errors. Turn off: `Environment=HS2_QUEUE_CTL_UP=0`
(Iran), `Environment=HS2_QUEUE_CTL=0` (kharej), or `HS2_DEFAULTS=legacy`.

## qctl62 — queue controller for tunnel 1 measured (docs only, keys stay OFF)

No code change. Lab A/B of the fixed controller (qctl61 binary) against the
tunnel-1 env (docs §11.15): with `HS2_QUEUE_CTL_UP=1` (Iran) +
`HS2_QUEUE_CTL=1` (kharej), 8 uploads: crowd p99 −15% (p=0.010), call p99
−37% (p=0.037), throughput +3% (n=6 per arm); under 8 downloads neither
side caps (no effect). With `HS2_QUEUE_CTL_FAST=1` as well: u8 throughput
−6% — out. Default stays OFF and tunnel 1 is unchanged: d8 reached only
n=3-4, u1 n=3, d1/idle were not run (gate needs n≥10 / n≥6); qctl63 is to
finish Q vs T before a default flip.

## qctl61 — the queue controller's stuck cap fixed (binary, keys stay OFF)

`HS2_QUEUE_CTL` / `HS2_QUEUE_CTL_UP` (opt-in, off by default; tunnel 1 runs
neither, tunnel 2 runs `HS2_QUEUE_CTL=1` on kharej): no cap starts while
the links deliver under 25 Mbit/s (`qCapMinRate`, also the FAST guard) or
receive more than 4x what they send (`qRevFrac`: the queue is the other
direction's; a cap in place comes off after 2 ticks); a cap that neither
binds nor idles for 8 ticks comes off as an idle one (`qLimboTicks`). Before,
under 8 downloads the edge capped its light upload at 6.7-9.1 Mbit/s for
good, holding the downloads' ACKs under it. Lab (docs §11.14): d8 0 stuck
caps (base 1 of 8; 6 of 13 in lat59), throughput 1977 vs 1772 Mbit/s
(p=0.02), u8 within noise. `HS2_QUEUE_CTL_FAST` stays off (not re-measured).

## lat59 — `HS2_QUEUE_CTL_FAST` only on real traffic (binary, key stays OFF)

`HS2_QUEUE_CTL_FAST=1` (lab key, off by default) now takes the fast start
(2 ticks) only when the links deliver at least 25 Mbit/s (`qFastMinRate`);
under it the normal 5-tick start holds. Lab (8 downloads): the edge saw the
download's queue behind its light upload (7 Mbit/s) and FAST capped that at
6.8 Mbit/s — a call's p99 went to ~2 s. Evaluation at 40 s heavy phases
(docs §11.13): u8 crowd p99 −16% (p=0.007), no other light class
significant, throughput within noise → not accepted, stays off. Found:
`HS2_QUEUE_CTL_UP` itself can cap the edge's light upload under downloads
and keep the cap (6 of 13 runs, d8 throughput −18%) — follow-up.

## upov57 — `HS2_MCONN_STRIPE_OVERLAP`: the stripe's chain does not wait for the raw upgrade's handshake (binary, default OFF)

From evac56 (4a21e33c). Production finding (tunnel 1, rel54, `FAST=1` +
`EARLY_MS=200` on Iran, 75 ms RTT): a single upload ran at ~370 Mbit/s for
~4 s and ~1000 after (`[496, 368, 371, 371, 1017, 979, …]`); up56's lab logged
`tick held hs=true`: after the raw → stripe upgrade the FAST doubling chain
waits until Iran reads the exit's `UPGRADE(Bk)`, and until then only the
upgrade's own sub takes new data.

- **Change** (`engine/mconn_stripe_overlap.go`, Iran, key
  `HS2_MCONN_STRIPE_OVERLAP=1`): the chain's extra stripe subs open at once
  after the upgrade's sub (in `openStripe`, and in the manager's tick while
  still handshaking). No wire change: Iran already writes offset-tagged
  records on new subs from step 1, the raw sub is gated (no new data on it,
  its queue only drains), and the exit keeps records past its raw position in
  reassembly (ooo) under the existing bound and budget until its raw reader
  reaches the boundary. The raw sub becomes an ordinary sub at UPACK, as before.
- **Capability**: ext bit4 `capExtMConnOverlap`, announced with the stripe on.
  Iran overlaps only toward an exit that announced it; r31 / rel54 / evac56
  exits keep today's order.
- **Failure**: RESEND/EVAC stay off while handshaking (unchanged); a sub dying
  in that window ends the connection (unchanged rule, now more subs are live
  in it — usually about one round trip).
- **Diagnostics**: with `HS2_MCONN_STRIPE_LOG=1` Iran logs
  `stripe <id>: raw handshake <dur> (subs N, sent raw B)` when the handshake
  ends, so production can measure the stall directly.
- **Tests**: `mconn_stripe_overlap_test.go` — order kept across the raw
  boundary with 3 stripe subs open while the exit's raw write is held (the
  handshake held on purpose), no overlap without key or peer capability, a
  sub dying in the overlap window ends the connection (no resend while
  handshaking), the gate. `-race` green.

Lab (whole-lab interleaved, reverse l3mtcp, 74 ms RTT, crowd 3000 + call +
game, FAST on; O = evac56 base, V = overlap; Welch p):

| scenario | n O/V | Mbit/s mean O→V | s1–5 O→V | crowd p99 | call p99 | game p99 | echo p99 |
|---|---|---|---|---|---|---|---|
| u1 | 12/11 | 781→795 (0.86) | 731→702 (0.66) | 78.9→79.0 | 111.7→110.5 | 77.6→77.9 | 77.0→77.6 |
| short 1 conn 10 s | 6/6 | 749→906 (0.12) | 730→762 (0.68) | 79.2→79.0 | 78.9→79.6 | 78.1→77.7 | 77.9→78.4 |
| short 5 conns 9 s | 6/6 | 1833→1982 (0.36) | 1707→1852 (0.33) | 109→116 | 105→121 | 85→98 | 94→88 |
| d1 | 6/6 | 800→758 (0.41) | 565→545 | 79.9→82.8 | 80.1→90.4 | 78.3→78.6 | 79.5→78.0 |
| u8 | 6/5 | 2430→2300 (0.16) | 2228→2081 (0.06) | 129→139 | 155→158 | 95→94 | 99→91 |
| d8 | 3/3 | 1794→1992 (0.14) | — | 118→92 | 708→84 | 84→99 | 116→91 |
| link kill (c) | 3/3 | 581→816 (0.09) | — | 81.2→81.1 | 83.8→83.1 | 80.5→79.2 | 77.0→89.1 |
| 8 bulk uploads behind | 4/4 | 321→298 (0.52) | 256→253 | 98→105 | 94→108 | 90→89 | 88→92 |

The overlap engages (lab logs: 2nd and 3rd stripe subs 0.7–1.3 ms apart with
the key, 76–126 ms = one handshake round trip without it), but the lab does
**not** reproduce production's ~4 s plateau: its handshake ends in about one
round trip, so seconds 1–5 are already fast in the base. The acceptance rule
(u1 seconds 1–5 +50 %) is not met → **default OFF**. Mixed versions (Iran
overlap × exit evac56 / r31 / rel54, and the reverse, up and down): 0 errors.
Next: production measurement with the new log line, then a lab whose first
sub sits on a deep shared queue (see the agent report).

## evac56 — `HS2_MCONN_STRIPE_EVAC`: a striped upload moves off a path that went bad (binary)

From rel54b (16897189). Production finding (tunnel 1, rel54, `FAST=1` on Iran):
a single upload at ~1.3 Gbit/s fell to 150–200 Mbit/s and stayed there while
a path went lossy and was lowered/cut (`lb auto2: … cut (loss 27.4%)`,
`lb cut-drain: … flowing ones stay`). Cause: the cut drain keeps flowing
connections and the rescue moves only light copies, so a heavy connection's
stripe sub on the bad path stays; the receiver reassembles by offset, so the
whole stream runs at that sub's pace (head-of-line).

- **Fix** (`engine/mconn_stripe_evac.go`, Iran, key `HS2_MCONN_STRIPE_EVAC=1`):
  Iran's manager tick closes a sub of a striped **upload** whose link is on a
  lowered or cut path (`rescueLowKey`), when another live sub is on a good
  path; the stripe resend (default on) re-queues everything it held, so no
  byte is lost. `pickStripe` (and its shared / cross-direction picks) skips
  lowered links while the key is on, so the replacement sub lands on a good
  path. Bounds: one move per connection per second, 8 per second per process
  (`HS2_MCONN_STRIPE_EVAC_PER_SEC`); never a light user or a call (only
  striped connections); never the last good sub; every move is logged
  (`stripe …: evac sub N off a lowered path`).
- **Peer gate**: new kindInfo ext bit3 (`capExtMConnResend`, announced when
  `HS2_MCONN_STRIPE_RESEND` is on). Iran moves a sub only toward an exit that
  announces it, so an r31/rel54 exit behaves exactly as before. Both sides
  need this build for the move to engage.
- **Uploads only**: the first build also moved download subs; lab d1 with a
  mid-run degrade then ended 3 of 4 downloads (0 Mbit/s from ~14 s: the exit,
  the download's sender, kept writing into the closed sub). A download needs
  the close handshake first (`HS2_MCONN_CLOSE_HS`) — not done.
- `HS2_MCONN_STRIPE_EVAC_RAW=1` (off, not measured): a raw single-sub
  connection on a lowered path still moving ≥ 1 MB/s grows a stripe sub on a
  good path below the growth rate; the move then takes its first sub off.
- **Lab** (whole lab per run, `labslot.sh --all`, lab1 + lab2, reverse
  l3mtcp, 3 paths, 37 ms each way, 100–150 links, crowd 3000 + call + game,
  `FAST=1` on Iran; one iperf3 -P1 22 s; path b to 15 % loss at 7 s → auto2
  lowers then cuts it; pre = s2–6, post = s9–21; Mann-Whitney; E = first
  build, F = final build, both with the key on, B = rel54b binary):

  | class | B | E+F | p |
  |---|---|---|---|
  | u1 degrade n | 17 | 17 (11 E + 6 F) | |
  | post/pre ratio | 0.89 | 1.06 | 0.029 |
  | post Mbit/s | 687 | 830 | 0.020 |
  | runs with ratio < 0.7 / < 0.5 | 3 / 1 | 1 / 0 | |
  | light p99 crowd / call / game / echo / ping ms | 217.4 / 153.3 / 106.5 / 114.0 / 108.6 | 218.7 / 162.4 / 147.0 / 113.0 / 112.6 | 0.22 / 0.74 / 0.11 / 0.80 / 0.20 |
  | u1 degrade, F alone n=6/17: post, mean Mbit/s | 687, 705 | 894, 891 | 0.042, 0.030 |
  | u1 degrade, F alone: game p99 ms | 106.5 | 207.4 | 0.059 |
  | d1 degrade, F (final build, uploads only) n=7/15: ratio, post | 1.94, 846 | 1.81, 787 | 0.55, 0.70 |
  | u1 no degrade (E) n=6/6: Mbit/s, crowd p99 | 870, 79.8 | 869, 80.8 | 1.0, 0.26 |

  The final build moved subs in 5 of 6 F uploads (1–3 moves a run, 1 s
  apart) and in the logged run `fL0` (3 moves, ratio 0.95); in no download.
  Mixed versions (whole lab, 40 s upload with the degrade, key on for the
  new side): new Iran + rel54 exit, rel54 Iran + new exit, new Iran + r31
  exit, r31 Iran + new exit — all ran the full 40 s, crowd failures 0,
  connection failures 0, 0 moves (the peer gate), as intended.
- **Default: off** — not fully accepted: one of 17 degraded uploads still
  fell under 70 % (B: 3 of 17), and the final build's game p99 under the
  degrade leans worse (n=6, p=0.059). To enable: `HS2_MCONN_STRIPE_EVAC=1`
  on Iran, this build on both sides.
## mem64 — memory growth: closed mconn subs kept until the connection ends; lazy relay buffers (binary)

From rel54b (16897189). Production: Iran RSS +~2.5 MB/min for 8 h, kharej too.

- **Root cause (Iran, the edge):** `mconnEdge.subs` kept every sub a user
  connection ever opened and dropped them only at `end`. A long-lived
  connection whose subs were retired, re-opened or lost with their link kept
  every closed one, and each pinned its smux stream and, through it, the
  stream's session (a dead link's whole session: streams, frames, TLS
  buffers). Soak (rel54, lab1, 4000 long-lived idle + 40 new conn/s + crowd
  3000 + heavy 8 + call, 30% link kill every 3 min): live `smux.newStream`
  objects on Iran 140k -> 614k in 9 min with ~7300 live connections and flat
  goroutines/fds; `mconnEdge.open` sub objects 24k -> 98k while live
  connections went 9k -> 14k. Fix: `mconnSub.Close` removes itself from the
  edge (`forget`). Test: `TestMConnEdgeForgetsClosedSubs`.
- **Per idle connection:** relays used `io.CopyBuffer` with a pooled 32 KB
  buffer held for the connection's whole life, also while waiting in Read:
  2 x 32 KB per side per live user connection (65% of the heap in the soak).
  `copyLazy` waits on a 2 KB buffer, takes the 32 KB one only while data
  streams and returns it on the first short read. 1000 idle relays after a
  1 MB burst: 8 KB each (73.5 KB before). `HS2_RELAY_LAZY=0` = old.
- **Also:** the exit closes a new mConn whose id is already taken (its manage
  goroutine leaked).
- **Soak** (same load, whole lab each; lab1 = rel54, lab3 = mem64; post-GC
  HeapAlloc on Iran at ~2/5/11 min): rel54 637 / 743 / 825 MB (892 at 16 min,
  906 at 22 min), live smux streams 140k / 471k / 614k; mem64 220 / 323 / 294
  MB, streams 72k / 135k / 142k. Kharej post-GC HeapAlloc: rel54 667 / 658 /
  641 MB (flat), mem64 388 / 325 / 353 MB. Iran RSS slope after 5 min: rel54
  +31.8 MB/min, mem64 flat. In the lab the pinned subs are freed when the
  connection ends (3-30 min lifetimes), so rel54's growth levels off; in
  production connections live for hours, so it keeps growing.
- **Left:** kharej RSS still rises ~5-6 MB/min in both builds while its
  post-GC HeapAlloc is flat (HeapInuse - HeapAlloc ~400 MB: span
  fragmentation / idle retention, not live data); throughput/latency A/B not
  run yet.
- Final checks (clean clone, lab1): gofmt 0, vet 0, -race on the new tests 0;
  binary `CGO_ENABLED=0 -trimpath -ldflags="-s -w"` sha256 `98562fad...9f05` (head 1408cb92). Iran post-GC HeapAlloc in mem64 still rose 294 -> 494 MB from 11 to 22 min (rel54 825 -> 982): the level is about halved, but the slope target is not met.

## rel54b — fast upload off by default; `HS2_MCONN_STRIPE_FAST_SOLO` (binary)

From rel54 (9eb3b199). Goal: remove the light-user cost of the fast upload
with many parallel uploads (u8 crowd p99 +18 ms in rel54's confirmation).

- **Isolation** (u8, whole lab per run via `labslot.sh --all`, all three lab
  hosts, arms interleaved, n=12 each; A = rel53, B = rel54, D = B +
  `FAST_RESERVE=0`, E = B + `FAST_EARLY_MS=0`, F = B + `FAST=0`;
  Mann-Whitney vs A):
  | arm | Mbit/s | s1–5 | crowd p99 | call | game | echo | ping |
  |---|---|---|---|---|---|---|---|
  | A | 2182 | 2006 | 130.0 | 160.4 | 125.5 | 117.1 | 101.7 |
  | B | 2400 (p=0.009) | 2426 (p<0.001) | +3.4 (p=0.54) | −24.9 (0.93) | −4.2 (0.75) | +5.8 (0.75) | +0.9 (0.80) |
  | D | 2395 (0.009) | 2405 (<0.001) | +2.8 (0.62) | −24.1 (0.93) | −4.7 (0.67) | +0.2 (0.71) | +11.4 (0.18) |
  | E (n=11) | 2287 (0.26) | 2008 (1.0) | +1.7 (0.88) | +1.0 (0.78) | −4.1 (0.93) | −6.8 (0.83) | +9.4 (0.33) |
  | F | 2168 (0.62) | 1928 (0.34) | −6.1 (0.37) | +3.4 (0.84) | −14.1 (0.24) | −3.1 (0.84) | +0.9 (0.53) |

  rel54's +18 ms did not repeat at n=12 (+3.4, p=0.54). B vs F crowd +9.5 ms
  (p=0.11): the fast rule costs a few ms with eight uploads; no single part
  (reserve, EARLY_MS) carries it alone.
- **`HS2_MCONN_STRIPE_FAST_SOLO`** (new, Iran, in the table as `1`; acts only
  with `HS2_MCONN_STRIPE_FAST=1`): the fast rule (one hot sample, the doubling
  chain, 250 ms retries) applies only to an upload that stripes alone; while
  another connection's stripe last grew for an upload (process counter
  `stripeUpActive`), uploads widen the rel53 way and a running chain stops.
  `=0` gives rel54's behaviour. Unit tests `TestStripeFastSolo*`.
- **Acceptance** (final code with FAST on = arm X vs A, whole lab, interleaved):
  u8 n=11/12: Mbit/s 2133 → 2217 (p=0.26), crowd +5.3 (p=0.69), call −18.2
  (0.34), game +0.7 (0.44), echo +13.3 (0.06), ping −1.2 (0.95) — passes
  (p ≥ 0.05). **u1 n=12: 620 → 838 (+35 %, p=0.002), but light p99 crowd
  +3.0 ms (p<0.001), echo +7.7 (p<0.001), game +1.6 (0.014), ping +2.0
  (0.026)** — on all three hosts (crowd per run A 80.9–85.1, X 84.0–88.5).
  c n=6/5: 363 → 616 (p=0.055), light equal or better; d1 n=6: 496 → 623
  (0.13), echo +2.8 (0.02); d8 n=6: 1609 → 1667 (0.81), light ±5.
- **Decision (operator rule: ~1–2 ms approved):** u1's light cost is above it
  and significant, so `HS2_MCONN_STRIPE_FAST` is **no longer a default**: rel54
  ships the mem55 memory fix (+ `HS2_MCONN_STRIPE_UP_PATHQ=1`). Turn the fast
  upload on with `Environment=HS2_MCONN_STRIPE_FAST=1` on Iran (EARLY_MS 200 and
  SOLO 1 then apply from the table). No wire change.
- Final checks: see `rel54-notes.md` (rel54b section).
- Binary ed6e7073, sha256 `70cc33e4…cd08c`.

## rel54 — release candidate: mem55 memory fix + fast upload on by default (binary)

From rel53 (18c5a999): merge of mem55 (size-classed mconn chunks) and up57
(up54–up57 upload-stripe work), then the operator-approved Iran-side
defaults. Only `CHANGELOG.md` conflicted (both entries kept); every commit
builds and vets.

- **New defaults (production table: reverse `l3mtcp`, Iran/edge only, not
  under `HS2_DEFAULTS=legacy`)**: `HS2_MCONN_STRIPE_FAST=1` (with its
  standing reserve `HS2_MCONN_STRIPE_FAST_RESERVE`, default 3),
  `HS2_MCONN_STRIPE_FAST_EARLY_MS=200`, `HS2_MCONN_STRIPE_UP_PATHQ=1`. The
  EARLY_MS and PATHQ keys are now read by `loadEnvKeys` from the envdef
  table instead of their own `init`. Still off: `HS2_MCONN_STRIPE_XDIR`,
  `HS2_MCONN_STRIPE_UP_NOSHARE`, `HS2_STRIPE_UP_SUB_WIN_MB` (kharej). The
  start log shows them (`defaults: production set on (edge side, 11 keys …)`).
  Turn off: `HS2_MCONN_STRIPE_FAST=0` (all of FAST, reserve, EARLY_MS) or
  `HS2_MCONN_STRIPE_FAST_RESERVE=0`, `HS2_MCONN_STRIPE_FAST_EARLY_MS=0`,
  `HS2_MCONN_STRIPE_UP_PATHQ=0` one at a time. No wire change.
- **Confirmation** (whole lab per run, `labslot.sh --all`, one run at a time
  per host, arms interleaved; A = rel53 binary, B = rel54 with its defaults;
  reverse l3mtcp, LB 3 paths × 1 Gbit/s 37 ms, 100–150 links, crowd 3000,
  call 2 Mbit/s, game; 12 s heavy; Mann-Whitney p):
  | class | A rel53 | B rel54 | diff | p |
  |---|---|---|---|---|
  | u1 mean Mbit/s (lab2, n=11/12) | 537 | 769 | +232 (+43 %) | 0.002 |
  | u1 seconds 1–5 mean | 469 | 647 | +177 (+38 %) | 0.004 |
  | u1 per-second median A | 368 590 470 448 472 461 408 536 631 575 677 641 | | | |
  | u1 per-second median B | 310 792 648 660 773 750 851 813 827 807 751 730 | | | |
  | u1 light p99 crowd / call / game / echo / ping ms | 81.5 / 85.2 / 81.6 / 85.6 / 78.6 | 82.3 / 83.3 / 83.5 / 80.8 / 78.9 | +0.8 / −1.9 / +1.9 / −4.8 / +0.3 | 0.78 / 0.64 / 0.83 / 0.88 / 0.93 |
  | c = u1 + 75 % link kill at 6 s (lab2, n=6/6) mean | 196 | 674 | +478 | 0.005 |
  | c per-second median A | 353 556 475 461 339 173 0 0 0 0 0 0 | | | |
  | c per-second median B | 342 807 745 784 930 621 277 547 676 776 869 721 | | | |
  | c light p99 crowd / call / game / echo / ping | 85.9 / 88.0 / 80.5 / 79.8 / 82.0 | 84.8 / 86.1 / 87.5 / 79.4 / 81.4 | −1.1 / −1.9 / **+7.0** / −0.4 / −0.6 | 0.69 / 0.69 / 0.17 / 0.38 / 0.58 |
  | d1 mean (lab3, n=5/5) | 639 | 703 | +64 | 0.30 |
  | d1 mean (lab2, n=5/6) | 475 | 682 | +207 | 0.036 |
  | d1 light p99 crowd / call / game (lab3) | 81.6 / 82.5 / 82.8 | 81.5 / 85.9 / 82.6 | −0.1 / +3.4 / −0.3 | 1.0 / 0.30 / 1.0 |
  | d1 light p99 crowd / call / game (lab2) | 85.5 / 88.4 / 87.5 | 91.5 / 91.8 / 88.9 | +6.1 / +3.4 / +1.4 | 0.17 / 0.93 / 0.93 |
  | u8 mean (lab3, n=8/10) | 2360 | 2479 | +119 | 0.12 |
  | u8 seconds 1–5 | 2132 | 2458 | +326 | 0.004 |
  | u8 light p99 crowd / call / game / echo / ping | 119.5 / 129.9 / 153.7 / 104.3 / 103.2 | 137.7 / 134.5 / 101.8 / 102.0 / 95.9 | **+18.2** / +4.6 / −51.9 / −2.3 / −7.4 | **0.10** / 0.89 / 0.20 / 0.76 / 0.53 |
  | d8 mean (lab3, n=6/6) | 1896 | 1851 | −45 | 0.81 |
  | d8 light p99 crowd / call / game / echo / ping | 99.7 / 93.1 / 94.5 / 86.1 / 88.3 | 98.5 / 116.1 / 95.0 / 89.8 / 84.1 | −1.2 / +23.0 / +0.5 / +3.7 / −4.2 | 0.81 / 0.38 / 0.81 / 1.0 / 0.30 |

  Isolation arm C (rel54 with `HS2_MCONN_STRIPE_FAST=0`, lab2 d1 n=6): 429
  Mbit/s vs A 475 (p=0.93) and B 682 (p=0.02): the size-classed chunks
  alone do not change d1. **Open: u8 crowd p99 +18 ms** (median 114 → 138; p=0.10 at n=8/10, p=0.043
  at the interim n=7/8, when call was +44 ms p=0.032). Not under the
  p < 0.05 stop rule at the final n, but too large to call verified: rel54
  is **not** a verified release candidate until a u8 retest (plan in
  `rel54-notes.md` of the operator). No other class has a light p99 worse by > 3 ms
  with p < 0.05. The c arm A's upload stops after the kill (0 Mbit/s from
  second 7), B keeps ~700, so B's light users share the paths with a live
  upload there.
- **Memory soak** (lab1 slot0, the rel53 soak: crowd 3000, heavy 8 down,
  calls, 30 % link kill every 3 min, SIGHUP every 10 min, 30 min):
  Iran RSS start / warm (300 s) / 600 s / 1200 s / end 327 / 620 / 624 /
  648 / 696 MB, peak 696 MB at 1688 s, slope after warm-up +3.2 MB/min
  (rel53 in the same soak: untraced mem55 window 320 / 610 / 631 / 642 / 642
  MB, +3.0 MB/min; rel53 lab1 slot1 646 / 689 / 1688 / 1831 / 1808 MB, peak
  1888, +35 MB/min; traced rel53 peak 1238 MB). No 1 GB+ peak; the
  ≤ 2 MB/min target is still not met (as in mem55: the rest is not chunks).
  Kharej 570 → 902 MB.
- **Final checks** (clean clone of the bundle): gofmt 0 files (third_party/utls excluded), `go vet ./...`
  clean; `go test ./... -timeout 40m`: all packages ok except
  `udpcarrier TestLabAdaptiveStep` under load (package untouched; alone
  ×3 ok); `-race` on engine, envdef, cmd/hs2: no DATA RACE, three timing
  failures under load — `TestStripeAllDieGraceRejoins` and
  `TestMConnStripeSlots` pass alone ×3, `TestStatsRoundTripSlowVsFastReader`
  fails under `-race` on rel53 too (2/3, same host) and passes without
  `-race`; installer sh tests all pass except two real-vim cases of
  `tm_edit_test.sh` on the loaded lab1 (installer untouched since rel53;
  alone on lab3 it passes, rc=0 fail=0 twice);
  `multi_tunnel_test.py` alone on lab2 (no mtir/mtkh netns, 7.9 GB free):
  first run 2 failures in step 10 uninstall (ir) (pty prompt timeout),
  second run 57 PASS / 0 FAIL (rc 0). Intermediate commits build and vet.
- **Mixed versions** (whole lab, 120 s single upload + light users):
  rel54 Iran / rel53 kharej 839 Mbit/s, rel53 Iran / rel54 kharej 1457,
  rel54 Iran / r31 kharej 965, r31 Iran / rel54 kharej 1235 — every run 0
  crowd failures, 0 call/echo loss, 0 ping loss, 150 links, no panic.
- Binary d7457e60fc97, sha256 `10b0df74…c9458`.


## mem55 — Iran-side memory: size-classed mconn chunks (binary)

Traced soak on lab1 (reverse l3mtcp, LB 3 paths, crowd 3000, heavy 8 down,
calls, 30 % link kill every 3 min, SIGHUP every 10 min, `HS2_MEMTRACE=1`
counting every chunk get/put and walking every live mConn every 5 s, heap
after two forced GCs every 5 min):

- **No lingering reference.** The chunk heap after GC follows the chunks
  logically held (`gets − puts`): 306 MB at 2 min, 128 / 79 / 30 / 24 / 26 MB
  at 5 / 11 / 17 / 23 / 29 min; live heap after GC 370–645 MB, flat after
  ~10 min (the relay copy-buffer pool is a constant ~197 MB of it).
- **What is held**: in steady state ~5 000 chunks, ~2 000 of them small send
  records in sub queues (~115 B of payload each) and the rest in writers'
  batches; at link kills the stripe's out-of-order list adds bursts of
  50–135 MB of full records (`out` p50 541, p90 7 168, max 20 557 chunks).
  Every chunk was one fixed 16 KB buffer (an 18 KB allocation), so 100 B
  records cost ×160. The peak sets the GC heap goal (GOGC=100) and the RSS
  follows the peak, then falls slowly (the scavenger): the traced rel53 run
  went 318 → 1 238 MB (at 5.5 min) → 769 MB at 31 min.
- **Fix**: `mchunk.buf` is a slice from size classes 512 B / 2 KB / 8 KB /
  16 KB with one `sync.Pool` each; `Write` and the sub reader size the chunk
  from the record (`getChunkN`), control records take 512 B, `getChunk(DATA)`
  stays full-size. Tests: `TestMChunkClasses`,
  `TestMChunkSmallWriteHeldMemory`.
- Soaks after the fix (lab1, untraced, 31 min): 308 / 608 (5 min) / 625 /
  672 / 718 MB, +4.1 MB/min after warm-up; rel53 untraced in the same
  window 320 / 610 / 631 / 642 / 642 MB at 17 min, +3.0 MB/min. In this
  window only the traced rel53 run had the standing small-record queues;
  the traced fix runs had none (`out` max 543), so the soak does not yet
  show the fix under the condition that reproduced. The ≤ 2 MB/min target
  is **not met** by either build: the remaining slope is not chunks (chunk
  heap after GC ~25 MB; the heap diff from 5 to 28 min is ~50 MB spread
  over smux/tls buffers) — open, see mem55 notes.
- c/s A/B on lab2 (4 rounds, rel53 and fix in parallel slots, 90 s, crowd
  3000 + heavy 8 + call): heavy median 1326 (n=3, one iperf start failed)
  vs 1346 Mbit/s, game ping p50 94 vs 93 ms, p99 178 vs 179 ms — no
  regression.
- `GOMEMLIMIT=700MiB` as a safety net held the RSS at ~500 MB but cost ~20 %
  more CPU (2.30 vs 1.92 cores) in the same soak: not enabled by default.

## up57 — upload stripe: early upgrade, per-path gate; where the light users' cost came from (binary; keys off by default)

On top of up56 (412b9397). All new keys are Iran-side and **off**; no wire
change (old peers work both ways — lab: new Iran/old exit and old Iran/new
exit, single upload, 0 crowd failures, no protocol errors).

- **`HS2_MCONN_STRIPE_FAST_EARLY_MS=N`** (with FAST, 50-500): a raw
  connection's first second is sampled every N ms, so a fast upload (≥
  `HS2_MCONN_STRIPE_FAST_MBIT`) starts the raw → stripe upgrade at ~2N ms
  instead of at 2 s, before the raw link's queue fills (the up56 handshake
  waited ~4 s for it). Only uploads; downloads keep the 1 s ticks.
- **`HS2_MCONN_STRIPE_UP_PATHQ=1`**: the upload gate also takes the light
  users' queue per path (Iran's local IP of the link; worst path with ≥ 4
  active light links); it only ever closes the gate earlier.
- **`HS2_MCONN_STRIPE_UP_NOSHARE=1`**: an upload's stripe sub never takes a
  light user's link (STRIPE_SHARE's shared pick). Lab: no measurable effect
  (Q vs S n=6/6, light p99 ±8 ms p ≥ 0.17) — kept off.
- Where the +7..+15 ms light p99 came from: **CPU contention in the lab
  slot**, not the path queue or link sharing. Per-path netem backlog and the
  light links' srtt (ss, every 0.2 s) barely moved with FAST (median +0-1 ms,
  p90 +1 ms); the slot's 7 cores went from 72-76 % busy (A) to 80-86 % (Q).
  Three parallel slots per host made it far worse (A alone 781 Mbit/s and
  crowd p99 81 ms vs 330 / 186 with three slots). On the whole lab (20
  cores) the cost is gone within ~1-2 ms.
- Lab (rel53 production keys, reverse l3mtcp, 3 paths × 1 Gbit/s, 37 ms each
  way, 100-150 links, crowd 3000 + call + game, 12 s heavy; arm A = FAST off,
  Q = FAST + EARLY_MS=200 + PATHQ; interleaved; MW two-sided p):
  - whole lab, single upload, n=13/13: mean 587 → 936 Mbit/s (p<0.001),
    seconds 1-5 526 → 847 (+61 %, p<0.001); per-second medians A 373 670 558
    549 563 560 439 574 634 644 762 764, Q 342 974 850 853 877 976 1080 873
    848 868 1123 1033; light p99 crowd +0.9 ms (p=0.058), game +1.7
    (p=0.13), call −3.1 (p=0.44), echo +0.4 (p=0.28), ping −0.4 (p=0.82).
  - whole lab, upload with 30 % link kill at 8 s, n=5/6: 517 → 833
    (p=0.008); light p99 within ±1 ms (p ≥ 0.12).
  - one 7-core slot, n=12/12: single upload 497 → 772 (p<0.001), seconds 1-5
    410 → 630 (+54 %); light p99 crowd +4.9 (p=0.03), echo +15 (p=0.012),
    game +7.9, call +6.2; d1 444 → 731 with crowd +17.7 (p=0.022); c, u8,
    d8 (n=3-6) faster, light p99 mixed.
- Verdict: the throughput and first-seconds targets are met; the strict
  "light p99 p > 0.2" is not (crowd +0.9 ms p=0.058, game +1.7 ms p=0.13 on
  the whole lab), so the defaults stay **off**. The ready default patch
  (`HS2_MCONN_STRIPE_FAST=1`, `_FAST_EARLY_MS=200`, `_UP_PATHQ=1` on the
  edge in envdef, with tests) waits for the operator's call.

## up56 — upload stripe: link reserve, instant pool target, seconds 1-5 (binary; keys off by default)

On top of up55 (9331efc1). Every change is behind `HS2_MCONN_STRIPE_FAST`
(Iran/edge, still **off**).

- **`HS2_MCONN_STRIPE_FAST_RESERVE=N`** (with FAST; default 3, 0 off, at
  most 6): the pool keeps N idle links set aside for the stripe at all times,
  on top of the autopilot's target (capped by max_links; a set-aside link is
  heavy for every light pick), renewed each health tick; a sub that takes one
  asks for its replacement at once (one per link taken). Status
  `stripe_fast_reserved`, `stripe_fast_refills`.
- **Instant pool target** (FAST): a stripe's request raises the pool target
  by the new links right away (`stripeWantRaiseLocked`), instead of at the
  next 2 s health tick — the lab log showed the reserve's replacements and
  the doubling chain's links set aside in batches exactly 2 s apart.
  Status `stripe_fast_bumps`.
- The doubling chain resumes the moment the raw → stripe upgrade's handshake
  ends (manager kick; `stripe_fast_hs_kicks`).
- Log: a connection whose subs all ended after the peer's FIN now ends with
  "mconn: peer closed (every sub ended after its FIN)" instead of "every sub
  died" (no wire change).
- Lab (lab3 only — lab1 was fully used by mem55; rel53 production keys,
  reverse l3mtcp, 3 paths, 100-150 links, crowd 3000 + call + game, 37 ms
  each way, 1 Gbit/s, 12 s heavy, arms interleaved over 3 slots; MW = Mann-
  Whitney two-sided p):
  - single upload, A (FAST off) vs J (FAST + reserve + instant target),
    n=12/12: mean 511 → 889 Mbit/s (p<0.001), p10-second 400 → 415,
    seconds 1-5 441 → 485 (+10 %, p=0.40). Per-second medians A 337 487 490
    503 486 492 435 560 568 587 669 728, J 364 476 419 483 662 989 1280 1472
    1076 1033 1145 1334. Light users' p99 (mean): crowd 93 → 108 (+15,
    p=0.007), call 108 → 116 (+8, p=0.08), game 96 → 104 (+8, p=0.31), echo
    98 → 102 (p=0.54).
  - the reserve alone (G, n=12+12 in two rounds) did not move seconds 1-5
    (385-412 vs F 445 vs A 376-428): the links are there, the time goes to
    the raw → stripe upgrade handshake (the logged run: `tick held hs=true`
    4 s into the upload; Iran's UPGRADE waits until the exit has read the raw
    sub up to the boundary, i.e. the raw link's whole in-flight queue).
  - tighter upload gate (`HS2_MCONN_STRIPE_UP_QMS=2`, arms H/K): light p99
    back within ±7 ms of A (p ≥ 0.2) with mean +163-299 Mbit/s, but seconds
    1-5 not up and the TUN ping p99 +13 ms (K, p=0.018).
  - multi-stream download (s = d8), n=8/6: unchanged (1638 vs 1614, p=0.95;
    light p99 −3..−13). Upload with 30 % link kill at 8 s (c), n=8/7: mean
    388 → 712 (p=0.002), light p99 +6..+16 (p ≥ 0.12). u8 1917 → 2176
    (p=0.056). Single download (the reserve serves download stripes too),
    d1 J n=6/6: 409 → 819 (p=0.005), light p99 within ±4 (p ≥ 0.47); an
    earlier d1 G round n=8/8 had crowd +27 (p=0.014), game +23 (p=0.018).
- Verdict: median and p10 up, but seconds 1-5 only +10 % (target +50 %) and
  the light users' crowd p99 +15 ms (p=0.007) under one upload — acceptance
  not met, the FAST default stays **off**. The trade: +70 % single-upload
  throughput for +8-15 ms crowd/call p99 while that upload runs.

## up55 — fast upload stripe widening (binary; keys off by default)

On top of rel53 (18c5a999) with up54's `HS2_MCONN_STRIPE_XDIR` (off) and its
diagnostic line.

- **`HS2_MCONN_STRIPE_FAST=1`** (Iran/edge, off): a heavy upload stripes
  after one hot sample (downloads keep `stripeHotTicks`=4), the width doubles
  while every sub carries `HS2_MCONN_STRIPE_FAST_MBIT` (30) — the first growth
  1 → 3, then n → 2n up to K (and STRIPE_UP's cap), each extra sub opened right
  after the previous one through the same picker (share / keep-free rules
  unchanged) — the pool is asked for that many links at once
  (`stripeWantHint`, not one per pick), and a failed open is retried every
  250-500 ms (6 tries) instead of waiting 2 s. Status:
  `stripe_fast_chained`, `stripe_fast_early`, `stripe_fast_retry`.
- **`HS2_STRIPE_UP_SUB_WIN_MB=N`** (kharej/exit, off, 3-8): a fixed smux
  window for an upload's stripe subs. Rejected in the lab (light users' p99
  worse, throughput not better than FAST alone); kept off.
- Lab (lab3, rel53 production keys, reverse l3mtcp, 3 paths, 100-150 links,
  crowd 3000 + call + game, 37 ms each way, 1 Gbit/s; HEAVY_OMIT=0, 12 s):
  single upload mean median 364 → 595 Mbit/s, p10 293 → 373 (n=12/16); u8
  927 → 1329; d1/d8 unchanged (no code path). Seconds 1-5 only 318 → 336:
  the stripe waits for the pool to dial links no user rides (~1.5-2 s a
  round) and both arms dip at second 2 (the raw → lite upgrade). Light-user
  p99 under a single upload +15-20 ms median (crowd 114 → 129, call 127 →
  138) — within the run-to-run spread (d1, where nothing differs: crowd 101
  vs 154) but not proven equal, so the default stays off for the operator's
  call.
- Per-sub cap: `ss -ti` of Iran's link sockets during an upload: every busy
  link app-limited, `rwnd_limited` ~1 %, ~1300-1500 segments (~2 MiB)
  unacked — the exit's smux window, which the adaptive rule hardly grows
  for an upload (`aw_kh` peak 2 MiB, 1-2 grows a run): ~220 Mbit/s a sub at
  75 ms.
- Upload end: "every sub died" (RESEND on; "a sub died holding the only
  copy" with it off) is the exit closing the connection after its target
  (the iperf3 server, told TEST_END over the direct control path) closed with
  data still in flight in the tunnel (sent 730.6 vs received 711.6 Mbit/s):
  the exit's log shows a local close of every sub (`read/write on closed
  pipe`), Iran then sees `peerFin` and EOF on each sub, `rst=false`. Not a
  tunnel fault; only the label is misleading.

## rel53 — release candidate: the three rev53 reviews merged (binary)

Merges rev53a (installer/menu), rev53b (data plane) and rev53c (operations)
on top of rel52 (7ad72f77). **rel52's installer has a critical bug** (with
alerts on, `install.sh upgrade` turned `hs2-alert-<unit>.service` into a
second `hs2 run` on the live tunnel's config) — rel53 fixes it and heals a
server an older upgrade damaged; do not upgrade with rel52's `install.sh`.

- **installer, one implementation** of what rev53a and rev53c fixed twice:
  `dropin_exec_sync` (a drop-in `ExecStart=<other binary> run …` is pointed at
  `/usr/local/bin/hs2`, written atomically, the original kept as
  `*.pre-upgrade`, one `daemon-reload`, a wrapper such as `taskset … hs2 run`
  only reported), `running_exe_ok` (after the restart `/proc/PID/exe` must be
  the upgraded binary, compared resolved; otherwise the tunnel counts as
  failed), `tm_units` (skips oneshot units **and** the reserved helper names
  `hs2-stats`/`hs2-lb-park`/`hs2-coreguard`/`hs2-alert-*` unless the unit
  really runs `run -c`), `heal_alert_units` before the tunnel list, drop-in
  aware `tm_cfg`. Both branches' tests kept (`helpers_test.sh`,
  `upgrade_dropin_test.sh`).
- **menu (Go) upgrade path:** `upgradeBinary` no longer says "upgraded" when a
  drop-in keeps the tunnel on another binary; it names the running exe and the
  fix (`TestExeIsBin`).
- **tests:** two `-race` reports that were test-only (a `fillMin` goroutine
  started with HS2_LB_AUTO2 off outliving its test; `shapeSmall`/`shapeBulk`
  toggled under sessions left by earlier tests — now atomics, same production
  behaviour) and the flaky `TestMConnStripeSlots` (now checks the
  connection's own slot, not the process-wide counter).
- **Iran-side memory (investigated, not fixed in rel53):** the rel52 soak's
  Iran RSS climb reproduces with the **r31 binary and r31 keys** on the same
  harness (lab1: r31 peak 1.56 GB, rel53 1.89 GB; kharej flat), so it is not
  new in rel52/rel53. For r31 it is mostly runtime retention (live heap after
  two forced GCs 378 MB at RSS 1.55 GB). rel53 on lab1 once held 1.05 GB of
  live 16 KB mconn receive chunks after three GCs; a traced rel53 run on a
  quiet lab (lab2) stayed at 0.6–0.65 GB with 17 MB of chunks live.
  Goroutines and fds are flat. Not root-caused; tables and the plan:
  `rel53-notes.md`.

### rev53a — installer/menu review

- upgrade with a drop-in whose `ExecStart=` runs another binary (see above);
  `tm_cfg` reads the drop-in's `-c`; backups include the drop-ins.
- `cfg_put`: every menu config write is atomic (temp file + rename, mode and
  owner kept); a full disk no longer leaves an empty config (also the certbot
  renewal file).
- `hs2://` links: a field with `"`, `\`, whitespace or a control character,
  or a `udp` other than true/false, is refused (it could inject JSON keys).
- helper units are not tunnels and their names are reserved; deleting a
  tunnel removes its alert timer, uninstall the stats / LB-park timers.
- `hs2 lb --ip` accepts `[IPv6]`, rejects zones and invalid addresses.

## rev53b — long-uptime review of the data plane (source only, no binary)

An independent review for bugs that show only after long uptime, many users
and churn. Fixed (each with a test that fails before the fix):

- **Receive memory amplification** (`engine/mconn.go`): the receive limits
  (`mRecvBuf`, `oooMax`) count payload bytes, but every waiting record held a
  whole 16 KB chunk. A reader that stops while the other end writes small
  messages (a backgrounded phone, a push channel, a destination that never
  reads an upload) pinned ~650 MB for 4 MB of 100 B records — any user could do
  it to the exit. A record up to 4 KB that has to wait is now copied out of its
  chunk (`compactRecv`); the no-backlog path is unchanged.
  `TestMConnSmallRecordsHeldMemory`: x164 before, x1 after.
- **Clock steps** (`engine/monoclock.go`): 37 hot-path atomics (`lastRecv`,
  dgtun mute/retire/drop marks, log limiters, stripe rebuild window...) held
  wall-clock `UnixNano`, so an NTP step moved them all (a step back of an hour
  kept a dgtun link muted for that hour; a step forward made every sub look
  idle). They now use `monoNano`/`monoTime` (monotonic).
  `TestNoWallClockNanosInEngine` guards against new ones.
- **A panic in a front's handshake killed the whole process**
  (`engine/panicguard.go`): the link listeners now run `AcceptFront` under
  `guardAccept` — that conn is closed and counted, with one log line (and stack)
  at most every 10 s. The tunnel's own code after the handshake is not covered
  on purpose (a recovered panic there could leave a shared lock held).
  `TestGuardAcceptPanicFlood`.
- **LB path view kept counters for every path ever seen** (`engine/lbview.go`).
  `TestLBViewForgetsGonePaths`: 201 entries before, 2 after.

## rev53c — operations review: upgrade, alerts, doctor (installer + binary)

Found on a lab copy of the production layout (two reverse `l3mtcp` tunnels on
one kharej host, LB 3 IPv4 + 1 IPv6 × 6 IPv4 + 1 IPv6, classic units with
drop-ins), reproduced on real systemd, fixed, tested:

- **critical, installer:** `install.sh upgrade` took the helper units
  `hs2-alert-<unit>.service` (and `hs2-stats.service`) for tunnels and
  rewrote the alert check as `hs2 run -c <the tunnel's config>` — with alerts
  on, every upgrade started a **second daemon on the live tunnel's config**.
  `tm_units` now skips oneshot units; the next upgrade heals a server an older
  one damaged (`heal_alert_units`); deleting a tunnel removes its alert timer,
  uninstall the stats / LB-park timers.
- **installer:** a drop-in whose `ExecStart=` runs another binary
  (`/usr/local/bin/hs2-r31 run …`) kept running the old build while the
  upgrade said "upgraded". It is pointed at `/usr/local/bin/hs2` (old file
  kept as `*.pre-upgrade`) and the upgrade checks `/proc/PID/exe`.
- **installer:** backups now include each tunnel's drop-ins (the HS2_ keys).
- **alerts:** a failed Telegram send (no route, proxy down, 429) no longer
  loses the message: it waits in the state (max 40 lines) and goes first,
  time-stamped, at the next check; texts over Telegram's 4096 limit are
  split; `/var/log/hs2-alert.log` rotates once at 2 MiB; one bad line in
  `alert.conf` no longer discards the lines after it (bot_token) and is
  reported once.
- **doctor --deep:** pinged only the first 8 LB paths; now up to 24 with every
  local and remote address first (IPv6 path, third kharej IP, second tunnel).

## rel52 — final release candidate: everything since rel40, with EE parity (binary)

One binary (build c4562e3dbbed, sha256 bea01c7c…; rel52b) = rel51 + ee51 + the rel52
commits. Since rel40, per area (details in the entries below and in
docs/reality-architecture.md §11):

- **Start and LB** — branch start42 start window (refill wait, start dial,
  on by default); rel47: refused dials before the peer listens no longer put
  the extra paths into the start back-off; rel51: a whole-peer outage no
  longer cools the non-primary paths. **rel52 shows it in the lab** (new r47
  `ir.sh` event `irdown`: every link reset and every dial refused for 3 s):
  20 s later rel52 is at 51/50/49 links on A/B/C, the tree before the fix at
  150/0/0 with B and C cooling (n=4 vs 4). A 3 s or 12 s SIGSTOP of the Iran
  process (`irstop`) drops no link (l3 links die only after 30 s): users see
  the freeze, nothing to rebuild, both arms equal (n=4+4 each).
- **Calls** — rel51: the stuck-call trigger also runs under
  `HS2_LB_CALL_LATE` alone. rel52: the same reading for the game flow
  (`HS2_MCONN_TRACE_GAME`, lab trace, off) found its subs all on the
  degraded path in 0 of 8 traced runs and no bad game run: not confirmed,
  no game trigger added.
- **Reality** — rel44 cold dialer, suite parity without the global lock,
  bounded target wait; rel45 no switch stall; rel47 fast fail of a plain tls
  dial to a pure-reality IP; rel48 direct `both`, rotate-sid, target suite;
  rel50 `both` downgrades only on real refusals, installer names; **ee51
  EncryptedExtensions parity** (uTLS v1.8.0 vendored as
  `hs2-src/third_party/utls`, `Config.ServerNameAck`: the EE acks the SNI
  when the target does, EE 36 = 36 B) and **post-Finished records replayed
  in the target's observed order**; ClientHello bytes unchanged.
- **Heavy upload after a link kill (u8)** — rel49 resend + last-sub grace,
  `HS2_MCONN_STRIPE_RESEND=1` default on both sides. rel52: light users in
  the 10 s after a 75 % kill are clearly worse (crowd p99 261 vs 174 ms
  without a kill, call late 45 % vs 5 %, n=8+8). New
  **`HS2_MCONN_STRIPE_REBUILD=1`** (off by default, Iran): after the pool
  falls under half of its recent peak, striped uploads use one stripe sub
  beside the primary for `HS2_MCONN_STRIPE_REBUILD_MS` (5000). Lab 2, n=6+6:
  crowd p99 265 -> 196 ms (p=0.03), call late 47 -> 7 % (p=0.03), Mbps not
  lower (956 vs 1072); engaged 6/6, 0/6 off. **rel52b confirmation round:
  not accepted, stays off.** 0 triggers in 48 no-kill runs (c0/c1/u1/s,
  n=6+6 each, no significant regression) and in an LB cut of one path
  (scenario c, n=10+10); but after the kill the gain did not replicate (lab 1
  n=12+12: crowd p99 221 -> 268 ms, p=0.06, call late 81 -> 47 %, p=0.07;
  lab 2 rerun n=6+6: no gain, crowd fails slightly up, p=0.04). The window
  is now per pool (per tunnel), not process-wide.
- **Scenario s** — rel52 vs before the call-stuck change, n=8+8: Mbps 1185 vs 1138, Mann-Whitney p=0.57 (the rel51 1176 vs 1314 at n=4 was noise).
- **Release tooling** — gofmt excludes the vendored upstream uTLS files
  (only `third_party/utls/hs2_*.go` is checked); `release_files_test.sh`
  runs that check when gofmt is on PATH.
- **Tests** — `TestRealityRSAKeyAndTicketPads` deterministic (rel51);
  `ports_menu_test.sh`: 105 looped real runs at load up to 45, 105 pass, not
  reproduced; new `TestMTraceGameBand`, `TestStripeRebuildWindow` (rel52b:
  also a second pool not narrowed, a one-path cut opens nothing).

Defaults that change for an **r31** deployment (reverse l3mtcp only; not
under `HS2_DEFAULTS=legacy`): `HS2_LB_JUDGE_DATA` (both), `HS2_LB_RESCUE`,
`HS2_LB_RESCUE_CALLFREE`, `HS2_LB_CALL_LATE`, `HS2_REFILL_WAIT_MS=300`,
`HS2_REFILL_WAIT_BYTES=1` (Iran), `HS2_START_DIAL` (kharej),
`HS2_MCONN_STRIPE_RESEND` (both); `hs2-menu` opens the new menu. How to get
the old behaviour back: README "Upgrading from r31 / r34".

Final checks (lab 3, clean checkout of a5316087): gofmt 0, `go vet ./...`
ok, `go test ./...` ok (rc=0), `-race` on the new/changed tests ok, sh
tests 23/23, multi_tunnel alone (no mtir/mtkh netns before, /tmp 5.1 GB free) rc=0, 57 PASS / 0 failures, no mtir/mtkh netns left after. Upgrade test (labs 2/3): r31 + an
r31-style reverse l3mtcp lb config, `install.sh upgrade` in place on both
sides: 8/8 links, config byte-identical, traffic 0 % loss, `hs2 doctor
--deep` 0 bad / 0 warnings; mixed versions (new kharej + r31 Iran and the
reverse) 120 s each, 0 % loss. docs/reality-architecture.md §11.11.

## rel51 — release candidate: everything since rel40 in one build (binary)

One binary (build c0c1afbb7de6, sha256 95379063…) that merges rel47, rel49
and rel50 (with rel44–rel46 and rel48 underneath). What changed since rel40,
per area (details in the per-release entries below and in
docs/reality-architecture.md §11):

- **Start and LB** — branch start42 start window (refill wait, start dial,
  on by default); rel47: refused dials before the peer listens no longer put
  the extra paths into the start back-off. **rel51: a whole-peer outage
  (every link gone, every path refusing) no longer cools the non-primary
  paths** — before, the rebuilt pool sat on the primary for a 60 s cooldown;
  the first dial that works again starts the other paths clean
  (`TestLBPeerOutageNoCool`, fails without the fix).
- **Calls** — **rel51: the stuck-call trigger (`HS2_LB_CALL_STUCK`) also runs
  under `HS2_LB_CALL_LATE` alone** (the production default; before it needed
  the off-by-default `HS2_LB_CALL_RESCUE`). Call late needs a call's subs on
  two paths; a call wholly on one degraded path had nothing to move it
  (`TestCallStuckUnderCallLateAlone`). Lab (mixed fronts, scenario c, B
  degraded at 15 s): see §11.10.
- **Reality** — rel44 cold dialer, suite parity without the global lock,
  bounded target wait; rel45 no 10 s stall on a both -> reality switch;
  rel47 a plain tls dial to a pure-reality IP fails fast; rel48 direct-mode
  `both` fixed, rotate-sid, target suite report; rel50 `both` downgrades
  only on real refusals, the classic installer writes the tls name on the
  listener, the legacy `"carrier": "reality"` warning in the menu. The
  Reality/tls throughput gap is not reproduced (§11.8).
- **Heavy upload after a link kill (u8)** — rel49: the failed write is kept
  and resent, last-sub grace (`HS2_MCONN_STRIPE_GRACE_MS`, 3 s);
  `HS2_MCONN_STRIPE_RESEND=1` default on both sides. rel51 check: c1 with
  resend on vs off, n=8+8 interleaved on one lab: median 1285 vs 1211
  Mbit/s, Mann-Whitney p=0.44 — no throughput cost.
- **Tests** — `TestRealityRSAKeyAndTicketPads` made deterministic (it read
  the target's ticket records with a fixed 300 ms window; under load the
  second record arrived later: 1 fail in 180 loaded runs before, 0 in 90
  after).

## ee51 — Reality: EncryptedExtensions parity (uTLS fork) and post-Finished order (binary)

- **EE 32 vs 36 B closed.** uTLS v1.8.0 vendored as
  `hs2-src/third_party/utls` (`replace` in go.mod) with one opt-in addition:
  `Config.ServerNameAck` acks the client's SNI in EncryptedExtensions (an
  empty `server_name`, as OpenSSL / Go 1.27 / www.microsoft.com /
  www.digikala.com send it). The mirror probe reads the target's EE record
  length and sets it only for targets that ack. Lab: every record of the
  authenticated handshake equals the target's for both targets (EE 36 = 36).
- **Post-Finished records replayed in the target's order.** A target that
  sends an h2 SETTINGS record after its tickets (www.digikala.com: 306 306
  57) got random sizes before (`[306 306 306]`); a pad now replays one whole
  observed sequence (`NSTSeqs`).
- **Dialer unchanged:** ClientHello bytes (deterministic fingerprints) and
  shapes (Chrome / Firefox, 300 each) identical before and after the fork.
- Tests: `TestRealityEEParity` (decrypts both EEs; upstream-uTLS, fork-ack,
  Go 1.27 and two `openssl s_server` targets), `TestRealityPostFinishedSequence`,
  `TestHS2EncryptedExtensionsServerNameAck` (fork module). Full suite on a
  clean bundle (lab 3): gofmt 0, `go vet ./...` ok, `go test ./...` ok
  except `TestMeteredConnBlockedTime` (engine, timing: 22 % under load
  average 120 from a neighbour job; 5/5 alone, and the whole engine package
  passes on a rerun at load 2), `-race` tlscarrier ok,
  the fork module's own suite ok. `docs/reality-architecture.md` §11.8.3.

## rel50 — Reality throughput gap measured out; `both` downgrades only on real refusals; installer names (binary)

- **Throughput gap (item 1): not a Reality cost.** Interleaved tls / reality /
  reality-with-AES-128-target, n = 7-8, direct and reverse l3mtcp, lossless
  and 1 % loss: reality = tls without loss; with loss -15 % (direct, p 0.10)
  and +12 % (reverse, p 0.14) — the heavy flow's own run-to-run noise. CPU
  profiles of the sender identical (AES-GCM 5.15 vs 5.33 %), same record
  sizes on the wire, same socket state. AES-128 targets are not consistently
  faster, so `HS2_REALITY_PREFER_AES128` stays off. `docs/reality-architecture.md` §11.8.
- **`both` front (item 4)**: only a reality refusal that the key or our
  listener explains (`ErrRealityCert`, the listener's clean close when its
  target is down/slow) counts toward the 10-minute tls cool-down. A reset
  or timeout during the reality handshake is now a path problem for the LB
  (no tls retry, no count, `path_fail`), so a censor that resets reality
  handshakes no longer pushes the path onto tls. A tls dial answered by the
  reality target ends the cool-down at once (`TestBothResetIsPathProblem`).
- **Installer (item 5)**: the classic direct listener (tcp, tun over tcp)
  writes `"sni"` on the listening side too, like the new menu
  (`listener_sni_test.sh`).
- **Legacy `"carrier": "reality"` (item 7)**: still warning-only; the warning
  now shows in the tunnel manager list and on upgrade, not only in `hs2
  doctor` (`legacy_reality_menu_test.sh`).
- Measured, nothing to fix: the direct reality cold restart (8.4-8.6 s to 90 %
  of the links vs tls 8.5-8.7 s, n = 3; rel48's 16.3 s was a one-off) and the
  judge's recovery after a bad IP heals (~15-19 s after the loss ends, the
  same for reality and tls, n = 3). Open with a plan: the 4-byte
  EncryptedExtensions gap against SNI-acking targets (patched uTLS, §11.8.3).

## rel48 — direct-mode `both` fixed; rotate-sid; target suite; mode matrix (binary)

- **Direct mode + `both` (fixed)**: a direct-mode listener has no `sni`
  (the installer writes it on the dialer only), so a `both` IP spliced every
  tls hello to the target: a dialer without the key never connected and the
  tls half of `both` never worked. With no `sni`, the names of this side's
  certificates are now our tls names (`TestTLSNamesFromCert`).
- **`hs2 reality rotate-sid -u U (--ip IP|--all)`**: a new short id first,
  the previous one still accepted until the next rotation, so no dial is
  refused (lab: a plain replace left a pinned dialer unable to rebuild after
  a link kill; two-step rebuilt in ~5 s).
- **Target suite**: `check-target`/`targets` report the TLS 1.3 suite the
  target picks and note AES-256 (suite parity follows it);
  `HS2_REALITY_PREFER_AES128=1` ranks AES-128 targets first (default off).
- Lab (docs/reality-architecture.md §11.7, harness `hs2-src/lab/s48`): mode
  matrix direct mtcp/l3mtcp/tls and reverse mtcp/tls x tls/reality/both with
  lb (3 IPs; the tls carrier has no lb), 74 ms, crowd + heavy, 75 % kill:
  every cell up, probes get the right certificate, doctor 0 problems, cold
  restart 2.3-2.6 s to the first link; direct l3mtcp x8 tls 726 / reality 551
  / reality with an AES-128 target 669 Mbit/s (n small, open). Scale: 6 IPs
  with per-IP keys and 2 targets, placement = weights 40/20/15/10/10/5.
- Release binary: build d3e00fa31aba, sha256 a8863c4c….

## rel47 — Reality with LB across IPs: mixed fronts, target trouble, start fixes (binary)

- **LB start (fixed)**: when the dialer starts ~1 s before the peer listens,
  the refused dials of that second no longer put the non-primary paths into
  the start back-off; the first successful dial clears it. Before, the redial
  burst could put all 100 start links on the primary (`[100,0,2]` at 6 s,
  call p99 6 s in the lab); a milder starved path was in ~1 of 5 runs of every
  front, tls included.
- **Plain tls dial to a pure reality IP (fixed)**: a dialer that does not
  know yet that an IP is pure reality (cold start before the announcement,
  or remembered fronts older than the switch) failed only after the 10 s
  auth deadline. Now a plain tls path checks the certificate against the
  peer's announced/remembered reality server names — at the handshake and,
  while it waits for the proof, every 100 ms — and fails at once
  (`ErrTargetAnswered`). A peer's own tls IP is never refused.
- Verified on the lab (docs/reality-architecture.md §11.6, harness
  `hs2-src/lab/r47`): mixed fronts (tls / reality / both) place links by
  weight, the judge scores them alike (no false lowering or cut), a reality
  path that degrades is cut/rescued like tls; a broken target on one reality
  IP stays on that IP (no judge input, cooled once at most, recovers);
  live front switches under load lose nothing; `hs2 lb` edits keep every
  IP's profile and key.
- Tests: `TestLBPeerNotUpYetNoBackoff`, `TestPlainTLSPeerTargetNames`,
  `TestLBEditsKeepMixedFronts`, `TestRealityCarrierTCPConn`.

## rel49 — the u8 upload freeze fixed: resend on by default (binary)

- **Root cause of the gap rel46 left** (survivors stalled ~4 s after the kill,
  for an app a 30 s hang): `writeLoop` released the record whose write
  FAILED on a dying stripe sub before `subDied` collected the sub's chunks,
  so no resend carried it; the receiver waited behind that one record (a
  22-byte or one-chunk hole, seen with a new per-second
  `HS2_MCONN_STRIPE_LOG` line of rx `next`/first ooo offset and per-sub tx
  offsets). Now kept and resent (`TestStripeFailedWriteResent`: hangs
  without the fix, passes with it).
- **Every sub died**: `HS2_MCONN_STRIPE_GRACE_MS` (default 3000, 0 = off,
  with the resend): both sides keep a connection whose last sub died; Iran
  opens a join sub on another link at once, the dead subs' chunks go first
  on it. Lab: joins 0.2-0.5 s after the kill. No wire change.
- **Lab u8** (8 uploads, 75 % of links killed; tls; labs 1+2): rel45
  5/8 runs frozen and 61/64 streams dead at the end; rel49 0 frozen, 0
  streams dead, every flow back within 0-6 s, no 30 s hang, no reset.
  c1/s: no regression beyond noise. See docs/reality-architecture.md
  §11.9.5 (incl. the single-upload numbers).
- multi_tunnel: 57/57 on lab 3 alone with the release binary (rel46's lab 1
  failures were host load; two concurrent runs collide on the fixed netns names).
- **`HS2_MCONN_STRIPE_RESEND=1` is now a production default on both sides**
  (`envdef`; `=0` turns it off). Binary build 5712ea8a30c7, sha256
  0595213f….

## rel46 — the u8 upload freeze: production has it, root cause found, fix kept off (binary)

- **Production (r31) has the bug**: lab u8 (8 uploads, 75 % of the links
  killed), tls, frozen = upload at 0 for the rest of the run: r31 4/6 (lab
  env), 2/5 (production env), rel40 3/6, rel45 11/13. Root cause: a
  striped connection (`HS2_MCONN_STRIPE=8`, a production default) is reset
  when one of its subs dies (`mconn: a sub died holding the only copy`):
  one copy per chunk, no retransmission. See
  docs/reality-architecture.md §11.9.
- **`HS2_MCONN_STRIPE_RESEND` (new, default off)**: the sender keeps written
  chunks 2 s / 16 MiB per connection and resends them on a live sub when a
  sub dies. Ends the resets in the lab but not the freeze (a connection
  whose subs all die still ends; some survivors stall on an unfilled gap —
  a hang, worse than a reset), so it stays off. Tests
  `TestStripeLinkDeathOldRuleResets` (reproduces the bug) and
  `TestStripeLinkDeathResend`.
- `HS2_MCONN_STRIPE_JUMP_MS` on Iran: no help (2/4 frozen).
- Lab harness `hs2-src/lab/s46` (several binaries per battery, r31 with the
  production env, the `r128` arm and a per-second `ss -ti` sampler).
- **Reality Mbps vs AES-128**: not the cipher. n=8 interleaved: c0 reality
  1607 = tls 1601; c1 reality 1169 vs tls 1312 (p 0.063), r128 1251 in
  between (n.s.), same cwnd/rtt. Target picker unchanged.
- Binary: build f2ec4e506acd, behaviour = rel45.


## rel45 — no stall when an IP switches both -> reality; reality vs tls measured (binary)

- **Switch grace (listener)**: an IP switched live from tls or `both` to pure
  `reality` still serves our own tls name for 30 s
  (`HS2_REALITY_SWITCH_GRACE_S`, 0 = off), so a tls dial already in flight
  is served instead of spliced to the target, where it waited out the
  dialer's 10 s auth deadline. Not at start, not for reality -> reality.
- **Fail fast (dialer)**: the tls leg of a `both` path fails at the handshake
  (`ErrTargetAnswered`) when the certificate is for one of the profile's
  `server_names` and not for our SNI, instead of waiting 10 s for a proof.
- `HS2_PPROF_LOCKS=N` (with `HS2_PPROF`): mutex and block profiles.
- Lab (docs/reality-architecture.md §11.5, harness `hs2-src/lab/s45`): the
  reality ping p99 penalty of rel44 was noise (n=13+13: 132.5 vs 138.9 ms,
  p 0.56; CPU/mutex/block profiles equal); c1 Mbps is 11 % lower with reality
  (p 0.009, open: cipher suite parity suspected); a 70 ms target does not
  change pool setup; u8 Mbps now measurable (iperf3 control off the links),
  which exposed a heavy-upload freeze after a 75 % link kill (both fronts,
  open).
- Release binary: build 7e2a6198222c, sha256 e31a18af….

## rel44 — Reality cold dialer, parity without a lock, bounded target wait (binary + menu)

- **Cold dialer fixed**: the dialing side remembers the announced reality
  profiles (public key, short ids, server names per IP) in
  `<config>.peer-fronts.json` (atomic, 0600) and dials them as `both` at the
  next start until the announcement arrives, so a restart no longer leaves a
  tunnel whose IPs are all pure `reality` down. New `hs2 reality pin-code`
  (listener) / `hs2 reality pin` (dialer), printed by `hs2 reality front`;
  menu screen 4 `p) Pin code`; `front … reality` refuses the last tls-capable
  IP without proof the dialer has the key (`--dialer-has-key`); doctor check
  `reality cold start`.
- **Parity suite per connection**: the sec41 linkname swap of uTLS's global
  TLS 1.3 preference list under a lock (serialised handshakes; a data race
  with uTLS clients) is replaced by a per-connection restriction of the
  parsed client hello.
- **Target wait bounded to 3 s** on authenticated links (`target_slow`,
  `target_hello_late`), instead of up to 5 + 10 s inside the dialer's single
  10 s deadline ("reading server proof: i/o timeout").
- Merged: sec41 (Reality security review), target43 (side-aware targets),
  start42 (start window). See docs/reality-architecture.md §11.

## After rel40 — branch start42: the start window (binary)

- **`HS2_START_DIAL` (kharej, on by default)**: the first 4 s of the exit's pool
  (and of a pool back after an outage, at most once a minute) dial through a
  start gate, 32 at a time, 10-35 ms apart, jittered; steady-state redials
  keep the ordinary gate.
- **`HS2_REFILL_WAIT_MS=300` + `HS2_REFILL_WAIT_BYTES=1` (Iran, on by default)**:
  the start hold's bound (v3 of §8.9) and a start episode that stays on at
  least 5 s even when the pool is complete early.
- Lab 1 (n=8+8, interleaved): crowd p99 of the first 5 s 1419 -> 157 ms, whole
  crowd p95 125 -> 116, ping p99 mean 134 -> 136 (p 0.60), call and heavy
  equal (§8.13). Why v3 alone cost ping: its early leavers stayed on the first
  links all run (busiest link 90 vs 72).
- Lab diagnostic `HS2_LINK_SPREAD_LOG=1` (edge, off, not in the table).

---

## Release rel40 — Reality front, the new menu, LB rescue (binary + installer)

Summary of everything since r34 (branches int39 + reality40 + reality41), one binary for
both servers. An r31 / r34 config runs unchanged; see README "Upgrading from
r31 / r34" for the exact default changes and how to restore the old behaviour.

- **Reality front, per IP** (Reality phases 1–5): each listening IP has a front
  `tls` (the default — what every existing config keeps), `reality` or `both`
  (reality with a TLS fallback and cool-down on the dialing side). `hs2 reality
  front | rotate-key | targets | check-target | status`, announced to the peer
  in the lb list; fingerprint and performance checks in
  `docs/reality-architecture.md` §1–9.
- **The new menu is the default** (M1–M7): bare `hs2-menu` opens it — dashboard,
  tunnels (wizard, connection code, edit, renew, clone), LB & IPs, security &
  camouflage, performance, monitoring & alerts, doctor, maintenance, settings —
  with safe apply (trial, confirm in place, automatic rollback). The classic
  menu stays one key away: `c`, `hs2-menu --classic`, `HS2_MENU=classic`,
  `default_menu=classic`.
- **Doctor and alerts screens**: `hs2 doctor [--deep] [--fix]` (network, CPU,
  tunnel, reality, panel; safe fixes with rollback) and `hs2 alert` (optional
  Telegram), new menu → 6 / 7.
- **LB rescue, on by default at the edge** (`HS2_LB_RESCUE`): a light user whose
  only sub sits on a lowered / cut path gets a copy on a good path at once; with
  **`HS2_LB_RESCUE_CALLFREE`** (also on at the edge) those copies stay off a
  call's links (§8.6–8.9).
- **`HS2_LB_CALL_LATE`, on by default at the edge** (with the rescue): a call
  whose copy on one path runs >= 100 ms and twice behind its copy on another
  (socket srtt, 1 s) gets a fresh sub off that path and no more copies on the
  late one, which is then given up: call p99 after a path degrades -29 % (c)
  and -69 % (k), never fired on equal / steady paths (§8.12). With it, a
  per-sub line in the `HS2_MCONN_TRACE` trace (TCP_INFO per sub).
- **`HS2_LB_JUDGE_DATA`, on by default on both sides**: the LB judge counts only
  dials that failed before TCP connected, so a front's handshake failure no
  longer lowers an equal path (§8.5).
- **Off by default, for the operator to decide** (measured, not adopted):
  `HS2_LB_CALL_RESCUE` with its stuck-call trigger `HS2_LB_CALL_STUCK` (§8.10–
  8.11), `HS2_LB_RESCUE_RAW` (§8.8–8.9), `HS2_REFILL_WAIT_MS` /
  `HS2_REFILL_WAIT_BYTES` (§8.7, §8.10).
- **M7 checklist**: the settings-persistence check in `m7drive.py` now reads
  the screen, toggles only what differs and verifies menu.conf and the next
  start, in both directions; re-run on the lab.

Upgrade test (labs 2/3): r31 binary + an r31-style reverse `l3mtcp` config with
lb, upgraded in place with `install.sh upgrade` on both sides: tunnel back,
8/8 links, traffic both ways, config byte-identical, menu opens, `hs2 doctor
--deep` 0 bad / 0 warnings on both. Mixed versions (new kharej + old Iran and
old kharej + new Iran): 130 s of traffic each, 0 % loss.

## Menu redesign M7 — lab test, fixes, the new menu becomes the default (binary + installer)

Full pty-driven lab test on two real servers (labs 2/3) with real tunnels;
checklist with PASS/FAIL per item: `docs/menu-m7-checklist.md`, driver
`install/tests/m7/m7drive.py`. Bugs found and fixed:

- **Fresh install from the new menu** went to the classic setup screens (no
  binary → no wizard). 2) Tunnels now offers to install the binary first, then
  runs the wizard.
- **Join with a code** failed at the last step when the code's tunnel name
  (both sides default to `hs2`) was already used here; the joined tunnel now
  gets a free name with a note (`--name` / `--update` still choose).
- **Non-interactive join on the listening side without a domain** got cert
  `auto` = `/etc/letsencrypt/live//fullchain.pem` and failed `hs2 check`; it is
  self-signed now, as the wizard's default.
- **Menu changes rolled back by themselves**: after an apply the menu only
  printed `hs2 apply --confirm`; it now asks "Keep the change?" in place and
  waits for the health check when y comes early.
- **Colour off** still coloured separators and notes (`hr`, `info`, `warn`);
  now plain, and `NO_COLOR` is honoured.
- **Uninstall / remove tunnel** left the wizard's self-signed key in
  `/etc/hs2/certs`; removed with its tunnel.
- **M4 `hs2 lb add-ip`** of the only IP on a dialer without `bind_local_ip`
  failed the weight check (the default path joined with 50 %): the listed IP
  the default route leaves from becomes the primary (`bind_local_ip`); a bound
  primary left unlisted scales the listed weights to fit; removing the bound
  primary moves it to a listed IP. Verified live: lab dialer, add-ip via the
  menu, health gate, confirm, 8/8 links.
- Stale wizard / pre-flight text "Reality — coming (M5)" now points to
  Security & camouflage → Front of an IP; the double default "[y/N] [n]" is
  gone.

**Readiness:** every checklist item passes after the fixes, so a bare
`hs2-menu` opens the new menu; the classic one stays one key (`c`) or one flag
(`--classic`, `HS2_MENU=classic`, `default_menu=classic`) away. Upgrade from
the rel34 release (old binary + old menu, tunnels running) with the new
installer: both tunnels restarted and verified, bare `hs2-menu` then opens the
new menu, `c` / `--classic` the classic one.

Merged with reality38 (LB rescue call-free, start window v3; docs §8.8/§8.9).

---

## Menu redesign M5 — Security & camouflage (= Reality phase 5; binary + installer)

- `hs2 reality front -u U --ip IP tls|reality|both [--target host:443]
  [--sni name] [--new-key] [--force-target] [--skip-check]`: the per-IP front
  in this side's lb list. Only on the LISTENING server (the dialing server is
  told the command to run on the other side); the dialer learns kind, server
  names, public key and short ids from the announcement, so nothing runs on
  the other server. Preview diff (private keys shown as `(hidden)`; per-IP
  fronts are not "bilateral"), backup, safe apply with a **reload** (SIGHUP)
  instead of a restart: established links keep running; the confirm timer and
  the automatic rollback reload too. A new key pair + short id is generated
  when the IP has none; `reality -> both` keeps key and target; `tls` keeps
  key, target and short ids in a `{"kind":"tls",...}` block (served and
  announced exactly like no block), so switching back to reality / both
  reuses the same key; a block without a key is removed. The target is checked first (a rejected target is refused unless
  `--force-target`).
- A wildcard listener (`addr 0.0.0.0:P` / `[::]:P` / `:P`, the default of
  `hs2 tunnel create`) announces its explicit lb list (`lbWildcardAdvert`:
  the weights as written when they sum to 100, else scaled to 100), so its
  per-IP fronts reach the dialer; before, its announcement was empty.
- Pre-flight refusals with the reason: an lb list the announcement cannot be
  built from (the dialer would never learn the front); `both` without any sni (no domain)
  would send every tls dialer to the cover target. A warning for pure
  `reality` unless the IP is already `both` (a dialer that has not heard the
  key, e.g. right after its restart, is refused): order tls -> both ->
  reality.
- `hs2 reality check-target host[:443] [sni] [--json]`: TLS 1.3, X25519
  offered alone, ALPN h2, certificate covers the name, chain size, handshake
  time (BAD > 1.5 s), multi-tenant CDN by name, Cloudflare address ranges, a
  shared certificate (> 60 names) or the CDN's response headers (cf-ray,
  x-amz-cf-id, x-served-by cache-, akamai, x-azure-ref, gws ...). Verdict
  ACCEPT / REJECT with reasons; exit 1 on REJECT.
- `hs2 reality targets [--add h,h] [--only] [--json]`: the target picker,
  built-in candidates plus yours, checked in parallel, accepted first by
  latency.
- `hs2 reality rotate-key -u U (--ip IP | --all)`, `hs2 reality keygen`.
- `hs2 reality status -u U [--json]`: per IP the configured and the in-use
  front with reality/tls/cover/rejected link counters (listening side), per
  path kind and "both" state (now reality / tls cool-down, ok/fail,
  fallbacks) on the dialing side, reality stats and the front events of the
  last 24 h.
- Doctor category `front` (`hs2 doctor --deep --only front`, menu 7 -> 7):
  reality target reachable, key / short id mismatch (no authenticated link
  but >= 5 auth failures), splices that could not reach the target, a "both"
  path that fell back to tls or has a high reality failure rate.
- Alert kinds `fallback` (a "both" path in its tls cool-down, or >= 2 new
  fallbacks between two checks) and `rtarget` (a reality target unreachable
  two checks in a row), fa/en texts, switchable like the others.
- Screen 4 of `hs2-menu --new`: live front state, front of an IP, target
  picker, one-target check, key rotation, doctor fronts, certificate;
  advanced: server name, one-IP rotation, keygen, config editor. Old binary:
  the classic items.
- Fixes found on the way: the M4 lb editors (`hs2 lb add-ip|remove-ip|
  weights|park|unpark`) dropped every per-IP `front.private_key` (the lb list
  round-trips through a type that never marshals it); an lb reload on a
  wildcard listener (`addr 0.0.0.0:P`) closed the `::` listener and left the
  tunnel without any listener.
- M6 leftovers: `hs2 tunnel delete` backs up and removes the
  `<unit>.service.d` drop-in directory (the restore line copies it back);
  `hs2 config history` marks only the newest version equal to the live config
  as current; `multi_tunnel_test.py` SIGKILLs its fakesd supervisors.
- Tests: Go (front edit, private-key preservation, target check against local
  TLS servers incl. one with Cloudflare headers, doctor fronts, alert rules,
  reload-mode apply + rollback, fa status render, delete drop-ins, history
  current, pre-flight, diff secrecy); pty `security_menu_test.sh` (17 checks).
- Lab demo (labs 2/3, direct mtcp, kharej listens): tls -> reality -> both
  under a 200 MB download plus a request every 0.3 s: 349/349 OK and the
  download complete; after a dialer restart 7 new links authenticated over
  reality; a client without the key (openssl / curl, sni github.com) got
  github.com's real certificate and HTTP answer; an unconfirmed apply rolled
  back by reload after 30 s; check-target ACCEPT github.com, REJECT
  www.cloudflare.com / cdnjs.cloudflare.com (name) and www.npmjs.com (address
  range).
- Leftovers (branch menu38): the wildcard announcement above; the kept key
  on `tls`; `hs2 tunnel create` / `join` write the domain as "sni" on the
  listening side too (before: the dialer only), so "both" works on a created
  tunnel without setting an sni by hand; tests `TestLBWildcardAdvert` (as-is / scaled weights, fronts
  carried, the dialer's paths from it, a kept key not announced),
  `TestSyncListenersKeepsWildcard` (an lb reload never closes the `::` /
  `0.0.0.0` listener of a wildcard addr), `TestFrontSpecEdit` (tls keeps the
  key, back to reality reuses it), `TestFrontPreflight`. Lab demo (labs 2/3,
  tunnel made by `hs2 tunnel create` with the default `addr 0.0.0.0:8443`,
  lb with the primary IP): the dialer took the front from the announcement;
  tls -> reality -> tls -> reality -> both (one key throughout, the same
  md5 before and after) under a request every 0.3 s and a 200 MB download:
  519/519 + 462/462 requests OK (a dialer restart fell inside the second
  run), the download complete; after that restart 7 links authenticated over
  reality (listener counters 7/1/0/0; the 1 tls link was dialed before the
  announcement arrived and "both" accepted it); a
  client without the key (sni github.com) got github.com's certificate.
  Second run with the final binary (the listener now gets "sni" from
  create): tls -> reality -> both with no sni set by hand, 281/282 requests
  OK (the one failure falls in a deliberate dialer restart, not in a front
  switch), 7 reality links after the restart.
- `alerts_doctor_menu_test.sh`: a check no longer runs under pipefail (`echo
  "$out" | grep -q` could fail by SIGPIPE on a busy host; seen once).

## Menu redesign M6 — Monitoring & alerts, Server doctor with safe fixes (binary + installer)

- New `hs2 alert conf show|set|token`, `hs2 alert rule NAME on|off`,
  `hs2 alert history`, `hs2 alert summary-now`, `hs2 alert telegram-check`
  (`cmd/hs2/alert_conf.go`). `alert.conf` writes are validated (chat id,
  proxy URL, numbers, lists, HH:MM) and atomic with mode 600; the bot token is
  accepted only on stdin and is never printed. New keys `disable=` (alert kinds
  never sent), `quiet_hours=` (only critical alerts and the "resolved" of a
  critical one reach Telegram; may cross midnight; everything is still logged
  with `[quiet hours: not sent]`), `summary_items=` (daily summary content).
  The reachability check tells an Iran server without Telegram access to set a
  proxy. `summary-now` sends the day so far without resetting it.
- New `hs2 doctor --only CAT`, `--fixes`, `--report FILE`, `--fix N`
  (`cmd/hs2/doctor_fix.go`): findings are grouped in network / cpu / tunnel /
  reality / panel; new checks for `netdev_budget` against `time_squeeze`, the
  24 h sampler and per-tunnel alert timers, and tunnel certificate expiry.
  Numbered safe fixes reuse M4 mechanisms (CPUAffinity safe apply with
  auto-rollback, measured netdev_budget, timed LB park), the stats timer, a
  `certbot renew --dry-run`; x-ui / xray / Reality / panel findings never get
  a fix. A fix checks its tunnel still exists, refuses a run older than 30 min
  and explains non-root.
- Fix: the doctor's "restarts / panics" lines counted every `hs2*` unit
  (`hs2-coreguard`, `hs2-alert-*`, …) as a tunnel; only units whose
  `ExecStart` is `hs2 run -c /etc/hs2/<name>.json` are counted now.
- Doctor: a stale status file that belongs to no config in `/etc/hs2` (a deleted
  tunnel, a test run) is INFO with the `rm` command, not "BAD — the tunnel is
  probably stopped".
- Installer: screens 6 and 7 of the new menu (`nm_alerts`, `nm_doctor`); an
  older binary (no `hs2 alert --supported` / `hs2 doctor --supported`) gets the
  classic screens. The `max-throughput` preset is labelled "experimental, not
  recommended" in the menu and `hs2 perf presets` (not A/B-proven).
- Tests: `cmd/hs2/m6_test.go` (11 tests: alert.conf validation and atomic
  set/token/rule, quiet hours across midnight, disable + summary items,
  history filter, Telegram check direct/proxy/blocked, fix mapping incl. never
  x-ui/xray, apply-fix safety (no run, stale run, gone unit), coreguard
  filtering, monitoring + budget checks, leftover status files); `install/tests/alerts_doctor_menu_test.sh`
  (26 pty checks); `new_menu_test.sh` keeps testing the classic fallbacks.

---

## Menu redesign M4 — Load balancing & IPs, Speed & performance (binary + installer)

- New `hs2 lb show|history|suggest|add-ip|remove-ip|weights|mode|park|unpark|parks`
  (`cmd/hs2/lbmenu.go`): live path table (share/eff, links, users, Mbit,
  loss, RTT, auto factor + reason, state, cuts in 24 h from the journal),
  local-IP suggestion by ping (loss, then RTT; current primary kept within
  1 ms / 1 %), add/remove IP with interface check and per-IP SNI/cert
  (front: tls; Reality in M5), weights (sum 100) / equal split / mode, and
  "park an IP for N hours" restored by `hs2-lb-park.timer` (persistent
  unit, survives reboot; restore via safe apply, confirmed on the next run,
  gives up after 3 rollbacks). Every change: diff + safe apply + bilateral
  `join --update` code.
- New `hs2 perf show|presets|preset|set|unset|nic|cores|kernel|speedtest`
  (`cmd/hs2/perf.go`): presets production / low-cpu / max-throughput /
  min-latency and per-key toggles as one `50-hs2-perf.conf` drop-in,
  effective-key view (set / default / off), single-queue NIC + rx-IRQ core
  detection with a `CPUAffinity` drop-in for hs2 units only, kernel view +
  `netdev_budget=600` measured before/after with automatic revert, iperf3
  speed test through a local user port only.
- Safe apply (`menu_apply.go`): `applyReq.NewDropins` — drop-in changes are
  diffed, snapshotted and rolled back like the config.
- Fix (`menu_validate.go`): `isEdge` was "mode dial XOR reverse", so on the
  kharej side of a reverse tunnel the validator took the Iran lb list as its
  own (and counted user ports on the wrong side): `join --update` carrying an
  lb section failed with "… is not an address of this server". Iran is mode
  `dial` in both directions.
- `hs2 tunnel join`: flags after the code are parsed too
  (`join --update CODE --yes` used to ignore `--yes`).
- M3 leftovers: `hs2 stats timer on|off|status` writes/enables its own
  `hs2-stats.{service,timer}` (wizard and menu setting use it; older
  binaries keep the installer's units); `tm_edit_test.sh` uses its own fake
  hs2 instead of the host's `/usr/local/bin/hs2`; `multi_tunnel_test.py`
  scenario 2 waits up to 10 s for tunnel 1's PID on both sides and reports
  a missing unit as a FAIL instead of crashing with `KeyError: 'hs2'`.
- `install.sh`: new-menu items 3 and 5 are the new screens (simple/advanced,
  fa/en, admin lock around every change); old binaries fall back to the
  classic screens.
- Tests: `cmd/hs2/lbmenu_test.go`, `perf_test.go` (path table rendering and
  alignment, journal event parsing, IP ranking, weights validation,
  park/unpark weights and timer logic, own-list sides, reverse validator,
  interleaved flags, presets → env per side, effective keys incl. legacy, NIC
  fixtures, cores guard, budget verdict, iperf parsing, drop-in apply +
  rollback, stats timer units), `install/tests/lb_perf_menu_test.sh`
  (22 pty checks); `new_menu_test.sh`: item 5 is now the Speed screen,
  item 4 still the M5 preview (25 checks).

## Menu redesign M3 — Tunnels: wizard, connection code, edit, renew, clone (binary + installer)

- New `hs2 tunnel list|create|join|code|clone|edit|renew|start|stop|restart|logs|delete|preflight`
  (`cmd/hs2/tunnel.go`, `tunnel_cmd.go`, `tunnel_code.go`). Creation wizard
  (interactive, `--advanced`, or `--from-json … --yes`) with pre-flight
  checks; nothing is written before the summary is accepted, and a unit that
  does not stay up is removed again.
- Connection code `hs2://v2.<base64url JSON>` (versioned, secrets listed and
  masked on display, short check line); `join` reads it (and classic v1
  links), `join --update` updates an existing tunnel through safe apply.
- Edit per section via semantic diff + `hs2 apply --confirm-within`, with
  the other server's required change printed as a `join --update` code; SSH
  over the tunnel is detected (user port / route via the tunnel interface).
- `renew` (backup, `--upgrade-from` binary with automatic restore, rebuild,
  `--pick-ip` best local IP by ping) and
  `clone --to-code`.
- `install.sh`: new-menu item 2 is the Tunnels screen (list, wizard, join,
  per-tunnel actions under the admin lock); old binaries fall back to the
  classic screens. The 24 h sampler timer stays off by default and is offered
  in the wizard's last step.
- Tests: `cmd/hs2/tunnel_test.go` (code round trip / versioning / v1 links /
  secret masking, join mirrors creator for direct, reverse, IPv6 and dgtun,
  defaults and rejections, multi-tunnel names/ifaces/subnets/ports,
  pre-flight fixtures, non-interactive create incl. dry run and removal of
  an unhealthy unit, wizard abort and answers, SSH guard, best local IP),
  `install/tests/tunnels_menu_test.sh` (15 pty checks).
- `install/tests/lb_menu_test.sh`: the three "live table" checks were stale
  since 9a6a42ac (the live view gained AUTO/NEW columns and "(…, the config)"
  in its last-load line); expectations updated, 38/38 pass with go on PATH.

## Menu redesign M2 — new main menu + live dashboard (binary + installer)

- New `hs2 dashboard` (live every 3 s / `--once` / `--header` / `--paths`,
  `--lang fa|en`, width-aware down to < 60 columns, ASCII + English when the
  locale is not UTF-8, one plain frame when not a tty). Read-only.
- New `hs2 stats sample|show`: hourly aggregates for 7 days in
  `/var/lib/hs2/stats-<unit>.json`; sampled by `hs2 alert` and by an optional
  `hs2-stats.timer` (menu settings).
- `doctor_deep`: the path pings are factored into `pingPaths` (shared with the
  dashboard's path check); findings unchanged.
- `install.sh`: the new menu (`hs2-menu --new`, `HS2_MENU=new`, or
  `default_menu=new` in `/etc/hs2/menu.conf`); the classic menu stays the
  default and gains `n) New menu`. Menu settings (language, simple/advanced,
  colour, default menu, history timer) persist in `/etc/hs2/menu.conf`. Edit
  screens run under the admin lock. Maintenance adds change history and
  rollback of the last change. Items 2–5 open the classic screens for now.
- Tests: `cmd/hs2/dashboard_test.go` (rendering at 50/60/80/120 columns,
  fa/en, ASCII, missing/stale status, no tunnels; stats aggregation and
  retention), `install/tests/new_menu_test.sh` (24 pty checks: navigation,
  settings persistence, lock, maintenance, menu selection, locale fallback).

## Menu redesign M1 — safe-change infrastructure (binary + small installer hook)

- New `hs2 config history|show|diff|validate|lock|unlock|lock-status`,
  `hs2 apply` (trial with confirm timer, health gate, automatic rollback that
  survives an SSH drop via `systemd-run` + an expiry timer) and
  `hs2 rollback`. Versioned per-unit history (last 50, with drop-in
  snapshots), semantic diff with **bilateral** marking, host cross-checks,
  admin lock with stale detection, audit log `/var/log/hs2-changes.log`.
- `install.sh`: the tunnel manager's **Edit config** saves through
  `hs2 apply` (Enter = confirm, `n` = roll back, no answer in 5 min =
  automatic rollback) when the binary answers `hs2 apply --supported`;
  older binaries keep the classic path. New test
  `install/tests/tm_safe_apply_test.sh`.
- No new menu screens yet (M2+); the old menu is unchanged otherwise.

---

## Track A — installer robustness (install.sh; no binary change)

### A1 — binary verification & upgrade safety
- Fail-closed sha256 verification of the downloaded binary (re-fetch past the
  CDN once, then refuse on mismatch or a missing hash).
- The installer copies itself to `hs2-menu` and publishes `install.sh.sha256`.
- **U1:** when a good binary is already installed, setting up another tunnel
  defaults to **keeping** it (the shared binary is not silently swapped under
  running tunnels); only Upgrade replaces it deliberately.
- **U2:** per-tunnel upgrade with an explicit restart-list confirmation.

### A2 — input validation & transactional config writes
- **#3/#4:** validate MTU and the ipx protocol number.
- **#16/#17:** base-10 parsing (no accidental octal) and JSON-escaping of the
  domain/panel values that land in the config and the `hs2://` link.
- **#10:** read role keys through the binary instead of a fragile grep.
- **U5:** optional subnet prompt. **U6:** warn on bilateral-field edits and
  offer a relink.
- **A2f:** every config write is transactional — written to a temp file,
  validated with `hs2 check`, then atomically moved into place.

### A3 — real liveness & a correct CLI surface
- **U11:** the tunnel list shows real peer reachability (a cached ping), not
  just "service running".
- **#5/U12:** `verify_tunnel` multi-pings and **warns rather than bailing** when
  a tunnel is slow to come up.
- **#9:** the CLI dispatch handles `status`/`uninstall`/`version`/`cleanup` and
  rejects unknown commands cleanly.
- **U13:** a "peer gone" label when a previously-reachable peer stops answering.

### A4 — crash-safety of restore/upgrade/tuning
- **#7:** restore never crashes on an old backup. **#8:** restore guards the
  binary clean-break. **#22:** re-setup stashes and restores the live config,
  scoped to its own run.
- **#6:** tuning detects an old binary (capture-then-grep, so pipefail cannot
  mis-fire the guard). **#18:** manual tuning rolls back if the tunnel does not
  come up.
- **U14:** "uninstall all" needs a typed strong confirmation.
- **U4 (rekey + rebuild the link): NOT implemented — deferred.** An earlier note
  wrongly recorded this as done; a real-server review confirmed there is no rekey
  code. It is blocked on the config not persisting the listener's public endpoint
  + domain, without which a regenerated link could be wrong and break a live
  tunnel. Left for a dedicated change.

### A5 — robustness polish
- **#12:** literal (`grep -Fx`) matching for IPs/subnets (dots are not
  wildcards). **#15:** the systemd `StartLimit` directive adapts to systemd
  older than 230. **#19:** the live monitor cannot busy-loop with no tty.
- **#20:** backup pruning is safe on bash < 4.4 (empty-array guard).
- **#21:** DNS resolution reads only answer IPs, not the resolver's address.
- **U7:** link-pool (min/max/per-link) editing in the Tuning screen.
- **U8:** the dgtun fixed-1280 MTU is explained. **U9:** user-port default IP is
  sane on multi-IP vs single-IP servers.

---

## Track B — binary features (Go; binary rebuilt)

### B1 — build stamp in `hs2 version` (U3) + a build-safety fix
- `hs2 version` now prints a build stamp (`… [build <rev> <date>]`) read from
  the binary's embedded VCS metadata, and the installer menu shows the installed
  binary's stamp — so an operator can confirm **both ends run the same build**.
  Metadata only: a stamped binary stays wire-compatible with an unstamped peer.
- **Build-safety fix found while doing this:** `BUILD.md` did not pin
  `CGO_ENABLED=0`, yet the published binary is static. A naive rebuild on a host
  with a C compiler would silently produce a dynamically-linked binary that
  fails on old-glibc/musl servers (`GLIBC_2.xx not found`). `BUILD.md` now pins
  `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`.

### B3 — `hs2 doctor`, an on-box health check
- `hs2 doctor -c <config>` prints a read-only PASS/WARN/FAIL checklist and exits
  non-zero only on a hard failure (same contract as `hs2 check`): config
  validity, whether the tunnel is running, **endpoint reachability** (the common
  "edge can't reach exit" failure), certificate expiry, the TUN device's real
  state, kernel-tuning-vs-`/proc/sys`, and a clock reminder (link auth is
  minute-bound). Also wired into the tunnel manager as a **Diagnose** action.
  The review caught and fixed a real issue: the tuning check must not run
  `modprobe`, so it is now fully read-only and needs no root.

### B4 — fix a stale obfuscation comment (#23)
- `encap/obfs.go`'s prose described an old 12-bit/4-bit split of the ICMP echo
  sequence; the implementation (and its tests) are 8/8. Corrected the comments.
  Documentation only — proven byte-identical build, so **no binary respin**.

### B2 — multiple IPs per tunnel (U10)
- Proposed as a larger design phase; **deferred at the maintainer's request**
  (not implemented).

---

## Phase C — active-probe resistance (TLS front door + cover page)

**From a field probe report:** the tunnel port answered differently depending on
the probe — a completed TLS handshake got an nginx-looking web page, but a plain
HTTP request (or random bytes) was closed silently with zero bytes. A real
HTTPS server does not behave that way, and the page claimed `Server: nginx`
while the TLS stack is Go's — a contradiction a prober can detect.

The fix makes every unauthenticated probe see **one coherent identity: an
ordinary Go HTTPS service** (the kind Caddy/Traefik are), instead of a
contradictory hybrid.

- A plain-HTTP request on the TLS port now gets the **exact** response Go's own
  HTTPS server sends (`HTTP/1.0 400 … Client sent an HTTP request to an HTTPS
  server.`), byte-pinned to the standard library by a test — not a silent close.
- Only the five request starts Go itself recognizes are answered that way;
  everything else (a real ClientHello, or garbage) flows into the TLS layer
  unchanged and behaves exactly like a real Go TLS server.
- The cover page is no longer the nginx default welcome page (itself a honeypot
  signature). It is a plain, self-contained generic site (fictional brand, no
  external requests, served via `http.ServeContent` with real static-server
  headers) and the `Server: nginx` header is gone — the Go TLS stack no longer
  contradicts a claimed server it cannot imitate.
- Listener-side only, no wire change → **drop-in and backward compatible**: an
  old-binary client still authenticates unchanged.

**Verified end-to-end** by running the new binary as a live TLS listener and
replaying every probe from the report, comparing against the previous binary:

| probe | before | after |
|-------|--------|-------|
| plain HTTP `GET /` (no TLS) | 0 bytes, silent close | Go-native `400`, 76 bytes |
| TLS + `GET /` | `Server: nginx`, "Welcome to nginx!" | no `Server` header, generic page |
| TLS, no/ wrong SNI | full handshake, cert | unchanged (consistent) |
| random bytes / unknown HTTP method | close | close (matches a real Go server) |

**Deliberately not done:** the TLS-stack fingerprint stays Go's — TLS must be
terminated in Go for the channel-bound auth, and "a Go HTTPS server" is an
ordinary, common identity once nothing contradicts it; closing that gap fully
would need a Reality-style redesign or a real-nginx dependency, neither of which
fits the single-static-binary design. A per-connection probe-flood cap was also
deferred (the half-open hold is unchanged from before, so nothing got cheaper
to flood).

---

## Post-review fixes (independent real-server review)

An independent review ran the new binary on a live server. Most claims held;
these real issues were found and fixed:

- **🔴 Regression fixed — TCP_INFO went dark on the TLS-server side.** Phase C's
  `prefixConn` stays wrapped around the connection for its whole life, and
  `Carrier.TCPConn()` unwrapped only one level (`tls.Conn` → expected
  `*net.TCPConn`), so it hit the `prefixConn` and returned nil — silently zeroing
  kernel loss/rwnd that the autopilot uses (loss-based soft-degrade never fired;
  slow-receiver vs capped-path could not be told apart) on the server side
  (Iran in reverse, Kharej in direct). Fix: `prefixConn` now exposes `NetConn()`
  and `TCPConn()` unwraps the whole chain; a regression test covers it.
- **🟡 Phase C close is now a clean FIN.** The plain-HTTP 400 path left the rest
  of the request unread, so the socket closed with an RST; a real Go server FINs.
  It now drains the (buffered, tiny) request before closing.
- **🟠 Invisible prompts fixed.** Two prompts (`install_binary` keep/update and
  `upgrade` "Proceed?") were written with `read -rp "…" 2>/dev/null`, and the
  `2>/dev/null` (there to skip silently with no tty) also hid the prompt text,
  leaving the operator at a blank cursor. The prompt is now printed separately so
  it is visible with a tty and still skipped without one.
- **🟡 Installer polish.** A bad MTU now re-asks instead of aborting the whole
  wizard; a multi-word bilateral field ("tunnel subnet") renders as one bullet,
  not two; the config-edit temp (a key-bearing copy) is kept out of `/tmp` —
  beside the config in `/etc/hs2` (root-only 600), removed on return, and swept
  at startup if an interrupt left one.

### Known limitations / follow-ups

- **The default cover page is identical on every install** ("Oakline", one fixed
  sha), so the page itself is a shared signature a censor could hash. For the
  best cover, set `backend_addr` in the config to your own real site; a
  per-install default page is a possible future change.
- **Backups contain the certificate private key** (`privkey.pem`); they are
  root-only (600) and the newest 10 are kept (`HS2_KEEP_BACKUPS`). Treat a backup
  as secret.
- Smaller: `hs2 doctor` does not detect a cert-renewal method that needs port 80
  (standalone authenticator); the link-pool screen (U7) does not note that the
  other side has its own cap; the config editor is always nano.

## Phase E — fixes from the second real-server review

The same independent reviewer re-tested the post-review build on a live server
(and ran a full direct-mode tunnel: TLS 1.3, matching keys, no plaintext on the
wire, hot cert reload, recovery after a killed link — all sound). It confirmed
the TCP_INFO fix with strace on both sides, and found two real problems with the
post-review fixes themselves:

- **🔴 The tunnel-manager menu died after every config edit** (`tmp: unbound
  variable` on Back). The edit temp's cleanup used `trap … RETURN`; a RETURN
  trap set in a function stays installed after the function returns and fires
  again when the *caller* returns — where `$tmp` no longer exists — and
  `set -u` killed the installer. (Our earlier claim, and the reviewer's, that it
  did not leak was wrong: the test that "proved" it never returned from the
  menu.) Fix: no RETURN trap at all. Every return path already removed the temp
  explicitly; an abnormal exit (Ctrl+C, SIGTERM, a `set -e` abort) is now
  covered by the existing `on_exit` EXIT handler via `HS2_EDIT_TMP`, and a hard
  kill by the startup sweep. The tunnel itself was never affected.
- **🟡 Probes other than the five plain-HTTP starts still closed with an RST**
  (DELETE, an HTTP/2 preface, random bytes) where a real Go HTTPS server sends a
  FIN. Cause: the 5-byte pre-TLS peek read *less* of the socket than crypto/tls
  does in its first read, leaving the rest of a small probe unread at close.
  Fix: the peek and its `prefixConn` wrapper are gone. `tls.Server` reads the
  socket itself, and a plain-HTTP request is recognised exactly the way
  net/http recognises it — from `tls.RecordHeaderError` — and answered with the
  same bytes and the same close. Read sizes, and therefore FIN vs RST, are now
  Go's own by construction rather than an imitation. (This supersedes the
  post-review "drain before close" patch, which only covered the 400 path.)
  Listener-side only, no wire change.

Not changed in this phase: the default cover page is still the same on every
install (see Known limitations above — set `backend_addr` to your own site).

New tests: `hs2-src/install/tests/tm_edit_test.sh` drives the real
`tm_tunnel_menu` through a pty under the installer's real `set -euo pipefail`
(Edit → Back, a saved edit, two edits, SIGTERM while the editor is open, a
`set -e` abort mid-apply) and forbids `trap … RETURN` in install.sh; it fails on
the previous install.sh. `TestProbeCloseMatchesStdlib` sends ten probes to both
our listener and a real net/http HTTPS server and requires the same bytes and
the same FIN-vs-RST for each; it failed on the previous server.

## Phase F — certificate renewal, editor choice, link-pool clarity

The follow-ups left open after phase E (all but U4 rekey, which was dropped at
the maintainer's request as too complex for the people who use this).

### F1 — certificate renewal that cannot fail silently
- **Found while preparing this phase:** a certificate issued with the installer's
  **DNS-01** option never renewed. certbot ran `--manual` with no auth hook, and
  certbot cannot renew such a certificate unattended (someone must publish a new
  TXT record each time), yet the installer said "Renewal set: 30 days before
  expiry". Such certificates expired after 90 days. Because the client never
  verifies the server certificate (auth is the shared key), **the tunnel kept
  working** — so nobody noticed, while every probe saw an expired certificate.
- The DNS-01 option now says up front that it is renewed by hand, and setup ends
  with an honest warning and the expiry date instead of "Renewal set".
- The tunnel screen's **Certificate** line (shown whenever the tunnel is opened,
  running or not) states expiry **and** how it renews; it flags DNS-01, an
  overdue renewal, a busy HTTP-01 port, a missing renewal config or
  `autorenew = False`, and an expired certificate. It replaces a line that
  claimed "auto-renews" for every certificate.
- New per-tunnel action **c) Certificate**: test the automatic renewal (a dry
  run against Let's Encrypt's test server — changes nothing), renew now, or for
  DNS-01 renew by hand on the same lineage with every name on the certificate
  (wildcards included). It refuses, and says why, when the HTTP-01 port is taken
  and no `pre_hook` frees it.
- `hs2 doctor` gains a read-only **cert renewal** check: it reads certbot's
  renewal config for the lineage, the kernel's listening sockets and the certbot
  timer, and reports DNS-01-by-hand, a busy HTTP-01 port (respecting
  `http01_port` and `pre_hook`), an overdue renewal, a disabled or missing
  renewal, no active timer, or an own certificate. It never runs certbot. An
  expired certificate still gets this diagnosis (why it was not renewed).

### F2 — the link-pool screen says which server's values count (U7)
- Verified against the engine: the Iran side always decides the link count. In
  direct mode the Kharej side only accepts links, so its values do nothing and
  the screen now says so and writes nothing. In reverse the Kharej side dials
  and caps Iran's target at its own min/max, so the effective ceiling is the
  lower max — the screen says to raise it on the Kharej server too. The `tls`
  mode is always one link and single-session transports have no pool; for those
  the screen explains and writes nothing.

### F3 — your own editor, without leaking the key
- Edit config uses `SUDO_EDITOR`, `VISUAL` or `EDITOR` (that order, as sudoedit)
  when it names a terminal editor installed here — a simple command line such as
  `vim -u NONE` is fine; desktop editors and missing ones are skipped with a
  note — else nano as before. Editor-specific hints (nano / vi keys).
- The copy being edited holds the tunnel key, and editors write more copies
  beside it (vim's `.swp`, emacs's `~` backup and `#autosave#`). Measured: with
  such an editor the previous version left **3 key-bearing files** next to the
  config; now the edit happens in a private directory (700) that is removed
  with everything in it on every exit path (normal return, Ctrl+C, SIGTERM,
  a dropped SSH session, a `set -e` abort; the startup sweep covers a hard kill).
- An editor that exits with an error (vim's `:cq`) after a change no longer
  applies it silently: the operator is asked first.

### F4 — stale tests brought in line with the code
- `helpers_test.sh` still expected the pre-A1 "warn and install anyway" when no
  hash is published; it now asserts the fail-closed contract (exact status 0 /
  1 / 2). `migrate_test.sh` extracts the new certificate helpers it now needs.

New tests: `cert_test.sh` (real certificates in a fake certbot tree; the menu
action driven on a pty with a recording certbot stub), `tm_tune_links_test.sh`
(every carrier × role × direction), the editor cases in `tm_edit_test.sh`
(including a **real vim** driven through the pty: `:wq`, `:cq`, and kill -9
mid-edit with the session dropped), and `doctor_cert_test.go`.

### Review fixes (independent adversarial review of phase F)
The review reproduced every finding through the real menu on a pty (and checked
certbot's behaviour against a real certbot 5.8). Fixed:
- **Pre-hooks certbot runs from anywhere count.** A port-80 holder freed by an
  executable in `renewal-hooks/pre/` or a `pre-hook` in `cli.ini` (not only the
  lineage's `pre_hook`) is no longer reported as "renewal will fail" — by the
  tunnel screen or by `hs2 doctor`. And when the port is busy with no visible
  hook, **c) Certificate** still runs the dry run (the real test — a hook we
  cannot see may free the port); a real "Renew now" asks first, default No.
- **The startup sweep no longer deletes another session's live edit.** The edit
  directory is named `.hs2-edit.<pid>.<random>`; one whose installer is still
  running (a second SSH window) is left alone, and swept once it is gone.
- `hs2 status` and the daemon log no longer say "certbot renews automatically"
  for an expiring certificate (a DNS-01 one never does); they point at
  `hs2 doctor`.
- The dry run is described honestly: it does not replace the certificate, but
  certbot's pre/post hooks do run.
- With no explicit `renew_before_expiry`, the renewal threshold follows
  certbot's own rule — a third of the certificate's lifetime (half under 10
  days) — instead of a fixed 30 days that called short-lived certificates
  "overdue" while certbot was correctly waiting.
- The installer and `hs2 doctor` read odd `cert_file` paths (`//`, `/./`)
  identically, and neither resolves `..` into another lineage.
- The editor shown in the menu is the one that opens (a leading space in
  `EDITOR` no longer shows "nano" while vim opens).
- `hs2 doctor` recognises a certbot scheduled from a crontab (a pip/venv
  install) instead of reporting "nothing will run the renewal".
- The `EDITOR='*'` test now really catches glob expansion (it runs where `*`
  would match an installed editor).

### Field request — the reverse exit's log says why a link went away
On the Kharej side in reverse, a link killed by the network (`ss -K`, a reset)
was logged exactly like the autopilot shrinking the pattern: `exit slot N
retired — pattern shrinking`. The behaviour was right (above the target, a slot
whose link ends is not redialed); the words conflated two different events and
dropped the cause. The link's end reason (the first socket error, in the same
words the Iran side uses) now reaches the pool, and the log says which it was:
- `exit slot 6 retired — closed by the edge while above its target (pattern
  shrinking) (now 7)` — the edge closed the link cleanly: normally the autopilot
  retiring an idle link. (Worded no more strongly than that on purpose: Go's TLS
  cannot tell the edge's close_notify from a bare FIN — e.g. a crashed edge
  process — so the log does not claim a decision it cannot see.)
- `exit slot 6 lost (read: reset by the network or the other server) — not
  redialed, pool above target (now 7)` — a loss (an RST such as `ss -K`).
- `… lost (no data from the edge for 24s (keepalive timeout — path stalled)) …`
  — a stalled path, instead of the misleading "closed locally".
- `… (read: aborted on this server (socket killed, e.g. ss -K or a local
  firewall)) …` — when the socket is killed on the exit itself (Go's raw
  "software caused connection abort", in the same plain words as the rest).
- `exit link down (slot 6: <reason>; now 7); redial` — at target, with the cause.
Log text only — no change to when links are retired or redialed. Proven over the
real TLS + smux stack: a clean close, a bare FIN, a genuine RST and a silent
(stalled) edge each produce their own reason, and the two reverse-shrink
integration tests now count retirements under the new wording.

A second, focused review of these changes (pool behaviour confirmed unchanged,
race-clean) found only wording/diagnostic gaps, all fixed: the clean-close
wording above and the keepalive-stall reason; hooks listed exactly as certbot
lists them (dotfiles count, `~` backups do not, `directory_hooks = False` /
`no-directory-hooks` honoured); a cron line counts only if it really runs
`certbot … renew` (not a `MAILTO=`, a check script, `certbot certificates` or a
`--dry-run`), only from cron.d names cron reads, and only with a cron daemon
running; the startup sweep also clears a leftover whose PID was reused by a
newer process (e.g. after a reboot); and the installer and `hs2 doctor` parse
cert paths identically (26-path differential test).

Not done: a per-install cover page (requested again from the field test). The
default cover page is still the same on every install — see Known limitations.

Known, not changed here: `tun_ports_test.py` fails the same 11 checks on `main`
and on this branch — it still expects the wizard's pre-U5 question order; it
needs its expectations updated, separately.

## Phase G — a per-install cover page (no user action)

**From the field test:** the built-in cover ("Oakline") was byte-for-byte
identical on every install and its text is in the public source, so one known
sha256 found every hs2 server in a bulk active scan. Pointing `backend_addr` at
a real site fixes it, but most users never do, so the default is what the whole
fleet shows.

- The installer now writes a per-install `cover_seed` (128-bit, from
  `openssl rand`, **independent of the shared key** so the public page can leak
  nothing about the secret) into every builtin-cover tunnel it sets up, and
  `migrate_config` adds one to existing builtin tunnels on the next upgrade — no
  user action. A custom `backend_addr` keeps priority and ignores the seed.
- `hs2-src/cmd/hs2/cover.go` turns the seed into a page whose brand, hero copy,
  accent colour (hue + saturation + lightness), favicon glyph, layout tokens
  (radius, widths, gap, class names), service cards, long-section set and order,
  size, `Last-Modified` offset and `ETag` all vary. No two installs share a
  hash, and no single surface value clusters (measured over 2000 pages: 2000
  unique hashes, ~1000 distinct brands, 359 hues, top hue 0.6%), so the ensemble
  is not obviously generated either.
- **Stability contract:** the bytes are a pure function of (seed, year) via a
  SHA-256 counter stream (byte-stable across Go versions, unlike math/rand), so
  a server shows the same page across restarts and binary rebuilds; only the
  copyright year ticks, for everyone at once. A golden test pins the exact bytes
  for a known seed, so the generator can never change silently (which would move
  every server's page on upgrade day — a correlated fleet event).
- **No seed → the exact legacy page**, byte-for-byte (an old or hand-written
  config is undisturbed; verified it still hashes to the field-observed
  `86dfcf86…`).
- **Honest scope (and the README says so):** this defeats cheap hash/structural
  enumeration; it is NOT a disguise against a determined prober — the TLS stack
  is still Go's, and a classifier trained on several generated pages could still
  recognise the family. A real `backend_addr` is still the strongest cover.

### Field-review fixes (real-server test of the cover page)
A real-server test confirmed the whole checklist, and found two over-claims and
a quality note, all fixed:
- **ETag was a bare `sha256(body)[:16]`** — a *single-probe* fingerprint: anyone
  could fetch the page, hash it, and confirm the server used hs2's exact rule.
  It is now SALTED with the seed (stable per seed+content, so caching still
  works, but unpredictable from the body, like a real server's mtime/inode
  ETag). The seedless legacy page sets **no** ETag at all, exactly as the
  pre-per-install binary did.
- **The neutral palette was constant** — only the accent varied; the text, grey
  and dark-background hexes were the fixed Oakline values on every page. They
  are now jittered per seed within a contrast-safe band, so no base colour is a
  shared constant either. (The earlier "nothing constant remains" was too
  strong; the honest claim is: no byte-exact token is shared, a trained
  classifier still could recognise the genre.)
- **migrate_config** now edits the config on a snapshot and keeps the result
  only if the binary still validates it (rolling back otherwise), instead of an
  unchecked in-place `sed` — matching the "check, then replace" path used
  elsewhere.

New tests: `cmd/hs2/cover_test.go` (determinism + golden, 500-seed uniqueness,
seedless==legacy with the `86dfcf86…` check, structural invariants: no external
requests, no JS, inline favicon, light+dark, balanced tags, size varies; served
headers: Content-Type, ETag, 304, root-only 404) and
`install/tests/cover_seed_test.sh` (every builtin heredoc seeded, seed
independent of the key, migration adds one / leaves a custom backend alone /
never duplicates, valid JSON). Verified live over real TLS: two seeds serve two
different pages, a seedless config serves the exact legacy page.

## Phase H — a link-pool ceiling sized to the server, shown exactly on both servers

The pool's ceiling (`max_links`) was a flat 32. At 300–400 *active*
connections the autopilot wants ~50 links (one per 8 active), so 32 was the
binding limit; a flat 64 everywhere, though, would let a 1 GB VPS buffer up to
~512 MiB (8 MiB per link under a stalled reader). The ceiling now follows the
hardware, and both servers show the number that actually applies.

### H1 — auto ceiling (`max_links: 0`), re-derived at every start
- `max_links` is three-state: **`0` = auto** — from the server's RAM/cores at
  every start, using the same low/medium/high profile that sizes the kernel
  buffers (**32 / 48 / 64**, `tune.RecommendedMaxLinks`); a **number** = fixed,
  never changed automatically; **absent** = the historical fixed **32**, so a
  config written before this behaves exactly as before after the upgrade.
- A server resized up or down gets the matching ceiling on its next start (a
  VPS resize needs a reboot anyway), with nobody editing the config. An older
  binary reads `0` as its fixed 32, so a rollback still runs.
- New installs write `0`. Existing installs are never changed (the migration
  still writes a literal 32 for the two pre-adaptive lines only).
- **One behaviour change to know:** an explicit `"max_links": 0` used to mean
  "the default, 32"; it now means auto. No installer ever wrote 0 (it wrote 16
  or 32, and the menu accepted 1–1024), so only a hand-edited config, or an
  older `hs2 config set max_links 0`, is affected — on a medium/high box it now
  gets 48/64. Set a number to keep it fixed, or remove the key for the old 32.
- When a tunnel is added on a server that keeps an older hs2 binary, the new
  config still says auto, and the installer says plainly that the old binary
  runs it as a fixed 32 until hs2 is upgraded (it never shows the old binary's
  "unknown command" as hardware information).
- Startup log line: `link pool: ceiling N links — auto …` / `fixed …` /
  `the default …`. `hs2 recommend-links [--why]` prints what this server's
  hardware gives; `hs2 config set max_links auto` sets auto.

### H2 — both servers learn the other's ceiling (display only)
- **Stream (TLS) tunnels:** a one-shot `kindInfo` stream per link, opened by the
  Iran (edge) side in both directions: `[ver][n][max u16]` each way, 5 s
  deadline, a few bounded retries on a congested link. An older exit closes the
  unknown kind and the edge stops at once; an older edge never opens it.
- **Datagram (dgtun) tunnels:** two trailing bytes on the control frames that
  already flow — the exit's ceiling on `TypeLinkStats`, the edge's on
  `TypePoolCtl` (now also sent by a direct edge, every ~15 s ± 20 %; a direct
  exit has always ignored its target). **No new frame type**, so the datagram
  carrier, FEC, pacing and drop paths are untouched; older parsers read exactly
  the bytes they always read. A report older than 45 s reads as unknown.
- None of it feeds the autopilot or the pool. Each server reads the other's
  number only from links that are up now, so a value from links that are gone
  (e.g. after the other server was rolled back to an older release) is never
  shown; a ceiling lifted by a higher `min_links` is shown as exactly that.

### H3 — the exact effective ceiling, on both servers, in both directions
- Direct: the Iran server's ceiling alone (the Kharej exit accepts every link
  it dials — its `max_links` is not applied). Reverse: the lower of the two
  (the exit clamps the edge's target to its own max).
- `hs2 status` prints a `ceiling:` line on **either** server: the effective
  number, which side sets it, and both servers' own ceilings (auto/fixed/
  default, with the profile and hardware); unknown is said plainly (no link up,
  or an older hs2 on the other side).
- `hs2 doctor`: WARN when a fixed (or default) ceiling is above what the RAM
  comfortably holds, INFO when the box could use more; for auto, WARN/INFO
  when the hardware changed under a running daemon (restart applies it); a
  direct Kharej gets only the fact that its value does not apply.
- Installer → tunnel → Tuning → **Link pool**: shows `auto, now N`, accepts
  `auto`, shows the hardware's number, and prints the daemon's exact ceiling
  line (on every role, including a direct Kharej whose own values do nothing).
  Min/max are written in an order the binary's per-step validation accepts.

### Measured (real binary, real TLS on loopback, 400 connections each moving data)
| scenario | Iran `max_links` | Kharej `max_links` | links reached | both servers show |
|---|---|---|---|---|
| direct | auto (64) | 48 | **50** | 64, set by Iran (Kharej's 48 not applied) |
| direct | 32 | auto (64) | **32** | 32, set by Iran |
| reverse | auto (64) | 48 | **48** (target 50) | 48, limited by Kharej |
| reverse | 40 | auto (64) | **40** | 40, limited by Iran |
| reverse | absent | absent | **32** | 32, both — the default |
| reverse, older Kharej | auto (64) | 48 (old binary) | 48 | Iran: "does not report — older hs2" |
| reverse, older Iran | 32 (old binary) | auto (64) | 32 | Kharej: "does not report — older hs2" |

Peak RSS per process in these runs was 55–67 MiB at 32–50 links with 400
connections (no stalled reader, so the per-link 8 MiB buffer cap was never
approached). Mixed versions carried traffic normally in both directions. The
datagram path was run the same way over real UDP and TUN devices (direct,
reverse, `max_links` absent, and an older binary on either side): every case
showed the exact number on both servers, or said plainly that the older side
does not report it.

### Field-test fixes (real servers, 400 active connections)
A real-server test confirmed every claim in both directions (auto 48 on the
field servers' 1.9 GB "medium" boxes, a fixed 40 on Kharej limiting reverse to
exactly 40 TCP links, direct ignoring Kharej's cap, old configs staying at 32,
mixed versions working). One display point was fixed:
- **Reverse: the Iran side's target did not say it was capped.** With the
  Kharej exit at 40, Iran still printed `target 48` and logged
  `pattern 28 → 48 links`, while 40 links ran. The links line now reads
  `40 up / target 48, capped at 40 by the Kharej server`, and the log line adds
  `— capped at 40 by the Kharej server (its max_links), so at most 40 links
  run` (stream and dgtun). Display only — the target itself is unchanged; a
  direct edge never claims a cap (a direct exit does not clamp), and an older
  Kharej that does not report its ceiling is never assumed to cap.
- `gofmt` alignment in two files touched by this phase.
- Not field-tested (no 1 GB server at hand): a 1 GB box getting 32. It follows
  from the profile thresholds and is unit-tested (512 MB and 1 GB → low → 32);
  on any 1 GB VPS `hs2 recommend-links --why` must print `32 (low profile …)`.

### Test maintenance
- `install/tests/tun_ports_test.py` had gone stale: it predated the optional
  tunnel-subnet prompt (U5) and the `hs2 check` validation of every written
  config (A2f), so 10 installer branches stalled (on `main` too). It now
  answers the subnet prompt on the link-making side and gives the installer a
  real `hs2` binary to validate with; all installer branches pass again, and
  with root + iproute2 + ping its namespace part passes too (137 PASS: real
  traffic, direct and reverse, l3mtcp / tls / dgtun over udp, ipx and gre,
  plus the blocked-GRE case).
- `install/tests/multi_tunnel_test.py` had gone stale the same way: it never
  answered the subnet prompt (U5), the upgrade restart confirmation (U2) or
  the typed `REMOVE ALL` that now guards a whole-server uninstall (U14). Its
  pty now answers the optional subnet prompt wherever it appears, and the
  upgrade/uninstall steps answer their confirmations; 54 PASS, 0 failures
  (several tunnels side by side on two namespace "servers", upgrade,
  backup/restore, delete, uninstall).

## Phase I — each Iran user port to its own panel inbound (per-port targets)

The Iran server could open several user ports (`forward_ports: "8443,2053"`),
but the Kharej server delivered every connection to its ONE panel address
(`expose`): a panel with one inbound per port could only be served by one
tunnel per port. There was also no menu to add or remove a user port after
setup, and neither setup question said how several ports are handled.

### I1 — the routing table lives on the Kharej server
- New Kharej key **`port_map`**: comma-separated `P` (= `127.0.0.1:P`, the same
  port on the Kharej server — the default) or `P=host:port`. A user port with
  an entry goes there; every other port goes to **`expose`** (the default
  panel); with no `expose`, a port without an entry is **refused**, and the
  Kharej server logs which port (once a minute per port) and shows it in
  `hs2 status` / `hs2 doctor`.
- On every user connection the Iran server says only **which of its user
  ports** the user came in on — a number, never an address — so a compromised
  Iran server can reach only what the Kharej operator listed (never
  `127.0.0.1:22`, a database or the panel's admin port).
- A config without `port_map` behaves exactly as before.

### I2 — on the wire, with every older/newer combination working
- **Who decides:** once the Iran server knows the Kharej server routes by
  port, it says the port on every TCP connection and stream (the Kharej server
  then decides with the table it has *now* — a port mapped there a moment ago
  included); the cost is 2–3 bytes once per connection. Only the datagram tun's
  UDP, where the bytes would ride on every datagram, is tagged just when the
  Kharej server has a table at all (a `port_map`, or no default): a Kharej
  server without one gets those datagrams byte for byte as before.
- **Stream tunnels (mtcp, l3mtcp, tls):** two new stream kinds,
  `kindTCPPort`/`kindUDPPort` = `[kind][port u16]`. The per-link `kindInfo`
  exchange (Phase H) is now **v2** — `maxLinks, caps, flags, count, ports…` —
  so each side learns whether the other routes by port, which ports the Iran
  server opens (and whether it forwards UDP) and which ports the Kharej server
  has its own target for (and whether it has a default). A v1 reader takes the
  first two bytes as always. A new link takes user connections once its first
  exchange attempt is over (one round trip): answered → that answer decides;
  closed by an older exit → never tagged; no answer in time (5 s) → the link
  goes by what the pool's other links learned from the exit while it keeps
  asking (after its three quick tries, once a minute) — a slow answer never
  leaves a link without one for its whole life. What the pool learned is
  forgotten when no link is left (the exit may have restarted).
- **Datagram tun (dgtun, every encap):** untagged traffic stays on tun port
  28443 → the default panel, exactly as before. Tagged traffic goes to the new
  tun port **28444**: such a TCP connection and each such UDP datagram start
  with `[1][port u16]`. The edge **probes** 28444 (port 0 = probe; the same v2
  info message both ways) at start, every 15 s, faster while unsure — also with
  no user port, so the Kharej server learns that too. Refused (an older exit)
  or answered by something that is not an hs2 exit → untagged; silent while
  28443 answers, asked twice, and again on the next probe → *filtered* (a
  firewall on the Kharej server's tun) → untagged, and both servers say so (a
  single silence after a good answer is taken for a tun flap); an answer that
  is only late changes nothing. Until the first probe answers nothing is
  tagged: a connection waits for it while the tun is not up yet (an untagged
  dial would wait just the same; at most 10 s), but goes untagged at once when
  the tun answers without a verdict. A refused tagged TCP connection falls back
  to 28443 *before any user byte is sent*; a UDP flow is redone the right way
  when what is known changes, and a refused UDP flow only asks the probe (it
  never turns per-port routing off for the whole edge). The exit now always offers UDP on
  both tun ports, so turning UDP on needs only the Iran server (an older Kharej
  build still needs `"udp": true`). Tagged UDP flows are keyed by the edge's
  source address and replaced, not dropped, when that address is reused for
  another port; targets are resolved once, not per flow.
- **Compatibility, all verified live:** newer Iran + older Kharej → every port
  reaches its one panel (the Iran side's `hs2 status` says the Kharej server
  runs an older hs2); older Iran + newer Kharej → it does not say the port, so
  every connection reaches the default panel (the Kharej side says so if it
  persists with links up). Direct and reverse alike.
- The throughput / FEC / drop path (udpcarrier, FEC, the datagram pool) is
  **untouched**: dgtun per-port routing lives entirely in the userspace
  forwarder on top of the tun.

### I3 — where each port goes, shown and edited on both servers
- **`hs2 status`** prints a `ports:` table on either server. Iran: each user
  port → its own target on the Kharej server / the Kharej server's default
  panel / **NO target (refused)** with the fix; Kharej: each port the Iran
  server opens → its target, ports with a target that Iran does not open, and
  the default. Unknown is said plainly (tunnel not running, no link yet, or an
  older hs2 on the other side).
- **`hs2 doctor`**: WARN for an Iran user port the Kharej server has no target
  for, for a filtered dgtun tag port, and (on the Kharej server) for a local
  target nothing listens on — TCP listeners and bound UDP sockets (a UDP-only
  inbound counts), read from `/proc/net/{tcp,tcp6,udp,udp6}`; no connection is
  made.
- **`hs2 ports -c cfg [add P[=host:port] | remove P | default host:port|none |
  udp on|off]`**: the table, and this server's half to edit (Iran: user ports
  and UDP; Kharej: own targets and the default; one port per change, and a
  hand-broken `port_map` is never rewritten). Every change is validated like
  `hs2 check` and written atomically. `hs2 config set` also accepts
  `forward_ports`, `port_map`, `expose` and `udp` (normalized).
- **`hs2 check`** validates `port_map` (port, target, duplicates), accepts a
  Kharej server with `port_map` and no `expose` (mtcp/tls still need one of
  the two), warns that `port_map` does nothing on the Iran server or on an IP
  tunnel, and (dgtun) that a target on 28443/28444 would fight the forwarder.

### I4 — installer
- **Kharej setup** says the Iran server opens the ports and this server decides
  where each goes, asks the **default panel**, then *"Iran user ports with
  their OWN inbound here"* (`2053,2083` = the same port on 127.0.0.1;
  `2053=127.0.0.1:2096` for another; Enter = none). A bad entry — including a
  target the binary would refuse, such as `:80` or an unbracketed IPv6 — is
  re-asked at the prompt, never at the end of setup; skipped with a note when
  the installed binary predates it.
- **Iran setup** says, before the user-port question, that each port reaches
  the Kharej server's target for it (its default panel unless it has its own).
- **Tunnel manager → `p) Ports`** on both servers: the live table, then
  Iran — add / remove user ports (checked free on TCP — and UDP when it is on —
  and never the tunnel's own listen port), UDP on/off; Kharej — own targets
  (Enter = the same port on 127.0.0.1, several at once), remove, default panel
  (or none; Enter keeps it without a restart). Each change is applied with the
  usual restart and automatic rollback, and followed by **what — if anything —
  has to change on the other server**. Several ports at once are all-or-nothing:
  one refused entry undoes the others, and says so; a change is not made when
  the backup it would roll back to cannot be written. An older binary or an IP-tunnel
  transport is refused with the reason. The tunnel's details show its own
  targets, and the setup summary points at the screen.

### Review fixes (independent adversarial review, all confirmed and fixed)
- dgtun: one refused UDP flow (Iran UDP on, Kharej UDP off — exactly what the
  Ports screen allowed) flipped per-port routing off for the whole edge, over
  and over: TCP on mapped ports went to the default panel and the log flooded.
  A UDP error now only asks the probe, and the exit always offers UDP.
- stream: a link whose info exchange timed out was used untagged for its
  whole life (in `tls` mode, every connection). It now goes by what the pool's
  other links learned, and keeps asking.
- dgtun: a late probe answer was taken for an older exit; now it changes
  nothing. A tag port that is filtered no longer leaves connections waiting:
  nothing is tagged until a probe answers, and filtered is detected and named.
- dgtun UDP datagrams carry the tag only when the Kharej server has a table,
  so an existing config sends them identically (no 3-byte header on datagrams
  near the tun MTU). A second review pass then found that the Iran side could
  keep an old copy of the Kharej server's table after a Kharej-side Ports
  change; now TCP connections and streams always say their port and the Kharej
  server decides with its current table. Also from that pass: a connection
  made before the tun is up waits for it instead of going to the default
  panel; a tun flap is not taken for a filter; "filtered" never outlives a
  later refusal; a silent tagged port pokes the probe at once; the UDP
  forwarder on 28443 checks its target resolves before it binds; the installer
  loops are safe under `set -u` on bash older than 4.4.
- dgtun exit: a reused source address no longer drops a new flow; targets are
  resolved once. Status: an Iran server with no user ports, a filtered tag
  port and a slow answer are each said as what they are (not "older hs2");
  the refusal log is per port AND protocol. Doctor counts UDP inbounds and
  `localhost` on `::1`. `hs2 ports` refuses a list in one change and never
  rewrites a broken `port_map`. Installer: all-or-nothing multi-port changes
  with a message, no glob expansion of entries, targets validated like the
  binary.

### Test maintenance
- `tm_edit_test.sh` was intermittently red (about 1 run in 5): the edit-dir
  sweep compared two whole-second clocks, so a directory made within a second
  of its owner's start could look older than it and be swept. The sweep now
  allows 2 s before calling a PID reused (a real reuse is hours apart).
- The pty tests (`tun_ports_test.py`, `multi_tunnel_test.py`) answer the new
  Kharej questions; the multi-tunnel pty also answers optional prompts that
  come after the last listed step.

## Phase Q — up to 300 links on a strong server, and what keeps that safe

Production was pinned at the old 64-link ceiling (Iran 20 cores / 17 GB,
Kharej 12 cores / 22 GB, ~500 active of ~6,000 open connections). The auto
ceiling now follows the RAM up to 300; most of this phase is what makes
hundreds of links safe on restart, on outage, under stalled readers and on
small servers. Every change went through an independent adversarial review
(33 findings, measured where possible); the confirmed ones are fixed here.
Both servers should run this build; mixed versions keep working.

### Q1 — the ceiling
- Auto (`max_links: 0`): one link per 48 MB of RAM, at most 300, never below
  the old 32/48/64 profile; one core keeps its profile value, 2–3 cores at
  most 128. Explicit numbers are never rewritten; absent stays 32.
- dgtun uses the same auto ceiling. Its interim cap (64 over a raw
  encapsulation, 128 over udp) was lifted after its load test: 300 gre
  carriers on the shared raw socket carried 176 Mbit/s (28 with one socket per
  carrier, the older build); 300 udp carriers ran with p99 121 ms, no dropped
  connection, ~5% more CPU than 128.
- Sizing reads the cgroup's limits when lower than the host's (containers,
  units with MemoryMax/CPUQuota). Go's soft memory limit is half the RAM.
- `hs2 check` warns about `min_links` above 64. A hand-edited **Kharej**
  config with `min_links` above `max_links` is now an error for `hs2 check` /
  `hs2 config set` (it was silently accepted).

### Q2 — getting to 300 and back without storms
- One dial gate per process: ≤ 8 handshakes in flight, starts 40–160 ms
  apart (~10 links/s; 300 links in ~31 s measured). A dial checks it is still
  wanted before taking a start slot, so retiring slots never hold up the ones
  that must dial (the exit's first link after an outage was ~17 s late; now
  0.2–1.3 s, test-measured). One failed handshake no longer drops the queued
  dials — only 3 in a row, or a failure with no link up.
- Outage: the Kharej side lets one slot retry (2 s connect, every ≤2 s); the
  others wait. Its log says when the outage begins (`no link up to the edge —
  dials fail`) and when a link is back (`… back after Xs with none up`); a
  shorter outage used to show neither (failures are folded every 30 s).
- Warm start: a restart within 15 min comes back at the previous size (only
  ever raising the start size; written after a minute of uptime).
- Refill hold: after a start or a total loss a new TCP connection waits up to
  10 s for a link with fewer **open** connections than the fair share, then
  goes onto the existing links — never refused, and said in the log when that
  is above the cap, with the reason: a pool of hundreds cannot be back within
  10 s at the gate's pace (`links open at the dial pace … — expected, not a
  fault`), and only a pool well behind that pace is blamed on the path.
  Simulated with the real pool: an outage with 2,400
  connections and 300 links put 2,400 on one link before, 24 now; a
  production-shaped restart (6,000 open, 63 links) 781 before, 96 now.
- Reverse accept cap: live links only, 2×max+8; a refused link is held 5 s
  and refusals are logged once a minute with their count.
- Per-link cost: pool-control refreshes fast on 2 links, every 30 s on the
  rest, and a change goes out spread over 1.5 s; control pings stay on a fixed
  3 s cadence (a jittered version had re-enabled a download-loss false alarm);
  l3mtcp's side channel keeps quiet when both servers support it.

### Q3 — stalled readers
- A few users whose apps stop reading used to fill a link's 8 MiB receive
  buffer, stop every other connection on it and get the link killed by the
  other server's TCP after 20 s (Kharej RSS 5.5 GB in the stall test). Now the
  connections whose app took nothing for 6 s while the buffer is full are
  reset (`mtcp: reset N connection(s) on K link(s) …`); slow readers that
  still read are kept (test: released in 7.7 s, slow and fast readers kept).
- Doctor: kernel TCP memory against tcp_mem, the conntrack table, and several
  tunnels' combined worst case; the status writer logs once when kernel TCP
  memory passes its pressure mark. `tcp_tw_reuse=1` is now set.

### Q4 — the Kharej side counts its own users and traffic
- Open connections, active ones, Mbit/s and the last minute's peak on the
  Kharej server too (direct and reverse, mtcp and dgtun), in the live monitor,
  `hs2 status` and `hs2 doctor`; an older hs2 there says "counted on the Iran
  server" instead of 0.

### Q5 — dgtun at scale
- Raw encapsulations (icmp/gre/ipip/ipx): one shared dial socket per peer
  instead of one per carrier — the kernel copied every received packet to
  every carrier's socket. Measured per packet (send+receive): 300 carriers
  86–90 µs before, 11.4–11.6 µs now; one carrier unchanged. Link ids are
  unique (at 300 carriers two used to collide about half the time).
- A carrier closed on purpose is closed on the other side at once (it stayed
  "alive" 15–17 s); a restarted peer's dead carriers go as soon as a fresh one
  comes up; a scout dial runs while every carrier is silent.
- Retiring is mirrored to the other side, and flows stuck to a retiring
  carrier move after 30 s, so a shrink finishes under nonstop download; the
  reverse edge serves retired/spare carriers again when its target rises.
- Accept loops survive transient errors; the shrink sort is O(N log N); per-
  carrier log lines are folded.

### Q6 — load test, and what it found
Three network namespaces (Iran, a userspace netem middle: 400 Mbit/s, 40±5 ms,
0.05% loss with bursts, a 3 Mbit/s per-flow policer; Kharej), production
tuning, 2,400 active users (256 B echo every 500 ms), 3,000 idle, 16 bulk
downloads: 5,416 open connections. Old = the main build before this phase.
- 300 links reached in ~57 s; CPU ~48% of a core per side, RSS 400–480 MB;
  four minutes after the load the pool is at ~90 links (old: 200).
- Iran restart under load (3 runs): users served normally again after
  16.3 / 16.4 / 16.3 s (old: 71 s); 40 s outage (3 runs): 16–20 s (old: 61 s).
- Mixed versions and l3mtcp reverse run at 300 links.
- dgtun counted interactive users (~1 KB/s) as idle, and each connection
  twice (once per direction): the pool sat at 8 carriers for 100 s with echo
  p99 up to 87 s. Both fixed: 2,416 active counted, p99 121 ms at 300 udp
  carriers, no dropped connection.
- Review fixes: the reverse edge's refill hold waits only for the links the
  exit allows and ends when links stop coming; dgtun's serve notice can no
  longer be lost for good, a restarted exit's first carrier is not taken for
  spare, receive stamps are monotonic, the direct exit's flow map is pruned;
  the outage begins/ends in the Kharej log and the scout's first retry is
  capped; doctor totals several tunnels at their effective ceilings; status
  names the usable cores; the installer's visibility note comes once the
  direction and carrier are known.
- Found: Go 1.24+ opens listeners as MPTCP by
  default, and an accepted MPTCP socket ignores tcp_notsent_lowat, so a user
  whose app stops reading holds up to its whole send buffer (4–7 MB) of
  kernel memory and keeps its link busy (echo p99 8 s for the others). With
  MPTCP off the stall guard resets exactly those users and p99 is 0.5 s; under
  normal load no difference.
- A degraded link no longer takes all its users down with it after 45 s (it
  cut 60–90 active connections per link in the test). It still takes no new
  user and is replaced at once; after 45 s its connections that moved no data
  for 15 s are closed (they reconnect onto healthy links): idle ones, and on a
  link that is really stuck every one, 15 s after its data stopped — all at
  once: each close waits for the link's writer, and one after another would
  have kept most users waiting minutes. A connection whose data still passes
  stays, however little moves (an SSH session being typed in, a game's beat),
  45 s more — 90 s after the link degraded the link closes with what is
  left. (A first cut kept them up to 5 min: in the test they had p90 1.6–2.7
  s all that time, against 0.2–0.4 s a minute after a reconnect.) The
  draining link keeps them only while the
  pool can refill without its slot: it does not count against max (all links
  stay within max + an eighth), the reverse exit is now asked for the
  replacement at once (before: only after the bad link closed), and when the
  pool is at its ceiling and short of serving links the oldest draining links
  close as before. Log: `link N degraded for 45s — its connections that moved
  no data for 15s (idle or stuck) are closed now …`, once a minute `closed N
  connection(s) on degraded links that moved no data for 15s`, and at the
  ceiling `N degraded link(s) (…) closed with their M remaining
  connection(s): the pool is at its ceiling and needs their slots`.
- A link failing slowly during the stats handshake was taken for an older
  exit: `the other server does not report link stats (older hs2)` with both
  servers current (seen on stuck links in the load test, old build too). An
  exit that answered the info exchange is newer than stats, so the edge now
  just tries again.
- MPTCP off: every listener is plain TCP (see the finding above). The
  carrier, user-port and dgtun listeners set it explicitly; go.mod's `godebug
  multipathtcp=0` covers the rest and the tests.

### Q7 — stuck links (throttled, not cut)
DPI can throttle a link to a few packets a second without cutting it. Such
a link still gets a keepalive through, so it is never *suspect*, and it
moves too little for the loss rule, so it is never *degraded*. It kept
serving, and it took new users because it looked the least loaded. In the
load test, main kept 7 of 10 such links until the end, and only 79% of
echoes were answered for good.

- **The signal.** Each link's control ping travels behind its own traffic
  both ways, so the age of its oldest unanswered ping is how long its users
  wait. The control loop keeps up to 4 pings outstanding. A write that
  timed out stays queued in smux, so it no longer ends the loop. The wait
  is published on every answer.
- **The rule.** A link is *stuck* when all of these hold:
  - its ping has waited 6 s, in two samples in a row;
  - it moves less than 12 KB per 2 s, or less than half of what the links
    that answer promptly move (and under 96 KB);
  - another busy link answers in under 2 s now, to a ping sent *after*
    this link's oldest;
  - the path is not slow (below).

  A stuck link is degraded at once: no new users, and its replacement is
  asked for. Its connections that moved no data for 15 s close right away,
  and the rest get 90 s at most.
- **What it leaves alone.**
  - **A slow path.** The path or the other server is slow when two or more
    links wait 6 s while moving little, and they outnumber the busy links
    answering promptly. Moving users would not help, so nothing is drained
    and nothing is judged for loss. That holds afterwards too, for as long
    as the spell was slow all told (30 s to 2 min), because TCP backed off
    through it and resumes up to that long after.
    - Links answering in 2–6 s, heavy links waiting behind their own
      backlog, and links that never answer (an older exit) count neither
      way. A review showed that counting them as "slow" let two lossy links
      at night switch both rules off, silently.
  - **A link moving its share.** On a full path, a link whose users wait
    behind their own load while it moves its share is not stuck.
  - **A link wedged by its own users.** If its reader was parked on a full
    receive buffer, the wedge guard handles it.
  - **A suspect link.**
  - **Drain limit.** At most an eighth of the pool drains as stuck at a time.
- **Log.**
  - `link N stuck: its traffic has waited 10s for an answer while it moved
    1.8 KB in 2s (the other links answer in ~85ms) — draining`
  - then `link N stuck — its connections that moved no data for 15s are
    closed now …`
  - on a slow path, once a minute: `N of M busy links have waited 6s+ for
    an answer and only K answer promptly — the path or the other server is
    slow, not those links: none is drained`
- **Load test (reverse, ~200 links, 1,600 active + 2,000 idle users, 16
  downloads; "stuck" = 3 packets/s each way via iptables). Release build
  unless noted.**
  - **10 busy links stuck.** 9 were caught at 10.6 s; the 10th was already
    draining for loss. Answered echoes went from 76% to 100% within 60 s,
    and p99 settled at ~335 ms. Main stayed at 79% for good.
  - **10 random links stuck.** All 10 were caught, 8 of them within 13 s.
    p99 stayed at 580–650 ms; main's went to 44–69 s.
  - **Small pool (max 10), 4 links stuck.** All 4 were caught at 9–15 s,
    and answered echoes were 100% after 30 s. The build before the review
    fix took the 4 for a slow path ("4 of 8 busy links … none is drained")
    and answered 51–56% for a minute.
  - **Squeezes: no stuck verdict, drops in main's range.**

    | Squeeze | This build (active drops) | Main (active drops) |
    |---|---|---|
    | 8 Mbit/s for 60 s, two runs | 1,485 and 1,178 | 1,519 and 1,527 |
    | 12 Mbit/s for 120 s | 1,219 | 1,108 and 1,410 |

  - **Shallow queue.** The same 8 Mbit/s squeeze through a shallow 20 ms
    queue, where packets are dropped rather than queued, also gave no stuck
    verdict. But this build dropped 1,730 and 1,750 active connections, and
    main 1,537 in one run, with 4 loss verdicts 1–2 min after the squeeze
    where main had 1. This is still open and is followed on the real server
    (VALIDATION.md, V5).
  - **Outage, 40 s.** No stuck verdict, and normal service 18 s after it
    ended.
  - **Iran restart under load.** No stuck verdict, and normal service
    again after 14.3 s, as before.
  - **600 users whose apps stop reading.** No stuck verdict; the wedge
    guard reset those 600 connections, as before.
  - **20% loss on 10 links.** The loss rule drained 7 of them in this run.
    In an earlier run of this phase it drained 11, where main drained 6.
- **The loss rule waits out a slow spell too.** Before this was fixed, the
  stuck builds dropped about 8% more in the 8 Mbit/s squeeze (1,651 and
  1,662), with no stuck verdict at all. The extra drops came from the loss
  rule: it drained 4–8 links for download loss just after each squeeze,
  where main drained 1–2.
  - Why main drained fewer: a single ping write timeout ended a link's
    control channel for good. That left the loss rule without the exit's
    retransmit count for the rest of the link's life. The control channel
    now survives, and right after a squeeze it reported TCP's recovery
    retransmits.
  - The fix: no link is judged for loss during a slow spell or its recovery
    window, and the rule then needs three fresh samples.
  - The same survival makes the loss rule see lossy links it used to miss.
    In the 20%-loss test the new build drained 11 lossy links where main
    drained 6, cutting 365 active connections against 178. Those users
    reconnect onto healthy links, and p99 matched main.
  - Only links whose control channel has answered count toward the slow
    test. A pool on an older exit, whose links never answer, would
    otherwise pass for slow for good and silence the loss rule.
  - Earlier cuts of the rule drained 210, 55, then 27 links in the
    squeeze, cutting hundreds of users who would have recovered. Each was
    fixed before this one.
- **Reviews.** Two independent adversarial reviews; every confirmed finding
  is fixed and has a test that fails without its fix (more than 30
  mutations, all caught).
  - A minority of stuck links (more than a third of the busy ones) passed
    for a slow path for good.
  - A link waiting on its own load was taken for stuck.
  - The per-tick cap did not bound how many links drained at once.
  - The wait stayed stale until every outstanding pong was back.
  - A reused ping buffer rewrote queued pings.
  - An answer arriving just after an outage began counted as proof that
    the path worked.
- **Not seen from the edge.** A wedge on the exit's side (its reader parked
  by a slow target). The exit's guard frees readers that stop within
  seconds, before a verdict; one that trickles is judged like a throttled
  path.

### Q8 — the health rules at high bandwidth (and UDP flows)
Q7 was load-tested at 16 downloads. A new rig mode (`run3.py --bulkmb
--bulkspread`: downloads of a few MB that end and come back, started over the
ramp) put 150 downloads and 2,400 active + 3,000 idle users through 300
links at 150-200 Mbit/s, each link behind the rig's per-connection throttle
(3 Mbit/s, dropping what goes over it). On the release build (and on the
build before it alike) the health rules misjudged the busy links there, and
an independent review found why.

- **The loss rule read pong timing, not loss.** The download loss was the
  exit's retransmits from the last control pong over the bytes of the 2 s
  health tick. Pongs come every 3 s, so one tick in three read no loss (the
  streak reset) and the others held 3 s — or 6 s — of resends over 2 s of
  bytes. A link resending 30-90% with steady pongs was never drained; one
  resending 9% with pongs jittering behind its own queue, as on every busy
  link at peak, was (16 of 20 in 5 min). It is now judged over the window
  between two pongs, against what came in over that same window, and each
  direction keeps its own streak.
- **The loss fraction was inflated.** Its denominator was payload bytes /
  1400 while retransmits count TCP segments, so a link of small or padded
  frames read 1.1-10× lossier than it was. It is now the socket's own count
  of data segments (TCP_INFO, Linux ≥ 4.6; bytes / mss on older kernels).
- **No limit, no comparison.** Every candidate was drained in the same tick:
  a lossy path drained all 300 links at once in the review's test, and the
  pool at its ceiling then closed them with their users. Now:
  - a link that resends a lot but still moves at least half of what the
    pressed links move (those whose sender waits for the path: what a link
    gets) is left alone — its resends are what the path's rate costs, a
    throttle dropping what goes over it, and its users would get no more
    elsewhere;
  - when most busy links resend more than 12% below that rate, the path is
    lossy: none is drained (`N of M busy links resend more than 12% — the
    path is lossy, not those links: none is drained`, once a minute);
  - at most an eighth of the pool drains at a time, the lossiest first.
  - The line now says what the link moves against the others: `link N
    degraded (up-loss —, down-loss 25% of 840 segments, moving 0.4 Mbit/s
    where the busy links get 2.8, rtt 120ms) — draining`.
- **A congested path passed for stuck links.** Through a squeeze, light links
  still answer within 2 s and outnumber the heavy ones backing off, so the
  slow-path count read a minority of stuck links: a 30 s squeeze from 190
  to 60 Mbit/s drained 33 links as stuck. The path is now also slow when
  links wait and the ones that answer promptly take 4× their usual time
  (the lowest median of the last 10 minutes) and over 0.5 s: `N of M busy
  links have waited 6s+ for an answer and the K that answer promptly take
  ~1298ms, 15× their usual ~88ms — the path is congested, not those links:
  none is drained`.
- **UDP: one slow flow held up the whole port.** The Iran side's one read
  loop per UDP user port wrote each datagram to its flow's link itself, and
  opened the stream of a new flow itself: a user whose link was throttled
  or busy stalled every UDP user of that port (in a test the other user, on
  a healthy link, got 0 of 200 datagrams). Each flow now has its own queue
  (256 datagrams, 512 KB) and writer; a full queue drops, as a full UDP
  socket would. UDP forwarding is off unless it was turned on at setup.
- **Load test, release build → this build** (reverse, 300 links, 5,500
  users, 150 downloads; "loss10" = 10% random loss on a link, "stuck" = 3
  packets/s each way):

  | Scenario | Release | This build |
  |---|---|---|
  | Steady, 2.5 min: loss verdicts / connections cut | 28 / 131 (the build before it: 40 / 693) | 1 / 2 |
  | Squeeze 190 → 60 Mbit/s for 30 s: stuck / loss verdicts / cut | 33 / 39 / 1,738 | 1 / 1 / 99 (the stuck one: a link TCP had backed off, still waiting 49 s, ~30 s after the squeeze) |
  | 3 links at 10% loss: caught in 90 s / loss verdicts / cut | 1 of 3 / 41 / 722 | 3 of 3 / 6 / 322 |
  | 4 busy links stuck: caught | 2 (2 more already draining for loss) / 31 loss verdicts | 4 of 4 in 9-11 s / 1 |

  And at Q7's load (1,600 active + 2,000 idle users, 16 downloads, ~200
  links):

  | Scenario | Release | This build |
  |---|---|---|
  | 10 busy links stuck | 9 of 10 at 10.6 s | 10 of 10 at 10.6 s |
  | 40 s outage: stuck / loss verdicts after it | 0 / 6 | 0 / 0 |
  | 8 Mbit/s squeeze, 20 ms queue: active cut / loss verdicts | 1,730-1,750 / 4 | 1,706 / 0 (one link still backed off 63 s after the squeeze drained as stuck) |
  | 20% random loss on 10 random links: drained / cut / p99 | 4 closed by 120 s / 657 / 3.0-4.3 s | none (light, or moving what the busy links get) / 0 / 2.5-5.8 s |

- **From the first real-server report** (two test servers, 1-3 cores and
  2 GB each, auto ceiling 48, 550-950 real users, reverse, the release build):
  - **Stalled readers under kernel memory pressure (🔴).** Twenty downloads
    whose app was stopped for 60 s (`iperf3 -R -P 20`, `kill -STOP`), under
    250 users: the guard reset nothing (one stalled download per link never
    fills a link's buffer), the kernel's TCP memory went past its pressure
    mark (113 of 169 MB), every socket was squeezed, and the health rules
    drained 11 healthy links for loss and as stuck — 44 users cut, p50 84 →
    170 ms. Now, while either server is past the mark (the exit says so in a
    new flag of its link stats; an older edge ignores it), the guard resets
    the connections whose app took nothing for 6 s without waiting for a
    full link, and no link is judged lossy or stuck, nor for the recovery
    window after.
  - **The pool did not shrink (🟠).** Five minutes after the load, 48 links
    stayed for 137 active users: the per-link capacity estimate (~0.9
    Mbit/s) came from the few slow links still pressed at low load, sample
    after sample. It is now one entry per link — the best sustained rate
    each showed while pressed in the 30-minute window — and their median.
  - **A lossy link missed (🟠).** One of two links at 20% loss was never
    drained in 150 s: the pong-timing estimate (above), and its traffic
    dipping under the activity threshold, which reset the streak. A quiet
    sample now keeps a streak whose last bad sample is under 20 s old.
  - **Uninstall left files.** The tunnels' warm-start records and the ping
    guard's folder in `/run/hs2` are removed; the option says it keeps the
    program, and asks whether to remove `hs2` and `hs2-menu` too.
  - Also confirmed there, with real users: growth, restarts of either
    server, a 30 s outage, one and four stuck links, a path-wide 2 Mbit/s
    slowdown (no verdict) — as on the rig.
- **What the rig also showed, not changed here.**
  - A sudden crowd onto a small pool (5,500 connections in 30 s onto 8 warm
    links) leaves the first links crowded (~235 connections each) until
    their users reconnect: the refill hold covers a start or a total loss,
    not a crowd onto a pool that is already up. Their downloads are pinned
    to those links.
  - 10,000 open connections: ~0.9 GB RSS on each server (copy buffers of
    32 KB each way per connection), 74% of a core on the Iran side at ~190
    Mbit/s in the rig.
  - The review also found (not changed yet): a burst of new connections
    (over ~40 a minute per link) can hide a link's real pressure from
    placement, so 6-10% of new connections land on throttled links; the
    autopilot's probe on the reverse edge does not know the exit's ceiling.

## Phase V — tun over icmp: a cut carrier heals in a second, and a ceiling that fits icmp

A review of the datagram tun (`dgtun`) over icmp found that the autopilot sizes
it like the stream pool and the pool already heals a restart, a path outage, a
policer and random loss — but not one carrier cut while the others work, and
that its ceiling ignored what icmp is.

### V1 — a carrier whose own way through is cut moves its flows in ~1 s
- **What was wrong.** A carrier was only taken for dead when it failed a send,
  when the other server said goodbye, after 15 s of silence, or when a new
  carrier arrived (and only those silent 3 s+), or when *every* carrier was
  silent (the scout). One carrier cut alone — its icmp echo id (or its port)
  dropped by a middlebox, its state lost on the other server — kept its flows,
  and took 1/n of the new ones, until its 15 s timeout. The inner TCP backs
  off meanwhile, so its users came back after 26-38 s; with only the replies
  cut, some never did within the test. Nothing was logged.
- **Now.** Each side looks every 250 ms. The other server sends feedback on
  every carrier every 100 ms, so a carrier that has heard nothing for 1 s
  while another carrier still hears the other server is **mute**: new flows
  avoid it and its flows move at their next packet; it tells the other
  server (`closeMute`, sent twice; an older peer ignores it), whose flows
  leave it too — so a one-way cut heals on both sides. At 3 s it is closed
  and replaced. A mute carrier that hears again takes flows again
  (`closeHear`; the other server's word also expires after 4 s). Nothing is
  judged while no carrier hears the other server — then the path or the
  other server is down, and the scout (unchanged) takes over — nor right
  after the process (or its VM) was stopped for a moment.
- **Visible.** `dg: carrier N has heard nothing from the other server for
  1.2s while 5 other carrier(s) still do — its own way through is cut: new
  flows avoid it and its 10 flow(s) move to live carriers`, then `dg:
  carrier N heard nothing for 3.2s — closed; a new carrier replaces it`; on
  the other server `dg: carrier N: the other server hears nothing on it —
  what goes there is lost: its flows move to live carriers`. `hs2 status`
  flags such a carrier `M` on its `carriers` line and counts the closed ones
  (`mute:` line; `mute_closed` in the status file). A carrier that fails
  outright (a send or read error) now says so too: `dg: carrier N failed
  (…) — its flows move to live carriers`.
- **Measured** (two network namespaces, dgtun over icmp, 6 carriers, 24 TCP
  downloads; one carrier's echo id dropped silently on the way in):

  | Cut | Release: connections stalled / longest | This build |
  |---|---|---|
  | Both directions | 9 / 26-38 s, no log line | 7 / 1.5 s |
  | Kharej → Iran only | 9 / 38 s (not back by the end) | 6 / 2.0 s |
  | Iran → Kharej only | — | 3 / 1.4 s |

### V2 — tun over icmp runs at most 8 carriers
- Every carrier of a tun over icmp is one echo identifier between the same two
  IPs: they share one path and one policer (an ICMP rate limit sees their sum,
  which is why the governor caps them together), so past a handful more
  carriers add no bandwidth — only a pattern ping never makes (up to 300 echo
  ids to one host on a strong server with the auto ceiling) and feedback
  traffic (10 reports a second each way per carrier). With `max_links` 0
  (auto) or absent, a tun over icmp now runs at most **8**; a positive
  `max_links` still fixes the number (doctor and status warn above 8).
- Said everywhere the ceiling is: the start line (`link pool: ceiling 8 links
  — tun over icmp: every carrier is one echo id between the same two IPs, so
  more than 8 add no bandwidth …`), `hs2 status`, `hs2 doctor`, `hs2
  recommend-links -c <config>`, and the installer (end of setup, and Tuning →
  Link pool).
- An existing icmp tunnel without `max_links` ran the historical 32; it now
  runs 8.

### V4 — ping under load: a fair queue per carrier, interactive flows first
- **What was wrong.** Each carrier had one FIFO send queue (up to 256 packets,
  50 ms) in front of its pacer, which holds up to 20 ms more. On a busy pool
  every carrier carries some download, so a game's, a call's, a DNS or a ping
  packet waited behind up to ~70 ms of it: that was ping and jitter under
  load. A full queue also dropped the newcomer — often the interactive packet.
- **Now** (`engine/dgfq.go`): each carrier's queue is a deficit round robin
  over its flows. A *sparse* flow — nothing queued, under 256 kbit/s lately
  (a game, a call's audio, DNS, ping, a remote shell, the first packets of
  any connection) — is served before the backlog, and its packet takes a
  fast lane in the carrier's pacer (after FEC parity, ahead of the data
  queue). Downloads take turns by bytes. A full queue drops the head of the
  flow with the most queued. A flow's packet takes the fast lane only once
  everything it sent the ordinary way has left the pacer's data queue (the
  carrier counts the shards in and out of it), so a flow's packets keep their
  order however slowly that queue drains; a heavy flow paced just under the
  carrier's rate does not keep the head start (all it sends spends its rate
  budget), so it cannot starve the downloads.
  Bandwidth, pacing, FEC and the 50 ms sojourn bound are unchanged.
  `HS2_DG_FQ=0` restores the single FIFO.
- **Measured** (two network namespaces, tun over icmp, 4 carriers, a
  20 Mbit/s bottleneck each way, 8 downloads, ping through the tun):

  | | FIFO (`HS2_DG_FQ=0`) | fair queue |
  |---|---|---|
  | ping idle | 0.7 ms | 0.6 ms |
  | ping under load: p50 / p99 | 66 / 94 ms | 12 / 20 ms |
  | jitter (std dev) | 17 ms | 5 ms |
  | pings lost | 5 of 120 | 0 of 120 |
  | throughput | 16.4 Mbit/s | 16.4 Mbit/s |

  What is left (~12 ms) is the bottleneck's own queue, which the carriers'
  delay-based pacing keeps short. Over three runs the fair queue gave p50
  11-15 ms with no ping lost; the FIFO 66-144 ms, losing up to 43 of 120.

### Review fixes (independent review of V1-V4, all confirmed and fixed)
- A flow whose queue never empties (a VPN over UDP inside the tunnel, faster
  than its carrier) grew its queue's array by every packet it had sent (64 MB
  after 2M packets in the review's test): the popped part is now reclaimed.
- The fast lane's ordering guard was a fixed 100 ms; at a carrier's floor
  rate, or under a policer cap, its ordinary packets could wait longer, and a
  later packet overtook them (reproduced). It is now the queue's own count.
- Pool control no longer rides a carrier the other server hears nothing on.
- A full queue looks for the fattest flow among the queued ones only, not
  every flow seen in the last 2 s.

### From the real-server report (two test servers, Iran 3 cores / Turkey 2 cores, reverse, build 1b5591f91bee)
- The icmp ceiling: 8 on both servers, with its reason (the hardware alone
  would have allowed 79-81).
- On that path only icmp carries data: udp carriers came up and went silent
  (100% loss), so a mixed icmp+udp pool would gain nothing there.
- Fair queue vs FIFO under an 8-stream download: download 66-72 vs 47-53
  Mbit/s, ping loss 0.7% vs 4.7%, p99 140 vs 198 ms.
- One carrier cut (both ways, and Iran-only): mute in 1.0 s, flows moved at
  once, closed at 3.0 s, a new carrier ~7 s; the download never stopped.
- The cap (~60-72 Mbit/s down, ~80-86 up) was the Turkey server's CPU (2
  cores at ~97%, shared with the production tunnel), not the path: one
  carrier 47-51, eight ~60 (CPU parallelism, no policer detected). A lab CPU
  profile of the sending side puts ~30% in syscalls (one per datagram on the
  raw socket and one per packet on the tun) and ~25% in goroutine hand-offs;
  crypto is ~4%.

### V3 — guidance: the kernel's own ping replies on a dedicated server
The icmp listener keeps the server answering ordinary ping by dropping only
the kernel's replies to tunnel packets (nft/iptables). The kernel still
builds each of those replies before the rule drops it — a copy per received
tunnel packet, on the side that receives the echo requests (in reverse the
Iran server, for the download). On a server that runs only the tunnel,
`HS2_ICMP_SUPPRESS=global` (in the service's environment) stops the kernel
answering ping at all instead, which saves that work; the server then does not
answer ordinary ping.

## Phase W — less CPU per gigabyte, and carriers that share the bottleneck

The real-server report of Phase V put the datagram tun's limit on a small
server at the CPU (Turkey, 2 cores at ~97%), and a lab profile of the sending
side spent ~30% of it in system calls: one per datagram on the socket, one per
packet on the tun.

### W1 — several datagrams per system call
- `sendmmsg`/`recvmmsg` (new package `mmsg`) on the raw sockets of every
  encapsulation (dial and listen side) and on udp sockets. The pacer gathers
  the datagrams already waiting that its token bucket can pay for (at most
  16, in lane order) into one call; nothing leaves earlier than its pacing
  allows. A datagram the kernel refuses (a soft error such as EMSGSIZE) costs
  only itself. `HS2_RAW_BATCH=0` sends and receives one at a time.
- The carriers' read loop takes the frames already waiting in one pass and
  writes what they carry to the tun together.
- `HS2_PPROF=127.0.0.1:<port>` serves Go profiles, on a loopback address only.

### W2 — TCP offload on the tun
- The tun is opened with a virtio-net header and TCP segmentation offload:
  the kernel hands hs2 one TCP packet of up to 64 KB, which hs2 cuts into the
  usual MTU-sized segments with full checksums (exactly what the kernel would
  have sent one by one; the IP ID, sequence numbers and flags per segment as
  Linux does it), and finishes any checksum the kernel left partial. On the
  receiving server consecutive in-order segments of one TCP connection are
  written to the tun as one packet (the kernel's GRO rules). Nothing changes
  on the wire; an older hs2 on the other server works with it.
- `HS2_TUN_OFFLOAD=0` turns it off; a kernel that refuses it gets plain
  packets. The start line and `hs2 status` (`tun:` line) say which, with the
  packets per read and per write, malformed kernel packets dropped and merged
  packets the kernel refused (those go in one by one).
- **Measured** (two network namespaces, icmp, reverse, 2 cores a side): a
  download ~450 → ~650 Mbit/s; CPU per GB on each server 27-39% lower (agent
  round 1: 16.3 and 14.3 CPU-s/GB against 23.3 and 21.8 for the release);
  every copy intact in every mode.

### W3 — carriers share the bottleneck (rate control)
A ping-under-load check of W1/W2 behind a 30 Mbit/s bottleneck found ping
p50 from 11 to 70 ms run to run and some runs losing 10-18% of pings — with
the release as well (61 ms in one release run). A trace of the rate
controller showed why: the carriers of a pool meet at one bottleneck, and
each read the queue they all built as its own.
- **A carrier stuck in startup.** Startup ended only on a queue seen while
  the carrier used its allowance. When the users' TCP filled the bottleneck
  before the pacer did, the carrier was never "limited" and stayed in startup
  at 2.9x its delivery (75 Mbit/s allowed on a 30 Mbit/s path): unpaced, the
  queue sat in the bottleneck instead of in hs2's fair queue, and pings
  waited and were dropped there. In the simulator a source offering 1.02-1.5x
  the path (2x as well on a short path; at 80 ms round trip the 2x offer
  used its allowance and left) kept it so for good: queue at the buffer
  limit (120 ms), thousands of drops. Now startup also ends once a queue has stood for 3 reports
  (~300 ms) while the carrier carries more than ~1 Mbit/s and at least half
  what the pool's busy carriers carry (their fair share; with none, the
  active carriers' mean). A light carrier — a call, the other direction's
  ACKs — keeps startup's fast ramp for its own bulk later, unless the queue
  stays past 30 ms for eight base round trips (one to three seconds) while
  it delivers less than it sends and the busy carriers do not see that queue
  (theirs short, or under half its own: the governor publishes the queue
  they see): far deeper than the others' pacing holds and growing under it,
  so its own, on a path of its own. The governor holds its fair share and
  that queue through a second without busy carriers (a base probe slows
  them all at once), so a light carrier does not read itself as busy, or a
  shared queue as its own, for a moment. The capacity it starts
  from is what got through, not the allowance: 9.5 ms, no drops.
- **A stale peak.** Out of startup, a carrier's capacity was floored at 0.4x
  its windowed peak delivery — a window that only advances while it uses its
  allowance. A carrier that had run at 900 Mbit/s and then met a 30 Mbit/s
  bottleneck kept ~360 Mbit/s and paced nothing (seen with mixed versions:
  ping 122/475 ms, 5% lost). The floor now holds only while the carrier uses
  its allowance (it guards against a queue someone else built); otherwise
  its capacity follows what it delivers; and while a queue stands a peak
  older than 1.5 s leaves the window, so the floor cannot come back from it
  either: 120 ms and thousands of drops → 9.5 ms, none (simulator, measured
  from 2 s after the bottleneck appears); lab, 30 Mbit/s tbf added after a
  ~510 Mbit/s download: ping p99 28-59 ms, none lost (release: up to 1960
  ms and 19% lost).
- **Late carriers starved.** After startup a carrier's capacity tracked its
  own delivery, so whoever held the queue first kept it: the others read it
  as theirs and sat at ~0.3 Mbit/s, with the flows on them. Now the pool's
  governor computes the fair share (the mean rate of the carriers using
  their allowance), and while a queue of 5-20 ms stands each such carrier
  grows toward it: 0.5% of the share, or 10% of its gap below it, per
  report — never more than 3% of its own capacity; like every per-report
  step, scaled to the part of a round trip the report covers (per round
  trip on paths over 100 ms).
  With the queue term's proportional cut that settles every busy carrier on
  the same rate. Not while no queue stands: a carrier on its own slower path
  (a pool over several IPs) cannot tell its queue from the pool's, and
  growth there drove it into its buffer (100-200 ms in an earlier version
  of this rule; now 33-40 ms p95 against 12-17 ms without the rules, no
  drops, its whole path's rate).
- **Base probes together.** The carriers' base-delay probes now fall on one
  shared 4 s clock (anchored on the monotonic clock, so a wall-clock step
  cannot hold them back; at least 3/4 of a period apart, so a probe that
  started late is not followed by another), so the whole pool slows at once
  and the queue really empties; one carrier probing alone while the others
  kept the queue full measured a base with the queue in it.
- **Paths over 400 ms round trip measured at all.** An RTT sample over 8x
  the smoothed RTT is dropped as a clock step; the smoothed RTT starts at 50
  ms, so on a path over 400 ms every sample was dropped and the controller
  ran on 50 ms for good (since the controller was written; the release
  too). The first sample now counts, and so does the third outlier in a row
  (a step is one sample in flight). Simulator, a carrier on its own path at
  600 ms round trip behind a 200 ms buffer: queue p95 168-251 → 39-60 ms,
  up to 1112 drops → none.
- `HS2_FAIR_SHARE=0` turns the three off.
- **Measured.** Pool simulator (new tests, `udpcarrier/rate_pool_sim_test.go`;
  each carrier sees the round trip its reports really measured and the loss
  a full buffer caused), carriers joining a second apart: Jain's fairness
  index 0.45-0.91 → 0.99-1.00 over 30-60 s on paths of 20-120 ms round trip
  (0.93-0.98 already 10-25 s after start), 0.43-0.55 → 0.77-0.87 on 160-240
  ms; queue p95 27-38 → 17-21 ms, utilization 99% both; eight carriers (the
  icmp ceiling) on 16-100 Mbit/s: 0.97-0.99, queue p95 19-21 ms, no drops.
  Lab (icmp, 4 carriers, 8 downloads behind a 30 Mbit/s tbf each
  way): ping under load p50 17-19 ms, p99 21-34 ms, no ping lost in any run
  (release: p50 11-114 ms, p99 47-410 ms, runs losing 13-24% of pings);
  behind 8 Mbit/s with 4 downloads p50 18-28 ms, p99 27-48 ms, none lost.
- The pacer's token bucket is now capped after a timer wait as well: a timer
  that fired late on a busy server let one batch exceed its budget.

### Verification by agents (six rounds, each agent under 20 minutes)
- **Round 1** (7 agents on W1/W2): bandwidth, mixed versions, every encap
  and stress passed; the code review found the three bugs below; the
  latency agent's regression traced to the rate control (W3), present in
  the release too.
- **Round 2** (8 agents, W1-W3): high bandwidth (NEW 486-748 vs release
  334-459 Mbit/s, CPU per GB 33-38% lower, 81 copies intact), every encap
  and switch (26 runs intact), stress (5% loss: 297 vs 181 Mbit/s), many
  connections (p99 21-38 vs 38-70 ms), 30 Mbit/s latency (p99 20-30 vs
  47-410 ms, no ping lost) passed. Found and fixed since: slow convergence
  on an 8 Mbit/s path, the stale peak after a fast period, the probe clock
  on the wall clock, a slow separate path pushed into its buffer, a light
  carrier ramping slowly later (W3 above).
- **Round 3** (4 agents on those fixes): 30 Mbit/s ping p99 23-26 ms with
  no ping lost in 12 runs (release 22-52), 8 Mbit/s delivery 5.65 vs 5.32
  Mbit/s and carriers within 2x from 18 s (release 5-24x), high bandwidth
  696 vs 488 Mbit/s and 5% loss 240-252 vs 142 Mbit/s, all intact. Found and
  fixed since: the stale peak floor returning on reports that use the
  allowance, and a light carrier on its own filled path kept in startup
  (W3 above); the simulators' probe clock made deterministic.
- **Round 4** (2 agents on those fixes): lab, a 30 Mbit/s bottleneck
  appearing after a fast download, 9 runs (7 new on both sides, 2 with the
  release on the Iran side): no ping lost, p99 24.5-45.5 ms, every copy
  intact, the fast carrier's capacity down to ~25 Mbit/s within 10 s (the
  version before kept 1043 Mbit/s: 12% lost, p99 1142 ms); 8 Mbit/s with 4
  downloads, 8 runs: no ping lost, p99 25.8-46.6 ms, busy carriers within
  1.41x at 22 s (before: 1.61x). The review found that a light carrier on a
  long shared path (100-200 ms one way) left startup on the busy carriers'
  own startup queue, and its bulk later ramped at 1.6-3 Mbit/s instead of
  20-26 — hidden in the pool simulator, which fed the controllers no round
  trip. Fixed since: the deep-queue exit needs eight round trips and a
  delivery shortfall (now 24-27 Mbit/s, as with the rules off, also behind
  a 600 ms buffer); the simulator feeds real round trips; the cap on the
  fair-share step no longer shrinks with the round trip (it was 3% of
  capacity × 250 ms/τ; now 3% per report on any path, per round trip past
  100 ms) (fairness there 0.63 → 0.81-0.88).
- **Round 5** (2 agents on those fixes): lab, 5 stale-after-fast runs (no
  ping lost, p99 18.9-28.8 ms, every copy intact), 30 Mbit/s with 8
  downloads (p99 21.7-24.7 ms against 36.2 before) and 8 Mbit/s with 4
  downloads, 12 runs (no ping lost, p99 median 34.2 ms against 33.0 before):
  no regression. The review found that the wait for a light carrier's own
  deep queue counted smoothed round trips, which hold that very queue: on
  its own path behind a 0.5-2 s buffer the wait grew with the queue until
  the queue became the base delay, and the carrier stayed in startup on a
  full buffer for good (thousands of drops). Fixed since: base round trips,
  at most 3 s (left startup 3.3-3.7 s after joining, behind a 1 s buffer at
  300 ms; after round 6's busy-queue rule the simulator gives 1.0 s at 2
  Mbit/s and 3.7 s with 11 drops at 4 Mbit/s). Its second finding — fairness among 10-16 carriers on a narrow
  short path worse than before the unshrunk fair-share step — did not hold
  in a wider sweep: 4-10 carriers on 8-24 Mbit/s at 20-80 ms round trip,
  Jain 0.825 on average against 0.813 before (better or worse case by case,
  by a wide margin either way where per-carrier shares are under ~2
  Mbit/s); 12-16 carriers with 4 light ones on 12-16 Mbit/s: 0.38 against
  0.41, both collapsing, 0.22 with the rules off. The pool simulator now
  also reports a full buffer's drops as loss, as the peer does.
- **Round 6** (2 agents on those fixes): lab behind deep tbf buffers (30
  Mbit/s with 1 s, 8 Mbit/s with 1.5 s): no ping lost, the queue held at
  15-75 KB after startup, no busy carrier left in startup — the release
  kept every busy carrier in startup there (ping p50 84-117 ms); the
  standard runs unchanged. The review found the base-round-trip wait too
  short at 360-600 ms round trip on a shared path: the busy carriers' own
  startup queue outlasted it, and a light carrier delivers short of what it
  sends in a shared FIFO as well, so it left startup again (its bulk later
  1.6 Mbit/s instead of 24). A carrier alone cannot tell a shared queue
  from its own, so the pool tells it: the busy carriers' queue (above).
  Now 24-27 Mbit/s for a 1.2-2 Mbit/s light carrier at 300-400 ms round
  trip in every probe phase, as with the rules off (a 4 Mbit/s one at 300
  ms: 3.2-4.0, rules off 2.5); a light carrier on its own path still leaves (72 cases,
  0.2-2 s buffers, 30-600 ms: at most 5.5 s, queue p95 at most 62 ms, no
  drops). It also found the 400 ms RTT lockout (above).
- Pre-existing, unchanged (release the same): under a steady policer with
  no loss episodes the pool sends ~2.5x what passes and parity rises to its
  ceiling; many tiny flows behind a shared bottleneck all count as sparse,
  so the fair queue cannot single out a UDP echo among them; 16 carriers on
  an 8 Mbit/s bottleneck collapse to the floor rate (see the next item for
  the wider picture); a carrier out of startup that later gets bulk on an
  empty path re-ramps at the capacity probe's 4% per report (every 100 ms:
  ~1.5x a second; 4% per round trip on paths over 100 ms); 300 ms RTT with 26% bursty
  loss sometimes collapses utilization (4 of 30 seeds in the simulator,
  rules on or off); a carrier still in startup from a fast period sends
  its first 1-2 s unpaced when a slow bottleneck appears (a few hundred
  drops at the bottleneck, before pacing takes over); after that the
  formerly fast carrier keeps a multiple of the others' rate for 10-20 s.
- Pre-existing, found in round 5 (release the same, rules off alike): with
  8 carriers on a 16-24 Mbit/s path at 80 ms round trip, the carriers that
  join while a queue already stands take it for the base delay, the tail
  drops of the full buffer for the path's random loss, and loss
  compensation then keeps the pool on a full buffer (simulator: Jain
  0.13-0.26, queue at the 200 ms buffer, tens of thousands of drops; with
  the drops not reported as loss, 0.81-0.92 and no drops). At 16 Mbit/s and
  40 ms, 30 Mbit/s and 120 ms, or 100 Mbit/s and 80 ms the same pool is
  fine (0.97-0.99, no drops). A fix belongs in the loss compensation and is
  left for a separate change. 10-16 carriers sharing 8-16 Mbit/s collapse
  the same way.
- Known, with the rules: a flow outside the pool that holds the
  bottleneck's queue past ~100 ms squeezes every carrier (a delay-based
  controller next to a loss-based flow; the release too), and with the
  rules a light carrier as well — a 1.2 Mbit/s call got ~0.5 Mbit/s in the
  simulator, where with the rules off it stays in startup and keeps its
  rate. Behind a queue held at 60 ms a call next to busy carriers keeps its
  1.2 Mbit/s; a call alone in its pool is no light carrier (nothing busier
  to compare with) and got 0.76 Mbit/s behind 60 ms, 0.5 behind 120 ms.
  `HS2_FAIR_SHARE=0` gives the release behaviour back if this shows.

### Review fixes (agent round 1)
- A udp listener on an IPv6 address read in batches dropped every datagram
  (the batch held IPv4 addresses only): batches are now used on IPv4
  sockets only.
- A packet the kernel refused in a tun batch write lost the rest of the batch
  and none of it was counted: now only that packet is lost (a refused merged
  packet goes in segment by segment) and the count is exact.
- A transport checksum that computes to 0 is written as 0xffff, as the kernel
  does (for udp, 0 means "no checksum").

## Phase AL33 — on-server alerts (`hs2 alert`, Telegram optional)

- New subcommand `hs2 alert -c <config>` (one check, or `--loop 60s`;
  `--test` sends a test message, `--dry` prints what would be sent). Run by a
  per-tunnel systemd timer the menu installs. Read-only toward the tunnel;
  state per tunnel in `/var/lib/hs2/alert-<unit>.json`.
- Rules: service down / restarted / panic, no serving link with users
  connected, links below half of the current target, an LB IP fully cut, a
  path flapping, RTT above its own (hour-of-day) baseline with a single
  network-wide message when all paths rise together, CPU pressure, and a
  certificate close to expiry. Each rule carries false-alarm guards for idle
  servers, small pools, the minutes after a restart, intentional drains and a
  missing/stale status file (see the README table).
- Output: journal (`hs2-alert`), `/var/log/hs2-alert.log`, and Telegram when
  `/etc/hs2/alert.conf` (mode 600) has a token and chat id; HTTP/SOCKS proxy
  supported; Persian by default (`lang=en` for English). The token never
  appears in logs or errors. Daily summary at 09:00 by default.
- Installer: tunnel manager → **a) Alerts (Telegram)** — on/off, token, chat
  id, proxy, language, summary time, test message, last alerts, check now.

## Phase P31 — nginx-identical answers to active probes

Front-door fixes for the built-in cover with `HS2_COVER_NGINX` (on by
default), checked probe by probe against a real nginx 1.24 (certbot's
default TLS config) in the lab. They only touch connections that have not
authenticated as a tunnel link; only the listening side (the Iran edge in
reverse mode) needs the new build.

- **Malformed request line:** parsed byte by byte like nginx; the first
  invalid byte gets an immediate `400 Bad Request` and a close (random bytes
  without `\n` used to get no answer; a lowercase method used to get `405`).
- **Oversized head:** request line > 8 KB → `414 Request-URI Too Large`;
  header line > 8 KB or head > 32 KB → `400` *"Request Header Or Cookie Too
  Large"* (used to be served normally).
- **Timeouts:** a connection that sends nothing is closed after 60 s with
  `close_notify` (was 30 s); a partial request is closed silently after 60 s
  with no response (was a `400`), like nginx's `client_header_timeout`.
- **`/robots.txt` and `/favicon.ico`** are served.
- **HTTP/2 deliberately not offered:** certbot-configured nginx offers only
  `http/1.1`, and Go's h2 `SETTINGS` would fingerprint differently.
- **Lab:** after == nginx on every probe in the battery except the intended
  ones (cover body/sizes, robots/favicon); tunnel A/B unchanged (links 60/60,
  no light-user loss, same CPU).
- **Tests:** `cover_head_test.go` (cmd/hs2), `server_nginx_test.go`
  (tlscarrier); README table *Answers to active probes, nginx-identical*.

## Phase R29 — flapping-path penalty on by default; single-queue NIC notes

- **Flapping-path penalty** (`HS2_LB_FLAP_PEN`, lb auto2): a local→remote
  IP pair with ≥ 4 relative cuts in 60 min (lossy and clearly worse than the
  best path; whole-IP verdicts, forced cuts and "all paths lossy" do not
  count) is kept out for 30 min, doubling on each repeat up to 2 h. At most
  ~1/3 of the paths are penalized at once, never leaving fewer than 2 healthy
  paths or under 60 % healthy; all penalties lift at once if the other paths
  degrade. A penalized path keeps probing and can return early (clean for a
  third of its penalty) on a 30-min probation. Tuning keys
  `HS2_LB_FLAP_N`, `_WIN_S`, `_HOLD_S`, `_MAX_S`, `_PROB_S`, `_EARLY_DIV`.
  Lab: cut cycles and lossy-exposure seconds fell in 6/6 interleaved runs,
  crowd p99 −12.7 ms, no light-user cost; in production on the reference
  tunnel's kharej since 2026-11-10.
- **On by default** for the exit side (kharej, `mode: listen`) on reverse
  `l3mtcp`, through the central table (`hs2-src/envdef`; now 38 keys: 29 on
  both servers, 5 Iran/edge only, 4 kharej/exit only). `HS2_DEFAULTS=legacy`
  turns it off; an explicit `HS2_LB_FLAP_PEN=0` wins.
- **README:** the key in *Production defaults* and *Throughput and
  load-balancing keys*; a new note, *Single-queue virtio NIC (one rx
  interrupt core)* — how to spot it and the optional manual steps that
  helped on the reference server (hs2 — and optionally other heavy
  processes — kept off the rx-IRQ core with `CPUAffinity=`/`taskset`, and
  `net.core.netdev_budget=600`: that core's idle at peak ~22 % → ~45 %).
  RFS was tried and not kept (single-stream upload regressed). Nothing
  there is applied by hs2 or the installer.
- **Tests:** `TestFlapPen*` (engine), `envdef` (table size, exit side),
  `TestApplyDefaults` (on for exit, off for edge, explicit `0`, legacy).

## Phase D22 — the production key set on by default (reverse `l3mtcp`)

A plain install (no `Environment=` lines) now behaves like the reference
tunnel: on the `l3mtcp` carrier in reverse mode, the 37 keys that tunnel runs
are on by default, each on the side it runs on (29 on both servers, 5 only
on Iran/edge, 3 only on kharej/exit). The table and its rules live in one
place (`hs2-src/envdef`); see README, *Production defaults*.

- **Explicit always wins.** A key set in the environment keeps its old
  meaning, whatever its value: `HS2_X=0`, `off` or an empty `HS2_X=` turn
  that one key off; a number replaces the default value.
- **`HS2_DEFAULTS=legacy`** (or `0`) keeps the table dormant: every key is
  back to its old default (off) unless set explicitly.
- **Scope.** Only `l3mtcp` (`l3`) in reverse mode. `mtcp`, `tls`, `udp`,
  `auto`, `dgtun`, `reality`, `noise` and direct mode keep the old defaults.
  The table is read after the config (the side comes from `mode`: `dial` =
  edge, `listen` = exit), before anything starts; the start-up log lists the
  defaults in force.
- **Left off:** `HS2_MCONN_STRIPE_JUMP_MS` (no measurable gain).
- **Mixed versions.** Every default-on key works against the previous
  published binary (each new behaviour is announced by a capability bit or is
  carried in a field older builds already parse); checked in the lab both
  ways (new Iran + old kharej, old Iran + new kharej).
- **Tests:** `envdef` (table, sides, legacy, explicit override),
  `TestApplyDefaults` (engine, both sides), `TestDefaultsApply` (cmd/hs2,
  which carriers).

## Phase U21 — adaptive windows and a faster load balancer (all keys opt-in)

A field-tuning round on a busy reverse `l3mtcp` tunnel (several Iran IPs,
~12k users, ~80 ms RTT). Every key is off by default; with none set the
binary behaves as before. Each change was A/B-tested in a production-like lab
(≥ 6 interleaved runs, crowd of light users, permutation tests) and kept only
if light users got no slower; then deployed and measured in production. The
README section *Throughput and load-balancing keys* lists the keys and the
recommended set.

- **U1 — adaptive smux window** (`HS2_SMUX_AUTOWIN`, `_REL`, `_SHARE_MB`). The
  receiving side grows a heavy stream's window while probes show no queue and
  shrinks it on a queue or a light neighbour; one shared budget. Upload on
  one connection 155 → ~1300 Mbit/s in production.
- **U2 — the download cap found.** Iran turned the adaptive window off for
  every stripeable connection and only turned it back on at stripe width ≥ 2,
  so an un-striped download sat at 2 MiB (~200 Mbit/s at 80 ms).
  `HS2_STRIPE_AW_SOLO=1` (width 1 for 3 s keeps autowin) and
  `HS2_STRIPE_AW_HEAVY=2` (every sub keeps it from the start): download on one
  connection ~190 → 744–1090 Mbit/s at peak hour. Status counters `aw_*`.
- **U3 — load balancer: recover and restart.** `HS2_LB_FAST_RECOVER`
  (3 clean raw readings reset the smoothed loss; first hold 10 s) and
  `HS2_LB_START_GRACE_S` (no whole-IP cut right after start).
- **U4 — load balancer: refill healthy paths.** `HS2_LB_FILL_PCT` lets spares
  past retiring links (slot cap target + N %), `HS2_LB_SPARE_PER_ROUND` asks
  for up to K spares per round. Healthy paths 14/20 → 19–20/20 in 10 min.
- **U5 — load balancer: a cut path comes back.** `HS2_LB_REC_CONFIRM`: a
  single bad reading from a tiny sample no longer resets a cut path's
  recovery clock (2 in a row, or one with ≥ 5000 segments). A path that took
  45 min to return now returns in 30–80 s; a path still bad stays cut.
- **Rejected, kept off:** `HS2_STRIPE_SUB_WIN_MB`, `HS2_STRIPE_SUB_DIV`,
  `HS2_STRIPE_AW_HEAVY=1`, loss-tolerant window growth, a looser stripe gate,
  spare-link bundles, a gradual ramp for a recovered path — no gain or a
  light-user cost.
- **Measured limit.** At peak hour the raw path between the two reference
  servers carries ~2 Gbit/s down; the tunnel reaches ~1.6 Gbit/s (80 %).
  Upload on 8 connections reaches 2.7 Gbit/s.
- **Verification.** `go vet`, the full engine suite (`-timeout 40m`, ~610 s),
  `third_party/smux` and `cmd/hs2` green; an A/A run showed the unified build
  equivalent to the per-side builds it replaced.

## Phase CA — optional ICMP traffic-shape camouflage (`HS2_ICMP_CAMO`)

A field report had an IP carrying the icmp tunnel filtered at the border, the
pattern — not the content — recognised. This phase hardens the icmp flow's
*timing and id* against a stateful classifier, behind an opt-in flag so the
default (and every existing user) is untouched. Set `HS2_ICMP_CAMO=1` on both
ends of an icmp tunnel. It changes only send timing and the id draw, not the
wire format, so it needs no peer agreement and is compatible with an old peer.

- **CA1 — the timing (the biggest cheap tell).** The feedback loop sent a
  control packet every 100 ms unconditionally — a sharp ~10 Hz spectral line,
  and at idle the only thing on the wire. And all carriers base-probe off one
  shared epoch every 4 s — a ~0.25 Hz dip correlated across the pool. With the
  flag: the feedback interval is jittered and an idle carrier (nothing received
  to report) falls quiet, relying on the jittered ~5 s keepalive (well inside
  the 15 s dead-link timeout) — so a near-idle tunnel stops beaconing, the case
  a low-traffic tunnel is most exposed in. The shared probe schedule is jittered
  off its fixed grid by a deterministic per-period offset — every carrier shifts
  the k-th probe alike, so the pool still backs off together (a clean min-RTT
  measurement) but not on a fixed period. Lab: no throughput/ping regression
  (icmp reverse, camo on vs off); idle packet rate roughly halved (the residual
  was a lingering test connection keeping the tunnel active). Honest limit:
  during an active download the receiver→sender direction is mostly feedback,
  so its cadence is jittered, not removed.
- **CA2 — the link id.** `uniqueLinkID` drew uniform-random 16-bit ids, so a
  tunnel showed N unrelated, stable ids to one host — which no real host sends
  to one peer. With the flag, icmp link ids come from one random pid-like base
  with small ascending steps, so they look like a host's own related ping
  processes. The id is only a demux label the peer reads back, so this is
  compatible with an old peer. Honest limit: on a host with a large `pid_max`
  the id *value* is already ~uniform, so the real id tell is count and
  persistence; this clusters them but a tunnel still holds its ids far longer
  than a ping.
- **Evaluated and deferred.** CA3 (hold packets under the field's size
  threshold) is the one discriminator the field proved, but it costs ~6-7x
  throughput and a flood of small packets is itself a new tell under heavy
  traffic, so it is left as a possible emergency low-rate mode, not built.
  CA4 (make the echo sequence a plain counter / couple the two directions so
  a reply echoes a request like a real ping) was investigated in full and not
  built: every variant trades the sequence tell for a worse one — a two-id
  scheme makes request and reply ids mismatch (real ping echoes the id);
  coupling the directions forces silencing the host's own ping and caps the
  upload direction — while the field filter keys on size and volume, not
  sequence correlation (the tunnel ran for a long time with the existing
  sequence mismatch). The honest high-value, low-risk wins were CA1 and CA2.

## Phase Y — the stage rule, re-measured against the field, and fixed where it lost

The Phase X field test (build 37e9f2f9c2e0, on the live reverse icmp tunnel)
found the opposite of the lab: under saturation the pool rules off
(`HS2_FAIR_SHARE=0`) carried 12% more per CPU-second than the default. The lab
reproduced it once the regime matched the field — asymmetric **and** starved.

- **Three lab regimes (same build, sender starved, interleaved pairs).** Under
  a hard CPU quota (symmetric: all the sender's threads stop together) the
  default wins big — 598 vs 375 Mbit/s per CPU-second, and in a profile 291 vs
  103 Mbit/s: the rules-off path wastes CPU sealing datagrams it then drops.
  Under `cpu.shares` with busy loops but hs2 given ~1.1 cores (asymmetric, not
  really starved) the two tie (~530 each). Under shares starving hs2 to ~0.4
  core (asymmetric **and** starved — the field: a co-tenant like an x-ui panel
  holds one core while the scheduler takes CPUs from hs2's threads one at a
  time) the rules off win by ~27% (615 vs 485), reproducing the field.
- **Why.** The machinery is cheap (a profile puts the fair queue, the stage
  bookkeeping and the pacer in the low single digits; the rules-off path even
  spends more on futex/select). The gap is the effect of the rate clamp on
  scheduling, not its cost: a stage-limited carrier's rate is clamped to ~2x
  what it sends, which shrinks the pacer's token bucket. When the scheduler
  puts the send goroutine aside for tens of ms, the 10 ms late credit let it
  drain only a little of the backlog in the slice it got, so it sent under its
  rate. The rules-off path leaves the rate unclamped (startup, `r` up to 23x),
  so its bucket is large and it drains the slice.
- **The fix (Z).** When the host CPU meter says the server is saturated
  (`host_saturated`, plumbed from the status loop to the data path via
  `udpcarrier.SetHostSaturated`), a stage-limited pacer with data waiting holds
  a larger catch-up — 50 ms of its rate instead of 10 ms (`pacerSatCredit`) —
  so it fills the slice the scheduler gives it. Gated on saturation and on the
  carrier being stage-limited (there the CPU, not the path, is the limit, so
  the burst is absorbed without a standing queue); on a host with room, or
  under a symmetric quota (where the meter does not read saturated), pacing is
  unchanged. Interactive packets take the fast lane, so their latency is not
  the cost.
- **Measured (lab, shares starving hs2 to ~0.4 core, 4-5 interleaved pairs
  against the rules off).** The default rose from 485 to ~555 Mbit/s per
  CPU-second median: the ~27% gap narrowed to ~14%, with ping under load
  better than the rules off (p50 31 vs 33 ms, p99 95 vs 116 ms, equal loss).
  A deeper send queue under saturation was tried too and dropped: it moved the
  number by nothing. The residual ~14% is deliberate — closing it fully means
  leaving the rate unclamped like the rules off, which floods a shared path's
  buffer when the CPU frees (Phase W8 / the simulator: hundreds of path drops),
  the behaviour the stage rule exists to prevent for a pool of real users on
  shared paths. Under the hard quota the default still wins (616 vs 378): no
  regression there.
- Two field-report log/wording fixes, source only: the FEC-at-ceiling log
  juxtaposed the governor's pool-wide loss estimate and the worst carrier's
  measured loss as if one bounded the other (it read "loss 39.2% (worst carrier
  27.5%)"), and "below its ceiling" read as if parity had dropped when it only
  meant no carrier was capped — both reworded. The `carriers` legend said
  `C=CPU-bound`; it now says `C=held by its send stage (CPU/socket)`, the
  code's actual meaning.

## Phase X — a sender short of CPU: seen, and sending more with the same CPU

### From the real-server report (build 2b2e2c8c3f86; reverse, tun over icmp)
- The Kharej server (2 cores, shared with another hs2 tunnel) was 96% busy
  and tasks waited for a core 76-87% of the time (the kernel's CPU pressure,
  PSI), while hs2 itself used 52-70% of one core. Its only CPU warning
  compares hs2's own use with 90% of every core, so it never fired, and
  nothing — status, installer, doctor — said the server was saturated.
- In that state no carrier sent 80% of its allowance: all stayed in startup
  (`S`, never `P`), the allowance stayed where startup had left it (18x what
  was sent), and the W3 rules never engaged. The release before Phase W
  does the same; the lab reproduces it with the sender's cores taken.
- The status showed a `pacer` drop counter that nothing ever incremented.

### X1 — the server's CPU and the send stage, visible (no data-path change)
- `hs2 status`'s `cpu:` line shows the whole server next to hs2: busy across
  all cores (softirq and steal when above 1%) and the share of the time
  tasks waited for a core (PSI avg10), with `SATURATED` once it stays at
  90% busy or 40% waiting for three samples; the log says once when that
  starts and once when it ends (once clear for 10 s). `net:` shows the IP
  packets the kernel discarded on output since hs2 started (Ip OutDiscards)
  — for the whole server, and on an icmp listener including the kernel's own
  echo reply to every tunnel packet, which the echo guard drops on purpose:
  shown, not alarmed on. This tunnel's own queue drops over icmp are counted
  exactly instead (X5: `send_refused`).
- `hs2 doctor` gains `server cpu`: the server measured over one second, PSI
  over 10 s and a minute, and what the running hs2 tunnels use in all; a
  warning when it is short of CPU.
- The send stage, over the last couple of seconds: what the pacers sent,
  the share of the time the carriers' writers waited for pacer room, the
  mean wait in the fair queue, and the mean socket write with the datagrams
  per write (`sending:`); the pool's fair share and the queue its busy
  carriers see (`pool:`). Each `carriers:` entry adds what it really sent
  (`s`) and the queue and smoothed round trip its rate control sees
  (`qQUEUE/SRTT`). Lab, the sender's two cores shared with two busy loops:
  `writers waited for pacer room 55% of the time`, every carrier `S` with
  `s` under half of `r`. (Writers wait as long on a full path — 74% behind
  a 30 Mbit/s bottleneck — but there the carriers are `P`, `s` close to
  `r`, with a 15 ms `q`.)
- The dead `pacer_dropped` is gone; `send_refused` counts the datagrams the
  kernel refused on a raw socket (a soft error), which the peer counts as
  path loss.
- The installer's tunnel screen shows the same CPU line and `Net:` line.
- The status file gains `host_cpu_pct`, `host_softirq_pct`,
  `host_steal_pct`, `host_cores`, `psi_cpu10`, `psi_cpu60`,
  `host_saturated`, `host_out_discards`, `send_refused`, `share_mbit`,
  `busy_queue_ms`, `send_mbit`, `send_held_pct`, `fq_wait_ms`, `write_us`
  and `per_write`.

### X3 — a lab sender short of CPU, the same every run
- `lab/cpuquota.sh`: two namespaces, reverse tun over icmp, 4 carriers,
  8 downloads; the Kharej hs2 (the sender) runs under a hard CPU quota
  (`QUOTA` cores, enforced every 10 ms) on its own two CPUs, everything else
  on two others (the receiving veth's work too). Busy loops beside it, as
  before, measured the scheduler's share, which moved with nice and the load
  (the same build gave 92 to 416 Mbit/s); a quota gives the sender the same
  CPU every run, so builds compare on Mbit/s per CPU-second. Prints the
  carriers' flags, the `sending:` line, ping under load and the kernel's
  output discards; `TRACE=1` adds a Go execution trace, `RATE=30mbit` makes
  the path the limit instead.
- Baseline (this phase's monitoring, no data-path change): 0.6 core 134-158
  Mbit/s, 234-281 Mbit/s per CPU-second, every carrier `S` and none `P` in
  every sample — the field regime; 1.2 cores 632 Mbit/s, 557 per
  CPU-second; no quota (2 cores) 696 Mbit/s, still all `S` (nothing else
  limits it), and 923 tunnel packets the kernel discarded on output. A
  starved sender is also a less efficient one: half the CPU, a quarter of
  the throughput. Behind a 30 Mbit/s path the three busy carriers are `P`
  at 9.3-10.1 Mbit/s against a 9.6 fair share, `q` 15 ms, ping p99 23 ms.

### X4 — less CPU per datagram, and no report waits on a writer
- **Sealing in place.** A data datagram was sealed into a new frame (three
  buffers, four copies of the payload: the plaintext frame, the ciphertext,
  the length-prefixed frame, then the carrier's `[seq][ciphertext]`), and
  the length prefix was cut off again. It is now built and encrypted in one
  buffer, in the carrier's wire layout (`core.Session.AppendDatagram`):
  the same bytes on the wire (a test checks them against the old path byte
  for byte, for every padding), 2552 → 948 ns and 4240 → 16 bytes
  allocated per 1300-byte datagram.
- **The FEC encoder's lock.** A carrier's writer holds the encoder's lock
  while its pacer applies backpressure, so the feedback that sets the loss
  estimate — on the carrier's receive loop — and the status that reads the
  encoder's counters — on the pool's loop — waited for the pacer too: under
  a starved sender, for most of the time. The loss estimate, the counters
  and the parity ratio are now read and set without that lock.
- Lab, sender at 0.6 core (`lab/cpuquota.sh`, 8 interleaved pairs against
  the build before): 167 → 195 Mbit/s on average (ahead in 6 of 8 pairs),
  297 → 340 Mbit/s per CPU-second, ping p99 under load 24.5 → 19.6 ms. Every
  carrier still `S`, never `P`: the controller's side of it is X6.

### X5 — icmp carriers no longer queue on one socket lock
- All carriers to one server sent on one shared raw socket, so their writes
  queued twice: on Go's per-socket write lock, then on the kernel's
  `lock_sock` (a raw socket that builds the IP header takes it for each
  send). On a sender short of CPU, a writer the scheduler put aside while
  holding them held every other carrier back (the field's profile: 16% in
  the scheduler, pacers waiting on the descriptor's lock).
- Now each icmp receive socket has a send-only `IPPROTO_RAW` socket beside
  it (dial side and listener). The carriers build the outer IPv4 header —
  from the receive socket's own TTL, TOS and DF policy, so the packet on
  the wire is the one the kernel built before (a test compares the
  kernel's header with the new one, both directions) — and send with
  `sendmmsg` without Go's lock (the kernel's raw `IP_HDRINCL` path takes no
  socket lock either). An `IPPROTO_RAW` socket receives nothing, so the
  per-socket receive cost the shared socket removed does not come back.
  A reply too big for the device without DF still goes out fragmented
  through the shared socket; a datagram the kernel refuses with a soft
  error is dropped and counted (`send_refused`), as before — and so is one
  the device's queue drops, which on a raw socket the kernel reports as
  sent: the send-only socket has `IP_RECVERR`, so `send_refused` counts
  this tunnel's own queue drops exactly. A datagram refused for a reason of
  its own (a firewall's `EPERM`, ...) goes through the shared socket, which
  sends it or reports it as before; only a failure of the socket itself
  turns it off for good, logged once, and its carriers carry on on the
  shared one. A kernel before 6.4 does not know the `IP_PROTOCOL` control
  message (policy routing by protocol): the packets go without it. TTL and
  TOS are read when the socket opens: a later change of
  `net.ipv4.ip_default_ttl`, or a route's hoplimit, is not followed. gre,
  ipip and ipx keep the old path: the kernel's default DF policy for them
  gives each packet a hashed IP id a built header cannot reproduce.
- A udp listener's batch sends drop Go's lock the same way (the kernel's
  udp send path takes no socket lock unless corked).
- `HS2_RAW_TX=0` turns both off; `HS2_RAW_BATCH=0` (one datagram per call)
  does too.
- Measured: the design's lab (busy loops beside the sender) 103 → 141
  Mbit/s median at saturation, 24% less CPU per Mbit/s; the send path alone
  on 4 idle cores 4.8 → 7.0 Gbit/s; no difference under a steady 64 Mbit/s.
  Under a hard CPU quota (`lab/cpuquota.sh`, 0.6 core, 7 interleaved
  pairs) it is neutral — 202 vs 190 Mbit/s, within the noise — with ping
  p99 under load 21.1 → 17.8 ms: a quota stops all of hs2's threads at once,
  so no thread is put aside holding the lock there. The field measurement
  compares it on and off.

### X6 — a carrier its send stage holds back (the rate control)
- **The signal.** A carrier that does not use its allowance (under 80% of
  it) while the pool's send queue for it drops packets, or its writer waits
  for room in its pacer for a quarter of the report or more, is held back
  by its send stage — the CPU, the socket — not by the path. Not under the
  pool's policer cap (its budget holds carriers back on purpose), not in a
  base probe (the rate is turned down on purpose there: a path-bound
  carrier's writer waits all the time), and only with the pool rules on.
- **What it changes.** Startup counts such a report like one that used the
  allowance, and leaves with the capacity estimate at twice what got out
  (not the allowance startup had reached: 2.9-18x what was sent); after
  startup the estimate stays within twice what gets out while the stage
  holds the carrier back; once it lets go (the allowance used, no queue),
  the estimate regrows 25% per report — per round trip on paths over 100
  ms — until a queue stands while the carrier uses its allowance (a queue
  another tunnel's burst puts on the path while this carrier is held back
  says nothing of its send stage, and no longer ends the regrowth). The
  pacer of such a carrier, with data waiting, keeps the credit of up to 10
  ms of a late wake-up instead of losing everything past 2 ms — on a busy
  server the 2 ms cap alone held a pacer to ~0.3-0.5x its rate; every other
  pacer keeps the 2 ms cap. `hs2 status` flags such a carrier `C`.
- **Measured.** Unit tests (startup exit within 7 reports and an allowance
  within 2.1x, the regrowth, nothing changed while the allowance is used,
  under the policer cap, or in a base probe; mutations of the probe rule and
  of the wait signal caught); the pacer at a 6 ms late wake-up sends 0.28 of
  its allowance with the cap and 0.95 with the credit. Simulator (a sender
  of limited CPU, Reno flows, the fair queue and the single writer): no
  carrier left in startup (all, before), the allowance at most 2.3x what
  got out (up to 6.4x), throughput the same while CPU-bound, after the CPU
  frees 0-86 path drops (0-477) and 166-168 against 174-177 Mbit/s on the
  plain scenario. Lab (`lab/cpuquota.sh`, 0.6 core, 5 interleaved pairs
  against the build before): 212 → 270 Mbit/s (ahead in every pair), 371 →
  484 Mbit/s per CPU-second, carriers `C` with an allowance 1.4-1.6x what
  they send; ping p99 under that load 21 → 29 ms — the same CPU now moves
  27% more, and the receive path waits longer for it: at the same
  throughput (157 Mbit/s) ping is no worse (p50 1.5-1.7 against 2.0-3.0
  ms). Behind a 30 Mbit/s path nothing changes (carriers `P`, `q` 15-18 ms,
  the same throughput and ping), and with `HS2_FAIR_SHARE=0` the build
  behaves as the one before.
- Cost: after the CPU frees, a carrier on a path that other traffic fills
  now and then regrows more slowly than with no rule (simulator: 66-89
  against 79-140 Mbit/s, where the rule-less carriers release their stale
  allowance into the path's buffer: up to 477 drops).

### X7 — fixes from the verification (five agents, each under 20 minutes)
- **The send-only socket (X5).** On a kernel before 6.4 (Ubuntu 22.04's
  5.15, Debian 12's 6.1) the `IP_PROTOCOL` control message is refused; when
  several carriers sent their first datagram at once, all but the first to
  notice read that refusal as a broken socket and turned it off for good
  (30 of 200 rounds in a test that makes the kernel refuse it). Each send now
  knows whether its own control data carried the message. A firewall's
  `EPERM` on one datagram turned the socket off for good too; now only a
  failure of the socket itself does. A full device queue's drops, silent on
  a raw socket, are reported (`IP_RECVERR`) and counted.
- **`net:` and the log (X1).** On an icmp listener, the kernel answers every
  tunnel packet with its own echo reply, which the echo guard drops on
  purpose — and counts each in Ip OutDiscards: the status said "tunnel
  packets lost before the wire" and the log said so every minute, for
  nothing (500 tunnel packets in, exactly +500). The count stays in the
  status as the whole server's, explained, and is no longer logged; this
  tunnel's own queue drops are in `send_refused`. The saturation line now
  clears only after 10 s below its marks (one dip used to flap it), and a
  sample that measures nothing changes nothing.
- **The send-stage rule (X6).** A base probe of a carrier that had been
  short of CPU refreshed its `C` flag and late credit every 4 s, long after
  the CPU had freed (on a path that never queues, 189 of 359 reports); the
  probe is now left out before the flag is set, a stage rate unused for 10 s
  is forgotten, and the flag goes off by itself when feedback stops. The
  pacer's late credit reached the data lane only when a batch was cut short:
  the data lane counted as idle, and the credit was cut back to 2 ms on the
  next round — 0.77 of the allowance with batches at 48 Mbit/s, none without
  batches. A pacer held back by its send stage now keeps up to 10 ms of its
  rate in its bucket while data waits (0.97 with batches, 0.95 without;
  on-time pacers never fill it, other carriers keep 2 ms).
- **The status.** `sending:`'s rate is now what the pacers wrote over the
  whole status interval (it was the last report's tenth of a second); the
  `carriers` summary above 32 carriers counts `C`.
- Docs: "30% less CPU per Mbit/s" was 30% more Mbit/s per CPU-second (23%
  less CPU per Mbit/s); the CPU-bound sign is `C`, not `S`; the socket write
  holds no shared lock on icmp any more; VALIDATION's thresholds now match
  `hs2 doctor`, the pool is fixed max first, and V13 measures Phase X.
- Checked and sound: the in-place seal byte for byte (mixed versions both
  ways in the lab), parity sizing under a concurrent `SetLoss`, every
  encapsulation direct and reverse, a two-address icmp listener, the header
  equivalence and the fragmenting fallback, `HS2_FAIR_SHARE=0` and
  `HS2_RAW_TX=0`; the installer's lines (shellcheck, both copies identical).

### X2 — documentation brought in line with the code
- The rate controller's description (`udpcarrier/rate.go`) named a
  `highQueue` hold band and 25%/6% growth that no longer exist: it now says
  what the code does (a proportional queue term around 10 ms; a capacity
  probe of 4% per report, per round trip past 100 ms; startup frozen on
  reports that do not use the allowance — until X6's rule, for a sender
  short of CPU). Phase W's numbers above corrected
  to the current simulator; README's status fields and the `carriers` line
  format; VALIDATION's rollback target, V11 and V12.

### D1 — the server doctor (`hs2 doctor --deep`, read-only)
- `hs2 doctor --deep [-c config] [--lang fa]` inspects the whole server in
  about 3–10 s and prints one finding per check: severity (OK / WARN / BAD,
  INFO, SKIP), what was seen, why it matters and what to do; `--lang fa`
  prints it in Persian. With `-c` it runs after the usual checklist. It only
  reads (/proc, /sys, the status files, journalctl, ≤10 pings per path) and
  never changes anything; a missing tool or file is a SKIP with a note. Exit
  1 when a BAD finding (or a doctor FAIL) is seen.
- Checks, from problems found in production: **NIC & softirq** (rx queues,
  which core the NIC's rx interrupts land on over 3 s, that core's softirq
  and idle share, the RPS mask, and whether hs2/xray still run on that core —
  advice: CPUAffinity without it); **CPU pressure** (PSI some avg10/avg60,
  top processes, WARN above 15%); **path quality** (a plain ICMP ping,
  source-bound per LB path, compared with the tunnel links' RTT and loss —
  "the IP queues and loses even without the tunnel → network/provider, not
  hs2", or "the bare path is clean, the delay is added on the servers"); a
  **whole-network congestion** hint when every path pings far above its
  lowest RTT; **Reality dest** (xray/x-ui config: a dest on a multi-tenant
  CDN, how many of xray's connections go to it, limitFallback advice; never
  prints keys or ids); **x-ui churn** ("Remove Inbound User" and node-sync
  errors in the last hour); **hs2 health** (links serving/target, stale
  status, LB paths cut or lowered and why, restarts, panics in the last
  hour).
- Installer: the tunnel manager's **Diagnose** runs the deep doctor (an older
  binary falls back to the plain one), and the main menu gains
  **9) Server doctor**; both ask English/Persian once (or `HS2_LANG=fa|en`).

## Verification, every phase

- Go: `go test ./...` and `go test -race ./...`.
- Bash: sourced-core unit tests + pty tests for the interactive prompts
  (including `tm_edit_test.sh`, which drives the menu under the real shell
  options).
- Probe behaviour is checked differentially against a real net/http HTTPS
  server, not against our own expectation of it.
- `hs2-src/install/tests/release_files_test.sh`: the two `install.sh` copies are
  byte-identical and the published sha256 files match their targets.
- `shellcheck -S warning` clean on `install.sh`.
- An independent adversarial review per phase; confirmed findings fixed before
  shipping.
