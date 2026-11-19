# hs2 rel69q — release notes

- Tag `v-rel69q`. Binary `hs2-linux-amd64`, sha256
  `13a120a433b91f716595c3005975f262f4705a4e7644ed469301e791bab6f5ed`, `hs2 version` shows `[build rel69q-rc2]`.
  These are the exact bytes the reference production tunnel 1 runs since 2026-11-19 13:06 UTC (tunnel 2 since
  16:07 UTC), published unchanged for traceability, so the stamp keeps the release-candidate name. Static,
  reproducible build (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.buildTag=rel69q-rc2" ./cmd/hs2`,
  go1.27.3) from an LF `git archive` of the rel69q source commit.
- Previous GitHub release: **rel69p** (tag `v-rel69p`, binary sha256 `c0e04036…`). The production defaults table
  (`hs2-src/envdef/envdef.go`) grew from 132 to 135 keys.
- `install.sh`: the rel69p installer plus the watchdog menu, `fix_perms` on upgrade and the join code on fd 3
  (sha256 in `install.sh.sha256`; the root `install.sh` and `hs2-src/install/install.sh` are the same file).
- **Config files: no change needed.** File modes under `/etc/hs2`, `/var/lib/hs2` and `/run/hs2` are tightened to
  owner-only by `bash install.sh upgrade` (`HS2_KEEP_PERMS=1` skips it; see below for a local non-root reader).
- The public `hs2-src/` carries no lab harnesses or lab results; only `hs2-src/lab/netsim` stays, because Go tests
  import it.

## What changed since rel69p

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
