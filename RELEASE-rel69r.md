# hs2 rel69r — release notes

- Tag `v-rel69r`. Binary `hs2-linux-amd64`, sha256
  `58982b229788aeb5e9211a1c9dd89219c75ec0dd2b29f3fe5bbce8407709f102`, `hs2 version` shows `[build rel69r-rc7]`
  (the exact production bytes, published unchanged, so the stamp keeps the release-candidate name).
- Previous GitHub release: **rel69q** (tag `v-rel69q`, binary sha256 `13a120a4…`).
- `install.sh`: the rel69q installer plus two lines (tunnel delete / uninstall also remove `<config>.peer-ips`).
  Menu, live load-balancing screen and Alerts screens unchanged. sha256 in `install.sh.sha256`.
- **Config files: no change needed.** Upgrade as usual (`bash install.sh upgrade`, Kharej first, then Iran).
- **New and optional: zero-downtime upgrade.** Off until you turn it on per tunnel; see README → *Zero-downtime
  upgrade (ZDU)*. Turning it on costs one normal restart; every later upgrade is then done without cutting users
  (refused HH:00–HH:03 UTC; automatic revert within 45 s; rollback steps in the README).

## For the owner, in short

- **Upgrades without cutting users** (ZDU, opt-in): new users move to the new version at once; connected users
  finish on the old one.
- **Censorship detector** on the Iran server: it only writes what it thinks is happening to the log
  (`censor351:` lines, `hs2 status --censor`). It changes nothing. Off with `HS2_CENSOR_DETECT=0`.
- **Fewer hanging new connections** when a path goes silent (STALLPICK, on).
- **The other server is no longer throttled by the reality front right after a restart**: its IPs are saved
  (`<config>.peer-ips`) and remembered across restarts.
- **auto6** now uses only the IPv6 of the default-route interface (a WARP / WireGuard address is never taken).
- **No duplicate packets during a handover** between rel69r servers.

## What changed since rel69q

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

