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
