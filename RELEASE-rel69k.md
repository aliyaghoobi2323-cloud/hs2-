# hs2 rel69k — release notes

- Tag `v-rel69k`. Binary `hs2-linux-amd64`, sha256
  `eb3373ffc18ab159cf18b34c7403e0e180b4a7e0da97dced14a39b2f600a403f`, `hs2 version` shows `[build rel69k]`.
  Static build (`CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.buildTag=rel69k"`, go1.27.3) from a `git archive`
  of the release source commit. This is the exact binary the reference production tunnels run since 2026-11-17.
- Previous GitHub release: **rel54c** (tag `v-rel54c`, binary sha256 `19f63b6b…`). rel58 … rel69j were built and run
  on the reference tunnel but never published, so these notes cover **rel54c → rel69k**. The production defaults
  table (`hs2-src/envdef/envdef.go`) grew from 50 to 105 keys.
- `install.sh` is unchanged since rel69j's tree (sha256 in `install.sh.sha256`). `hs2-source.zip` was removed from
  the repo root: it was stale (2026-09) and nothing used it. The source is the `hs2-src/` tree of the tag.
- **Config files: no change needed.** The config keys are the same as rel54c, and `hs2 check` accepts a config written
  by rel54c.

## Wire compatibility

Every change is capability-negotiated or local to one side.
- rel54c ↔ rel69j was tested in both directions in the lab (two-tunnel upgrade rehearsal, Reality front, traffic at
  every step).
- rel69k adds one record, XS (kind 0x15). It is sent only on a link whose peer announces capability bit 14, so rel69j,
  rel54c and older peers never receive it. The exit's new missed-join reply is the base RST record that every mconn
  peer since rel54c understands.
- rel69k ↔ rel54c was **not** run as a separate lab pair. Upgrade the kharej (exit) first and the Iran side soon after,
  so both run the same defaults.

## What changed since rel54c (by theme)

"Lab" means the production-like multi-path lab (crowd of light users plus heavy flows, real hs2 on both sides).
Most lab A/Bs are n = 2–7 runs per arm, so a few ms of p99 are inside the noise.

### Downloads and uploads (heavy flows)
- **Faster single downloads.** Stripes start from the first 200 ms samples (DL_FAST); the fast slot is a lease
  (DL_FAIR); downloads may use links that carry lite copies (DL_COPIES, rank mode DL_PRIM=2); windows are kept at
  ≥ 4 MiB once a download proves heavy (HEAVY_FLOOR). Lab: d1 455 → 761 Mbit/s (DL_FAST), a download among 3–10
  background users 427 → 752 (DL_FAIR), d8 +34 % and d1 +21 % (floor).
- **Faster single uploads.** The upload stripe gate grows on the median and cuts on the latest sample (gate74, lab +31 %).
  Uploads share copy links. A raw upload stuck behind its first sub opens a second stripe sub at once (ul257,
  `HS2_UL257_STUCK`, Iran).
- **A heavy stream survives a dark path.** Unacked chunks are kept and resent off a dark sub; a young sub's first hole
  is re-sent on an older sub (`HS2_DL243_RESCUE`, exit).
- **Queue control** (both sides): while a heavy flow keeps its path's queue big, this server caps its own egress on
  that path, so light users are not queued behind it. It also keeps an idle cap at the measured rate, caps a new burst
  at its onset, and re-inits a collapsed BBR estimate (`HS2_LINK_CC_REFRESH`). Lab: DL median 87/95 → 539/493 Mbit/s,
  crowd failures halved. Cost: crowd p99 +3.4 ms steady and +20–30 ms in storm seconds. The tc shaper (htb + cake) sits
  on the last child of a multi-queue NIC's `mq` root (`HS2_QUEUE_CTL_SHAPER=mqchild`). A 1-queue NIC, a kernel older
  than 5.18 or a missing `act_skbedit` falls back to root mode, and the log says so.
- **dl259 per-path download queue gate** (`HS2_DL259_PATHQ`) is in the code but **off by default**. The lab showed big
  single-download gains with it on, but a same-hour production A/B halved single-stream download (median ~474 vs
  ~1128 Mbit/s with it off). `=1` turns it on; do not.

### Light users, calls, games
- **Rescue of light connections on a silent path.** Blackhole lab: failed connections −74 %. Cost: kharej +~80 MB in
  bounded pools.
- **Dark-call handling.** A call whose path goes dark moves to a live sub on another path and resends only what the
  peer lacks. dc254 (rel69j) also catches a primary whose sends sit frozen in RTO backoff while its link still
  receives.
