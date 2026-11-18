# hs2 rel69m — release notes

- Tag `v-rel69m`. Binary `hs2-linux-amd64`, sha256
  `5e8fc8bc562ec1e9f525db0888b1c6830ab9e870f7be763e24b4dd4038fd82a2`, `hs2 version` shows `[build rel69m]`.
  Static build (`CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.buildTag=rel69m"`, go1.27.3) from a `git archive`
  of the rel69m source commit. This is the exact binary both reference production tunnels run since 2026-11-18.
- Previous GitHub release: **rel69k** (tag `v-rel69k`, binary sha256 `eb3373ff…`). rel69l was built and run in the lab
  but never published, so these notes cover **rel69k → rel69m**. The production defaults table
  (`hs2-src/envdef/envdef.go`) grew from 105 to 111 keys.
- `install.sh` is new: it brings the live load-balancing screen and the `hs2-menu-lb` wrapper (sha256 in
  `install.sh.sha256`; the root `install.sh` and `hs2-src/install/install.sh` are the same file).
- **Config files: no change needed.** The config keys are the same as rel69k.

## Wire compatibility

No capability bit and no wire change in rel69l or rel69m: every change is local to one side. rel69m talks to rel69k
(and older peers rel69k talks to) exactly as rel69k does. Still upgrade the kharej (exit) first and the Iran side soon
after, so both sides run the same defaults.

## What changed since rel69k

"Lab" means the production-like multi-path lab (a crowd of light users plus heavy flows, real hs2 on both sides).

### Paths that stop carrying data (rel69l)
- **Data-dark rescue** (ul268, Iran, `HS2_UL268_RESCUE`). A path can stay "up" while its data is frozen (TCP sends sit
  unacked, or in RTO backoff). Iran now watches this per link (TCP_INFO) and, while another path still acks, moves the
  light connections and upload stripes off the frozen link instead of waiting for them to time out. A fast upload's
  ring grows to half a second of its rate (bounded per connection and per process) so it can be moved without loss.
  Calls are left to the existing dark-call handling. Lab (path data cuts): crowd connection failures −62 per round,
  ping p99 2–5 ms lower with no fault, call tail unchanged.
- **Ping/ICMP no longer stalls on a stuck link** (b278, both, `HS2_B278_L3STALL`). Small interactive TUN packets
  (ping, games) leave a link that has data in flight with no ack progress, and the duplicate copy (`HS2_L3_DUP`) never
  goes to such a link or to the first copy's path. Lab data cuts: the 5–9 s ping gaps seen before did not recur (0 stall gaps in 9 fault runs vs 1 in 10 with it off).
- **A call's two copies are never on the same path** (cs286, both, `HS2_CS286_CALLAPART`). When a call's copy sub would
  ride the same path as its other sub it is re-picked on another healthy path (then a lowered one, then a degraded
  one). Before, a late LB cut could leave both copies on one path, and cutting that path stalled the call for 11 s.

### Uploads (rel69l)
- **New fast uploads may use an extra stripe slot** (ul281, Iran, `HS2_UL281_SLOT`, now on by default). When all 8
  stripe slots are held, a new fast upload may take one of the 4 shared extra slots downloads already use (bounded at
  12 per process) instead of staying on its first link. Lab, 10 off/on pairs: single upload 229 → 327 Mbit/s
  (10/10 better), light-user p99 unchanged, 0 failures.

### Fix (rel69l)
- **Exit connection counter crept up** (zs291, both, no switch). A connection that never got a sub was never ended, so
  the exit's `mem: mconns` counter rose at each data-dark event. Accounting only (memory was freed), now fixed.

### Scale and memory (rel69m)
- **Fewer allocations and GC cycles** (gc295, both, no switch): the smux shaper heap, request contexts, writer batch
  lists, batch buffers and TCP_INFO reads no longer allocate per packet. Lab 1 h parallel soak vs rel69l: allocation
  Iran −30 %, kharej −63 %; GC cycles per minute Iran −27 %, kharej −55 %; heap/RSS, goroutines and fds flat; latency
  equal or better in every 10-min window; 0 panics.
- **Rescue ring pool** (gc296, both, `HS2_GC296_RINGPOOL`): rescue ring buffers are reused from a pool per size class
  (4 KB–8 MB) instead of allocated each time; every byte still counts against the existing rescue memory budgets.
  Lab storm pairs: allocation −26…−50 %, kharej GC cycles −32 %; Iran RSS about +137 MB (bounded), latency within noise.
- **Scale fixes** (b296, both): the dark-call timer skips connections proven idle (`HS2_B296_DCSKIP`; calls are never
  skipped and every 5 s all connections are visited; 5–6 % CPU at 2x load), and the exit's traffic stats no longer
  hold the table lock while they scan, so new exit streams do not wait behind it. Lab at 1x and 1.5x load: call and
  crowd p99 equal or better, 0 failures, 0 panics.

### Live load-balancing screen (installer)
- The classic "Load balancing — live status" table is replaced by a live screen: one row per Iran IP sorted by
  traffic, with share, links live/target, down/up Mbit/s, streams, **loss** (the worse of down and up; `N↑` marks
  upload loss), RTT, **path MTU** (the lower direction; red under 1280) and the **automatic cuts** of the last 24 h.
  Cut or idle IPs sit on one dim line, then the Kharej IPs and the LB verdicts. Keys: `p` per-path table, `m` matrix,
  `k` Kharej IPs, `h` help, Enter back. Narrow terminals drop the least important columns; no-colour/ASCII terminals work.
