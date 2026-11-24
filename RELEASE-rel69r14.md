# hs2 rel69r-rc14 — release notes

- Binary `hs2-linux-amd64`, sha256
  `2f3d34a2ce6f008c8c49f89f89251229d707be981174949c7ffdc823921610a3`, `hs2 version` shows `[build rel69r-rc14]`
  (the exact production bytes, published unchanged, so the stamp keeps the release-candidate name).
- Previous GitHub release: **rel69r** (tag `v-rel69r`, binary sha256 `58982b22…`, `[build rel69r-rc7]`).
- `install.sh`: the rel69r installer plus the b430 certificate-renewal hook fix (below). Menu, live
  load-balancing screen, Alerts and watchdog screens unchanged. sha256 in `install.sh.sha256`.
- **Config files: no change needed.** Upgrade as usual (`bash install.sh upgrade`, Kharej first, then Iran), or
  with a zero-downtime handover if you turned ZDU on (README → *Zero-downtime upgrade*).
- No wire change that needs both sides at once: the new hand-off messages (LBSTATE, UNUSED) are only used when both
  processes announce them, and the heavy-download fixes work against an rel69r peer (they help most when both
  sides run rel69r-rc14).

## For the owner, in short

- **Heavy downloads no longer hang or reset when a link dies under them** (b449: stuck primary, dark open, slow
  open, target wait, keep floor, and UPGSAFE for a sub that dies during the raw -> records handshake). Lab under
  link kills and blackholes: lost connections on the Iran side 193 -> 10 a run.
- **A zero-downtime upgrade no longer gives everybody a ping spike when the old process exits.** The old Kharej
  process now ends its users one at a time at a fixed rate before it exits (DRAINFAST + STEP_MS), the Iran side
  limits how many re-opens it places at once (PICKCAP=2, CAPLAT keeps calls and games ahead of the queue) and holds
  its garbage collector back during the storm (DRAINSMOOTH). Lab at 12k users: ping spike after the exit
  400..2000 ms -> 60..190 ms, call loss 0.7..1.7 % -> ~0.07 %, Iran CPU at the exit 9 -> 4 cores.
- **Mass closes do not freeze the tunnel any more** (close pacing, rescue placement on the fast path): thousands of
  connections ending in one instant (the hourly xray restart, a client network drop) no longer stall ping for
  ~0.4 s.
- **Boot with a late IPv6 address**: a listen address that is not up yet (IPv6 DAD, DHCP) no longer stops the
  tunnel at boot (freebind).
- **Renewed certificates are loaded right away** again, also when the binary has a versioned name.
- Steady-state quality (calls, games, web, downloads) is unchanged: no column proven worse in the lab.

## What changed since rel69r

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

## Lab results (production-like harness, rel69r-rc14 vs the previous production build rel69r-rc12)

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