- **rel69k call fixes** (details in the rel69k table below):
  - xs261: a one-direction cut seen by one side now moves both sides' call primaries (cross-side switch).
  - b262: a darkcall switch never leaves a chunk's only copy on a dark sub. Lab one-direction cuts: call ≥ 1 s samples
    −76…−90 % vs rel69j; crowd p99 and game unchanged within noise.
  - b270: call copies stranded on a half-dead sub are resent on the live sub. Lab data-dark cuts: runs with a 3–11 s
    call stall went from 4/19 (rel69j-based) and 3/7 (rel69j) to 0/22, including a 20 s data cut (rel69j: 19.5 s
    stall) and a full path blackout.
  - b274: a call left with one live sub re-opens its second at once (engaged in the lab when a blackout killed a sub).
- **Path judging and LB**: blackholed paths are read as 100 % loss and cut in ~7–12 s, then probed back 2–8 s after
  they clear. A cut path gets no new connections (flap lab 0 vs 73–111 failed connections). Light users and stripes
  move off cut paths.
- **Connection storms** (e.g. an hourly panel restart): new connections are placed under the pool's read lock, and the
  edge paces lite second-sub upgrades (`HS2_ST244_UPG_RATE=auto`). Storm lab: crowd p99 in the storm minute
  457 → 377 ms, storm open p99 2027 → 1518 ms.
- **Exit restart** (rel69k, jm271): the exit answers a join for a connection it no longer knows with a reset, so the
  Iran side ends it at once. Lab: after an exit-only restart, 70–700 of 1000 long-lived connections hung for 85 s+ and
  Iran re-joined dead connections ~300/s forever; with rel69k 0 hung, all ended within p50 2 s.
- Idle user connections (SSH, push, IM) stay alive across pool shrinks.

### Correctness fixes
- Two data-corruption fixes (smux buffer ownership; a raw reader that read past its boundary).
- A cut upload or download now ends with a **TCP reset**, never a clean FIN (no more silent truncation). A lost short
  upload is fixed. New status counters `mconn_target_rst`, `mconn_user_rst`, `stream_rst`.
- A heavy stream could stall forever on an out-of-order smux window update (fixed).
- TCP half-close works through the tunnel (`HS2_HALF_CLOSE`); the half-open cap is 16384, over it a relay ends with a
  reset.
- Memory fixes; the records ring budget is sized from RAM; the queue-ctl shaper heals itself; every tc/ip call has a
  timeout.
- rel69k: data races found by `go test -race` fixed (feature switches read as atomics); no behaviour change. Full
  `go test ./...` and `go test -race ./engine` pass with 0 DATA RACE.
- Logs (rel69k): dc254 "silent rx" split into link evidence vs app silence; "no fresh target" split into lost vs
  survived; retire/back-in-service lines collapse into one summary per minute.
- Per-path MTU in `status`, `lb show` and `doctor`.

### rel69k table (on top of rel69j)

| change | side | turn off / old behaviour |
|---|---|---|
| xs261 cross-side call switch (+ cs224 re-chosen sub fix) | both | `HS2_XS261=0` |
| b262 darkcall switch coverage, darkwait (bounded 500 ms) | both | `HS2_B262_COVER=0`; `HS2_B262_WAIT_MS=0` = unbounded |
| b262 stale call-sub replace (prototype) | Iran | **off** by default; `HS2_B262_REPLACE=1` on |
| b270 resend of call copies stranded on a half-dead sub | both | `HS2_B270_TXRES=0` |
| b274 one-sub call repair | Iran | `HS2_B274_REPAIR=0` |
| jm271 reset on a missed join | kharej | `HS2_JM271_RST=0` |
| dl259 per-path download gate | kharej | **off** by default (was on in rel69j builds) |
| b271 log changes, b276–b278 race fixes | both | no switch (logs/tests only) |

## Measured on production (honest caveats)

The reference tunnel moved rel69j → rel69k on 2026-11-17 ~12:39 UTC. Same-day single tests (production speed swings
by the hour, so a few tests before/after are not an A/B):
- before (rel69j): tunnel single-stream download 1422 Mbit/s, light-user ping p50 79 ms;
- after (rel69k): tunnel download 1137 / 1037 / 1287 Mbit/s, upload 385 / 543 / 1073, light ping p50 78–79 ms;
- 0 panics on both sides; the second tunnel on the same servers moved at 13:17 UTC, also 0 panics.

So rel69k is **not** shown faster on production downloads; its gains are call/stall and exit-restart behaviour under
path faults, which production only meets during real cuts. The lab call-stall gains above come from n = 6–22 fault
runs per arm; the one-direction-cut results are within noise between several arms.

## Defaults

- On reverse `l3mtcp` the whole production table (105 keys) is on by default; the README table "Production defaults"
  lists them row by row. An explicit `Environment=HS2_X=…` always wins, and `HS2_DEFAULTS=legacy` turns the whole
  table off.