- Open it from the new menu (**3) Load balancing & IPs → l) Live view**), from the classic LB screen (9), or from a
  shell: `hs2-menu lb [tunnel] [--once] [--view V]`, or simply **`hs2-menu-lb`** (install/upgrade write
  `/usr/local/bin/hs2-menu-lb`; uninstall removes it).
- Read-only: it shows the same numbers as `hs2 lb show --status` and changes nothing.

### Switches (all on by default)

| change | side | turn off / old behaviour |
|---|---|---|
| ul268 data-dark rescue | Iran | `HS2_UL268_RESCUE=0` |
| b278 TUN interactive flows off stuck links | both | `HS2_B278_L3STALL=0` |
| ul281 extra stripe slot for new fast uploads | Iran | `HS2_UL281_SLOT=0` |
| cs286 call copy on another path | both | `HS2_CS286_CALLAPART=0` |
| zs291 mconn counter fix | both | no switch (fix) |
| b296 dark-call idle skip | both | `HS2_B296_DCSKIP=0` |
| gc296 rescue ring pool | both | `HS2_GC296_RINGPOOL=0` |
| gc295 allocation cuts, b296 exit-stats scan | both | no switch (roll back the binary) |
| live LB screen, `hs2-menu-lb` | installer | `hs2 lb show --status` still works |

## Measured on production (honest caveats)

The reference tunnel moved rel69k → rel69l → rel69m; rel69m runs on both sides of tunnel 1 since 2026-11-18 00:29 UTC
and on tunnel 2 since 01:00 UTC, 0 panics. Production speed swings by the hour, so a few tests before and after are not
an A/B: tunnel download/upload 702/865 Mbit/s just before, 742/834 after (3-round average); light-user ping 83/89 ms
before, 82/85 after. rel69m is therefore **not** shown faster on production; its gains are lower CPU/GC cost at high
user counts and behaviour under path faults, which production only meets during real cuts.

## Defaults

- On reverse `l3mtcp` the whole production table (111 keys) is on by default; the README table "Production defaults"
  lists them row by row. An explicit `Environment=HS2_X=…` always wins, and `HS2_DEFAULTS=legacy` turns the whole
  table off.
- Every other carrier and direct mode keeps the old defaults (off).

## Rollback

Switches (take effect after a restart, no binary change):
- rel69m's own: `Environment=HS2_B296_DCSKIP=0 HS2_GC296_RINGPOOL=0` (both sides). gc295 and the exit-stats change
  have no switch; for those roll back the binary.
- rel69l's: `HS2_UL268_RESCUE=0` and `HS2_UL281_SLOT=0` (Iran), `HS2_B278_L3STALL=0` and `HS2_CS286_CALLAPART=0`
  (both sides).
- Older steps: `RELEASE-rel69k.md` and README "Restore the old behaviour". Everything off: `HS2_DEFAULTS=legacy`.

Binary rollback: `cd /root/hs2 && bash install.sh restore` restores the newest backup the upgrade made in
`/root/hs2-backups/`. To go back to rel69k, pin its commit:
`HS2_REPO_RAW=https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/189eb7d9160fec008ad0d619a4d563efefd32fbd bash install.sh upgrade`.

## Upgrade steps

1. **Kharej first:**
   `curl -fsSL https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/main/install.sh | bash -s upgrade`.
   It saves a backup, downloads the binary, checks it against `hs2-linux-amd64.sha256` (fail-closed), restarts every
   tunnel, checks that the running binary is the new one and writes `/usr/local/bin/hs2-menu-lb`.
2. **Iran:** if the server can reach GitHub, run the same command. Otherwise copy the binary, its `.sha256` and
   `install.sh` from kharej into `/root/hs2/`, then `cd /root/hs2 && bash install.sh upgrade` (the hash is verified
   offline).
3. Check: `hs2 version` shows `[build rel69m]` on both sides; the log shows `defaults: production set on …`;
   `hs2 doctor --deep` reports 0 BAD; `hs2-menu-lb --once` prints one frame of the live screen.
4. Upgrade steps 5–6 of `RELEASE-rel69k.md` (several tunnels on one server, hand-managed units) still apply.

## Known issues

1. **The shaper is per device, not per tunnel** (see `RELEASE-rel69k.md` upgrade step 5).
2. **Short call stalls on one-direction path cuts remain** on every build tested, rel69m included (a few of ~50 cuts).
3. **Memory:** the rescue ring pool keeps up to about +140 MB more RSS on a busy Iran server (bounded by the existing
   rescue budgets); ul268 may hold up to min(256 MB, RAM/16) of upload ring per process.
4. The light-user latency cost of queue control from rel69k is unchanged (about +3 ms crowd p99 steady).
5. A target's own reset on the one-stream path and on raw lite connections still reaches the user as a FIN.
6. Test flakes under load, not code bugs: `TestWildcardListenerRepliesFromTargetedIP` (udpcarrier read timeout),
   `TestMConnLitePingsDualAndStall` (timing, on a loaded box only).