- Every other carrier and direct mode keeps the old defaults (off).
- At start the log prints `defaults: production set on (<side> side, N keys from defaults; …)`.
- Opt-in only (off unless set): `HS2_DL259_PATHQ`, `HS2_B262_REPLACE`, `HS2_MCONN_RESCUE_RST`, `HS2_LB_UPLOSS`,
  `HS2_SMUX_SENDBATCH`, `HS2_MCONN_STRIPE_DL_SHARE`/`_HEADROOM`, `HS2_MCONN_STRIPE_UPGATE_HYST`,
  `HS2_MCONN_STRIPE_JUMP_MS`.

## Rollback

Switches (take effect after a restart, no binary change):
- rel69k's own changes, all together:
  `Environment=HS2_XS261=0 HS2_B262_COVER=0 HS2_B270_TXRES=0 HS2_B274_REPAIR=0 HS2_JM271_RST=0`.
- rel69j's: `HS2_UL257_STUCK=0` (Iran), `HS2_DC254=0` and `HS2_HALF_CLOSE_MAX=4096` (both).
- rel69h: `HS2_ST244_UPG_RATE=0` (Iran). rel69f/g: `HS2_DL243_RESCUE=0` (kharej). rel69e:
  `HS2_QUEUE_CTL_SHAPER=root` (both).
- Queue control off completely: `HS2_QUEUE_CTL=0` (kharej) and `HS2_QUEUE_CTL_UP=0` (Iran).
- Older steps: README "Restore the old behaviour". Everything off: `HS2_DEFAULTS=legacy`.

Binary rollback: `cd /root/hs2 && bash install.sh restore` restores the newest backup the upgrade made in
`/root/hs2-backups/`. To go back to rel54c, pin its commit:
`HS2_REPO_RAW=https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/5aebf590a31dc8abaa1ae8b128c0182e7324444a bash install.sh upgrade`.

## Upgrade steps

1. **Kharej first:**
   `curl -fsSL https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/main/install.sh | bash -s upgrade`.
   It saves a backup, downloads the binary, checks it against `hs2-linux-amd64.sha256` (fail-closed), restarts every
   tunnel and checks that the running binary is the new one.
2. **Iran:** if the server can reach GitHub, run the same command. Otherwise copy the binary and its `.sha256` from
   kharej into `/root/hs2/`, then `cd /root/hs2 && bash install.sh upgrade` (the hash is verified offline).
3. Check: `hs2 version` shows `[build rel69k]` on both sides. The log shows `defaults: production set on …`, and on a
   multi-queue NIC the shaper line (`… mq child` or the root-mode fallback). `hs2 doctor --deep` reports 0 BAD.
4. First start on a multi-queue Iran NIC re-adds the kernel's default `mq` once as `mq 1fe0:` while the tunnel is idle.
   Do the first start in a light hour, not during a panel restart storm.
5. **Two or more reverse `l3mtcp` tunnels on one server:** keep the queue-ctl shaper on one tunnel only. On the others
   add a drop-in on both servers (`[Service]` / `Environment=HS2_QUEUE_CTL=0` / `Environment=HS2_QUEUE_CTL_UP=0`),
   then `systemctl daemon-reload` and restart that unit.
6. **Hand-managed units** (an `ExecStart=` drop-in that runs another binary): `upgrade` repoints such drop-ins to
   `/usr/local/bin/hs2` and keeps the old file as `*.pre-upgrade`. If you want to keep your own layout, swap the
   binary by hand instead.

## Known issues

1. **The shaper is per device, not per tunnel** (see upgrade step 5). The installer does not add that drop-in itself.
2. **Short call stalls on one-direction path cuts remain.** Lab: about 2–6 of 42–56 one-direction cuts still give a
   ≥ 1 s call stall on every build tested, rel69k included.
3. **One-way data blackout plus RTO backoff can stall an upload at 0 Mbit/s for seconds.** The fix (ul268) is in
   testing and is **not** in rel69k.
4. On a one-direction cut b270 may resend a half-dead sub's whole sent history in front of the live sub's data
   (lab: up to ~1300 chunks in one resend, no stall seen). `HS2_B270_RECENT_MS` limits it but stays at its unbounded
   default until a data-dark lab check proves a smaller window keeps the gain.
5. **Light-user latency cost of queue control:** about +3 ms crowd p99 steady and +20–30 ms in storm seconds, and
   +0.4–0.6 ms icmp p50 with more paths in use.
6. **Memory:** lab RSS Iran ~0.5–1.6 GB and kharej ~0.9–2.7 GB depending on load (crowd 1000–3000). Rescue ring
   budgets grow up to 4 % of RAM; the records big-ring budget is sized from RAM (up to 1 GB).
7. A target's own reset on the one-stream path and on raw lite connections still reaches the user as a FIN (smux has
   no reset frame).
8. Test flakes under load, not code bugs: `TestWildcardListenerRepliesFromTargetedIP` (udpcarrier read timeout).
