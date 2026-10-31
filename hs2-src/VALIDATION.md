# Real-server validation — stuck links and the loss rule (Q7, Q8), tun over icmp (V), CPU and the shared bottleneck (W, X), the user-port path (Z, Z2, Z3), one fast connection (Z5 step 1), the redesign's phase R1

This is the checklist for validating the releases from Q7 on (see CHANGELOG:
"Q7 — stuck links", "Q8 — the health rules at high bandwidth", Phases V, W,
X, Z–Z3 and Z5 step 1) on a real Iran/Kharej server pair with real users.
The load rig showed the rules work and do not cut users needlessly in the
scenarios it can make; what it cannot make is Iran's real DPI, real evening
congestion and real user traffic. Both servers must run the release build
(`hs2 version` shows the build id below).

- Release commit: see the "Release" line at the end of this file.
- Previous release (rollback target): binary build `ef2dffdbbb87` (Phases Z,
  Z2 and Z3; built from `ef2dffd`, the binary carried by commit `c9825f2` on
  the work branch `claude/smux-buffer-constants-fv9bu8`) — the build your
  servers may run now. Before it `0812bc9`, main until this release (binary build
  `da621f5db2bb`, Phases Y and CA), and `9b74b3c` (binary build
  `2b2e2c8c3f86`, Phase W).

## Rollback

For this release's own parts, try their switches first: they need no
reinstall (V16, "Switching a part off"). To go back to the previous binary,
on each server reinstall it pinned to the commit that carries it (no change to
the repository needed):

```
HS2_REPO_RAW=https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/c9825f2 bash <(curl -fsSL https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/c9825f2/install.sh)
```

then Upgrade the tunnel(s) in the menu (`hs2 version` must then show `build
ef2dffdbbb87`; pin to `ef2dffd` itself and you get the older binary that
commit still carried). To go back past Phases Z–Z3 as well, use `0812bc9` in
both places (binary build `da621f5db2bb`). To roll `main` itself back, revert
the release commits on main (do not force-push).

Roll back at once if any of these happens: users report being cut in waves;
more than a handful of `stuck:` lines an hour that no real throttle explains;
a burst of `stuck:` lines at the same second (more than an eighth of the
pool); `degraded (up-loss` lines by the dozen right after a congestion spell;
`hs2 doctor` FAILs that the old build did not have; any of V16's reasons that
remain with both of its switches off.

## What to watch

Find the service units with `systemctl list-units 'hs2*'`, then follow the
Iran (edge) side, where all of this is decided:

```
journalctl -u <unit> -f -o cat | grep -E 'stuck|answer promptly|degraded|closed with its|path is lossy'
```

| Log line | Meaning | Expected |
|---|---|---|
| `link N stuck: its traffic has waited 10s for an answer while it moved 1.8 KB in 2s (the other links answer in ~85ms) — draining` | a link throttled to a few packets a second; its users are moved | rare; each should match a real throttle (DPI) |
| `link N stuck — its connections that moved no data for 15s are closed now …` | the drain step of that link | right after the line above |
| `N of M busy links have waited 6s+ for an answer and only K answer promptly — the path or the other server is slow, not those links: none is drained` | path-wide slowness (congestion, outage, the other server slow); no verdicts for as long as it lasts and as long again after (30 s–2 min) | at most once a minute during real congestion; never on a quiet path |
| `N of M busy links have waited 6s+ for an answer and the K that answer promptly take ~1300ms, 15× their usual ~88ms — the path is congested, not those links: none is drained` (Q8) | the same, recognised by the prompt links' delay against their usual (lowest median of 10 min) | during real congestion (evening peak) only |
| `link N degraded (up-loss —, down-loss 25% of 840 segments, moving 0.4 Mbit/s where the busy links get 2.8, rtt …) — draining` | the loss rule: >12% resent while busy, 3 samples (download: 3 pong windows), and moving under half of what the busy links get | rare; a link at the others' rate is never drained for loss (Q7's format was `up-loss +a/bKB, …`) |
| `N of M busy links resend more than 12% — the path is lossy, not those links: none is drained` (Q8) | most busy links resend that much below the path's rate | at most once a minute, only while the path itself loses |

Count per hour (Iran side):

```
journalctl -u <unit> --since "-1h" -o cat | grep -c 'stuck: its traffic'
journalctl -u <unit> --since "-1h" -o cat | grep -c 'answer promptly'
journalctl -u <unit> --since "-1h" -o cat | grep -c 'degraded (up-loss'
journalctl -u <unit> --since "-1h" -o cat | grep -c 'path is congested'
journalctl -u <unit> --since "-1h" -o cat | grep -c 'path is lossy'
```

## Scenarios

Run V1 continuously; run V2–V4 in a quiet hour (they cut the users of the
links they touch, who reconnect at once). `PORT` is the tunnel's listening
port on the Iran server (reverse mode); find one link's peer port with
`ss -tn state established '( sport = :PORT )'` (the Peer column's port, `PEER`).

**V1 — normal traffic, 24 h including the evening peak.** Record the three
counts above every hour, plus `hs2 status` p50/p99 if available. Pass: `stuck`
lines rare and explainable; `answer promptly` lines only during real
congestion; no burst of `degraded (up-loss` within 2 min after a slow line;
`degraded (up-loss` per hour not clearly above the old build's rate (compare
with older journal entries of the same hours if they are still kept).

**V2 — one link throttled like DPI (3 packets/s each way).** On the Iran
server:

```
iptables -I INPUT  1 -p tcp --sport PEER --dport PORT -m limit --limit 3/s --limit-burst 3 -j ACCEPT
iptables -I INPUT  2 -p tcp --sport PEER --dport PORT -j DROP
iptables -I OUTPUT 1 -p tcp --sport PORT --dport PEER -m limit --limit 3/s --limit-burst 3 -j ACCEPT
iptables -I OUTPUT 2 -p tcp --sport PORT --dport PEER -j DROP
```

Pass: a `stuck:` line for that link within ~15 s, the link gone within 90 s,
its users answered again on other links. Remove the four rules afterwards
(same lines with `-D` instead of `-I n`). In direct mode swap `--sport` and
`--dport` (Kharej listens, Iran dials).

**V3 — three or four links at once, at low traffic.** V2 on 3–4 peer ports.
Pass: all of them caught (at most an eighth of the pool drains at a time, the
rest follow), and no `answer promptly` line — they are a minority, not a slow
path.

**V4 — a 40 s outage.** Block the tunnel port both ways for 40 s:

```
iptables -I INPUT 1 -p tcp --dport PORT -j DROP; iptables -I OUTPUT 1 -p tcp --sport PORT -j DROP
sleep 40
iptables -D INPUT -p tcp --dport PORT -j DROP; iptables -D OUTPUT -p tcp --sport PORT -j DROP
```

Pass: no `stuck:` line; at most one `answer promptly` line; users served
normally again within ~20 s after the block ends. Also count
`degraded (up-loss` lines in the 60 s after the block ends: on the rig the
Q7 build had 4-6 there (main 1), all at one tick ~18 s after — new links
flushing the users' backlog at the throttle's rate; the Q8 build had none
(they move what the other links get). If that comes in dozens on a real
pool, report it.

**V5 — the open question: drops after congestion.** On the rig, a squeeze
through a shallow queue (20 ms) gave the Q7 build more drops than main: 1730
and 1750 active connections in two runs against 1537 in main's one run (main
was not repeated), with 4 loss verdicts 1-2 min after the squeeze where main
had 1 — the downloads re-ramping into the rig's per-flow policer. The Q8
build had 1706 and no loss verdict there. Watch V1's evening peak: after each
slow or congested line, count `degraded (up-loss` and `stuck:` lines in the
following 3 minutes. If they come by the dozen and users complain, report
the lines with the slow line before them.

**V6 — high bandwidth (Q8).** At the evening peak, with downloads running:
count `degraded (up-loss` per hour (on the rig the Q7 build drained ~12 links
a minute for loss at 150-200 Mbit/s, the Q8 build none), and look at each
one's `moving X Mbit/s where the busy links get Y`: X should be well under
Y. A `stuck:` burst together with a `path is congested` line in the same
minute is a failure: report both.

**V7 — stalled readers on a small server (Q8).** Repeat the report's test:
20 downloads through the tunnel (`iperf3 -R -P 20`) and stop the receiver
(`kill -STOP`) for 60 s, under normal users. Pass: within ~10 s of the
kernel TCP memory line, `reset N connection(s) whose app had taken nothing
for 6s while kernel TCP memory was above its pressure mark`; no `degraded`
or `stuck:` line while it lasts (`kernel TCP memory on … — none is judged`
instead); the other users' p50 back to normal within ~20 s.

**V8 — the pool comes down (Q8).** After a busy hour, with traffic back to
normal: within ~5 min the link count steps down toward what the active users
need, and `hs2 status`'s `one link carries ~X Mbit/s` is near what a busy
link really moves, not the speed of the slowest links.

**V9 — tun over icmp: one carrier cut (Phase V).** On a test pair running a
tun over icmp under load (e.g. `iperf3 -R -P 16` through the tun), find a busy
echo id on the Kharej server (`tcpdump -n -i any -c 400 icmp | grep -o 'id
[0-9]*' | sort | uniq -c | sort -rn | head`) and drop it **on the way in** on
both servers (a drop in OUTPUT makes hs2's own send fail, which is not what a
cut path looks like):

```
nft add table inet hs2t
nft add chain inet hs2t i '{ type filter hook input priority -5; }'
nft add rule inet hs2t i icmp id X drop
```

Pass: within ~1.5 s on each server `dg: carrier N has heard nothing from the
other server for 1.2s while … still do — its own way through is cut`, the
downloads back within ~2 s, `… heard nothing for 3.2s — closed; a new carrier
replaces it` and a new carrier up within ~6 s. Then the same rule on the Iran
server only (a one-way cut): the Iran side logs the mute line and the Kharej
side `the other server hears nothing on it — … its flows move to live
carriers`. Remove with `nft delete table inet hs2t`. Also `hs2 status`:
`ceiling 8` with `tun over icmp` as the reason.

**V10 — ping under load (Phase V).** Through the tun (any datagram encap),
with downloads that fill the path (`iperf3 -R -P 8` through the tun), run
`ping -c 150 -i 0.1 <the other side's tun IP>` and note p50/p99 and mdev.
Then set `Environment=HS2_DG_FQ=0` on both servers (`systemctl edit
<service>`, restart) and repeat; remove it afterwards. Pass: with the fair
queue p50/p99 clearly lower and no fewer Mbit/s than without it.

**Before V11 and V12: is the sender short of CPU?** Both judge hs2 only where
the PATH is the limit. On the sending server (the Kharej server for a
download), `hs2 status`'s `cpu:` line shows the whole server next to hs2:
under the test's load it must stay under 75% busy, with tasks waiting for a
core under 20% (`hs2 doctor`'s `server cpu`: `ok`, not `busy`). If it says
`SATURATED`, or another tunnel's traffic shares the server, the run is
sender-bound: carriers that cannot send what they are allowed never push
(`P`); they show `C` (held back by their send stage), and `s` stays well
under `r`. A build before Phase X shows only `S` without `P` there: its
status has no `s`, `q`, `sending:`, `pool:` or server CPU — read the server
with `top` and `cat /proc/pressure/cpu`. Record such a run as CPU-bound (with the
`cpu:`, `sending:`, `pool:` and `carriers:` lines), do not count it for or
against what is being tested, and stop the other tunnel's traffic if you can.

Keep the pool the same size in every run: on the Iran server `hs2 config -c
<cfg> set max_links 4` and then `hs2 config -c <cfg> set min_links 4` (in
that order: a min above the current max is refused), restart,
and check that `links:` shows 4 up in each run — the carrier count alone
changes the CPU per Mbit/s (lab, 2-core sender: 1, 3 and 8 carriers gave
903, 1393 and 1453 Mbit/s).

**V11 — CPU per gigabyte (Phase W: W1 batching, W2 TCP offload).** On the
test pair (tun over icmp, reverse), the start line must say `TCP offload
on`. Under a download that fills the path (`iperf3 -R -P 8` through the tun
for 30 s), record Mbit/s, each server's hs2 CPU (`pidstat -p $(pgrep -x hs2
| head -1) 1 30`), the `cpu:` line, and `hs2 status`'s `tun:` line on both
servers (packets per read on the sending side, per write on the receiving
side, well above 1.0). Then set `Environment=HS2_TUN_OFFLOAD=0` and
`Environment=HS2_RAW_BATCH=0` on both servers (`systemctl edit <service>`,
restart), repeat with the same carrier count, and remove them
(`HS2_RAW_BATCH=0` also turns off Phase X's send-only icmp socket; V13
measures that one alone). Pass: with
both on, more Mbit/s or less hs2 CPU-seconds per GB; if the sending server
was saturated in either run, compare CPU-seconds per GB only and say so;
files transferred intact (compare sha256); no `failed`, `panic` or
`TUNSETOFFLOAD` errors in the log. For a profile of the busy side:
`HS2_PPROF=127.0.0.1:6060` in the same way, then `curl -o cpu.prof
'http://127.0.0.1:6060/debug/pprof/profile?seconds=30'` under load.

**V13 — a sender short of CPU (Phase X).** On the test pair (tun over icmp,
reverse, the pool fixed as above), with the Kharej server busy with its
other work as it normally is: under `iperf3 -c <T> -R -P 8 -t 30`, record
Mbit/s, the Kharej hs2's CPU (`pidstat`) and its `cpu:`, `sending:` and
`carriers:` lines at 20 s, in three settings taken in turn (X, R, F, X, R,
F, X), each set on both servers with `systemctl edit` and a restart: X the
default, R `Environment=HS2_RAW_TX=0` (the shared icmp socket), F
`Environment=HS2_FAIR_SHARE=0` (the send-stage rule and the Phase W rules
off). Pass: X's carriers `C` (not `S`) with `r` within about twice `s`;
X's Mbit/s per CPU-second within about 15% of F and no lower than R; and
under a fixed load below what F carries (`iperf3 ... -b`), ping through the
tun no worse in X than in F. X is not expected to beat F for raw throughput
on a saturated, asymmetrically loaded server: the stage rule trades a small
amount of throughput there (lab: ~14% after the Phase Y fix, down from ~27%)
for not over-committing the rate, which would flood a shared path when the
CPU frees — the better ping is the carriers paying that back. If X trails F
by much more than 15%, or its carriers stay `S`, that is a fail.

**V12 — carriers share the bottleneck (Phase W, rate control).** On the
test pair (tun over icmp, reverse), with the pool size fixed as above, the
path filled by downloads (`iperf3 -c <T> -R -P 8 -t 40` through the tun)
and `ping -c 200 -i 0.1 <T>` started 10 s in: note ping p50/p99/loss and
Mbit/s, and take `hs2 status` on both servers at about 20 s and 35 s. Only
path-bound runs count (see above); repeat until three are. Then set
`Environment=HS2_FAIR_SHARE=0` on both servers (`systemctl edit
<service>`, restart), repeat three path-bound runs, and remove it. Pass, on
path-bound runs: with the rules on, every busy carrier's `s` within ~1.5x
of the `pool:` fair share (none below ~1 Mbit/s while another carries
several times more), none still flagged `S` once its `q` has stood at 5 ms
or more for a second, ping p99 lower or no worse, and no fewer Mbit/s than
with `HS2_FAIR_SHARE=0`.

**Reading the send-stage fields.** In each `carriers:` entry `r` is the
pacing allowance and `s` what the carrier really sent (Mbit/s); `P` means
`s` reached 80% of `r`. `qQUEUE/SRTT` is the queue the rate control sees
on the path and its smoothed round trip (ms), measured from the moment a
datagram is written: it does NOT include hs2's own queues in front of the
socket — the `sending:` line's fair-queue wait, and the writers' wait for
pacer room — whose time adds to every packet's latency. `socket write` is
the time per write (the system call, plus the shared socket's lock on gre,
ipip and ipx or with `HS2_RAW_TX=0`) with the datagrams each carried. Writers wait for pacer room whenever the carriers
are the limit, on a full path as on a starved sender, so that share alone
tells nothing. Path-bound: `P` set (`s` close to `r`), `q` 5-20 ms.
Sender-bound: `C` without `P` (`s` well under `r`), `q` under 5 ms, and the
`cpu:` line saturated. `drops:`'s "refused by the kernel" growing during a
run over icmp means the interface queue overflowed: tunnel packets lost
before the wire, which the peer counts as path loss. `net:` is the whole
server's output discards — on an icmp listener mostly the kernel's own
replies the tunnel suppresses — so it is context, not a count of this
tunnel's losses.

**V14 — ICMP timing/id camouflage (Phase CA), opt-in.** On a SEPARATE,
user-free test IP pair (never a user IP), tun over icmp. Set
`Environment=HS2_ICMP_CAMO=1` on BOTH ends (`systemctl edit <service>`,
restart, wait 60 s). Compare a packet capture of the carrier traffic with the
flag off vs on. Pass: (a) with the tunnel up but idle, the per-second packet
rate drops sharply and the ~10 Hz feedback line and the ~0.25 Hz 4-second
probe dip disappear from the inter-packet-time spectrum; (b) link ids form a
small ascending cluster, not values spread across the whole 16-bit range;
(c) under a saturating `iperf3 -R` the throughput and ping are no worse than
with the flag off (timing camouflage must not cost goodput); (d) no link
churn or `idle past deadline` in the logs (keepalive still holds the link).
The flag changes only timing and the id draw, so an old peer interoperates,
but set it on both ends to shape both directions.

**V15 — the user-port path (Phases Z, Z2, Z3).** Both servers on this
release (the Iran side decides links; each side's smux changes help the
direction it receives, its send gate the direction it sends). Run it
alongside V1 for 24 h including the evening peak, with real users through
`forward_ports`. Watch the Iran side:

```
journalctl -u <unit> -f -o cat | grep -E 'waited .* to open on it|got no answer from the exit|round trip .* where the other links|resends .* where the other links|on par with the other links|while up: its receive buffer'
```

| Log line | Meaning | Expected |
|---|---|---|
| `link N: a new connection waited 3.1s to open on it — it takes new connections only when no other link can, for 10s` (Z) | a slow open: the link lags for 10 s and new users go elsewhere | rare; during a throttle or an outage of that link |
| `N new connection(s) got no answer from the exit on a link that went quiet: moved to another link (…)` (Z2) | users that opened on a link in the second before it went black were moved, their TLS hello sent again on another link | only around real link deaths; never in waves on a healthy pool |
| `link N: round trip 862ms at best where the other links take 61ms — new connections go to the other links while they have room` (Z3) | a link clearly worse than its siblings (delay, deep queue, spikes) takes no new users while others have room | only for a link that really is slower (compare its ping); never for most links at once |
| `link N: resends 4.2% of what it sends where the other links resend 0.3% — …` (Z3) | the same for loss | as above |
| `link N: on par with the other links again after 36s (round trip 61ms at best)` (Z3) | the verdict ends | some time after each verdict line |
| `link N down: … (while up: its receive buffer ran full N time(s); readers that fell behind braked M time(s))` (Z2) | how often the link's whole receive buffer ran full (every user on it stalled) and how often slow readers were braked | mostly absent; report links where the first count is large |

Pass: users report no slower start of large downloads and no stalls when a
heavy downloader slows down; new connections do not hang for ~20 s when a
link dies; the verdict lines name links that really are slower or lossier
(check with `ping`/`mtr` to the peer over that link's 5-tuple if possible) and
never most of the pool at once; uploads next to heavy downloads (and the
reverse) and interactive use next to bulk transfers feel no worse than on the
previous release. Report the counts of each line per hour and any user
complaint with its time. Roll back to the release before Phases Z–Z3 (above:
`0812bc9`, binary build `da621f5db2bb`) if verdict or move lines come in waves
across the pool, or if users are cut or stalled more than before.

**V16 — one connection past ~100 Mbit/s (Phase Z5 step 1: the download
window core and the exit's pacing net).** Before this release one connection
moved at most about 2 MiB per round trip (~112 Mbit/s at 150 ms, ~168 at
100 ms; on the rig 81 Mbit/s on a 300 Mbit/s path at 150 ms). Now:

- the **window core** runs on the **Iran (edge) server**, for downloads only:
  it lets a fast download's window grow past 2 MiB as far as the path carries.
  It starts on a link only once the Kharej server sends its new 96-byte stats
  record, so **both servers must run this release**; with an old Kharej server
  downloads stay exactly as before. It also stays off where the old windows
  already fill the path or the Kharej server resends over 1.5% (or over 1%
  that is not steady over its records, comes in bursts, or follows a policer;
  and for a few seconds after any record that lost in a burst).
- the **pacing net** runs on the **Kharej (exit) server**, on every link
  socket, whatever the Iran server runs: when a link socket resends 2% or
  more while it paces at over twice what the path delivers, its pacing is
  held at 1.25× the delivery rate until the resends stop.
- uploads are unchanged until step 3.

*1. One big download, before and after.* On a test pair with a fast path
between the servers (the clearest difference: 300 Mbit/s or more each way and
a round trip of 100 ms or more), move one large download through the tunnel:
either `iperf3 -s -p 5201` on the Kharej server behind a test user port (the
port in the Iran server's `forward_ports`, and `5201=127.0.0.1:5201` in the
Kharej server's `port_map`) and from the Iran server or a client behind it
`iperf3 -c <Iran server IP> -p 5201 -R -P 1 -t 60` (one connection, the
Kharej side sending), or one large file through a user's client. Take six runs
in turn: core on (the default), core off (`HS2_TUNE_WCORE=0` on the Iran
server, below), on, off, on, off. In each run record the Mbit/s, the Iran
server's `hs2 status` during the transfer, the new log lines on both servers,
and, from another client through the same tunnel, an interactive user's
latency (a small request every 200 ms): its p50/p99. Pass: with the core on
the download is clearly faster where the path has room (rig: 81 → 223 Mbit/s
on 300/150, 112 → 834 on 1000/100; a real path will differ), and never
clearly slower than with it off; during the transfer
the Iran server's `hs2 status` shows `windows:    fast connections: 1 (windows
6 MiB) · pool 4 of 64 MiB in use (grown windows 4 MiB, borrowed 0 MiB)` (sizes
vary: a window takes from the pool only its part past the base 2 MiB, and
`borrowed` is what link buffers that ran full took; build `a372b1301121`
shows the older `fast connections: 1 (windows 6 MiB of 64 MiB pool)`, whose
MiB are the windows, not what they take of the pool) and its log a
`downloads fill the path … window held at …` line for the link; no cut, and no `degraded` or `stuck:` line for that link;
the interactive user's p50 about the same (its p99 may be higher: the known
cost, below — report it). If the core never starts (no `windows:` line, no
`fill the path` line, the same speed as off; or only `… with their own
windows … it stays off for N s while M or more run`
lines: the old windows already fill the path,
and the core stays off by design): check `hs2 version` on the
Kharej server (an old one keeps the core off), whether one connection already
fills the path with 2 MiB windows (then the core stays off by design), and
whether the Kharej server's socket resends over 1.5% (or over 1% that is not
steady over its records, comes in bursts, or follows a policer; `ss -ti` on
the link).

*1b. Several big downloads at once.* The window cores of the Iran server's
links to one Kharej server share that server's path: since the third red-team
round they hold one queue target between them (~15 ms in all, not 15 ms each)
and even out their rates, so a download that starts late still gets its share.
With the core on, run two, then four single-connection downloads at once
(`iperf3 -c <Iran server IP> -p 5201 -R -P 1 -t 120`, each from its own
client or shell, started 20 s apart; the Iran server spreads new connections
over its links), and the interactive user of test 1 beside them. Pass: after
the first minute each download runs within 2× of the others (none stuck near
its old ~100 Mbit/s while the others take the path); the sum about what one
download reaches alone; the interactive user's p50 within about 10 ms of
its p50 with one download (rig, 2 and 4 downloads on 1000/100: the path's
queue 11–14 ms where it was 19–34 ms; a download started 30 s after three
others 205 Mbit/s where it stayed at 69–96); no cut. Report each download's
Mbit/s per 10 s.

*2. 24 h with real users,* alongside V1 and V15. Watch both servers:

```
# Iran (edge)
journalctl -u <unit> -f -o cat | grep -E 'fill the path|download windows|loss with no queue|window pool full|MiB ceiling|round trip grew to|window core:'
# Kharej (exit)
journalctl -u <unit> -f -o cat | grep -E 'pacing held|pacing net:'
```

| Log line (exact format; numbers vary) | Meaning | Expected |
|---|---|---|
| `mtcp: link N: downloads fill the path at ~287 Mbit/s (round trip 151 ms): window held at 6.4 MiB for 2 fast connection(s), queue ~4 ms` (Iran) | the core found the window that fills the link's path (end of its start-up, or the knee moved by more than 25%) | when large downloads run; at most once a minute per link |
| `mtcp: link N: downloads fill the path at ~260 Mbit/s (round trip 150 ms) with their own windows (8.0 MiB for 4 fast connection(s)) — the window core's step only added queue; it stays off for 64 s while 4 or more run` (Iran) | the core's first step found the path already full with the downloads' own windows (the rate did not follow it, the queue did): the core went back off, and stays off that long (32 s, doubling with each such finding within a hold of the previous one's end, up to 256 s) while as many fast connections or more run | on paths the old windows already fill (several downloads on one link); within the cap of the line above, at most once a minute per link |
| `mtcp: link N: <why> — download windows 6.4 MiB → 3.1 MiB` (Iran), `<why>` one of `path queue grew by 38 ms`, `the exit's queue grew by 12 ms`, `path queue grew by 12 ms (the link's own round trip)`, `control pings came back 40 ms late`, the same with ` (resent 2.1%)`, `packets were lost as the windows grew (resent 1.8%)`, `packets arrived behind holes as the windows grew`, `packets arrived behind holes on a clean path`, `path slowed to ~90 Mbit/s with its queue growing` | a drain: the core shrank the link's download windows (never below 2 MiB) because the path's queue grew, or loss came with the growth; a drain for holes (`packets arrived behind holes …`) waits for the Kharej server's next stats record (~2 s), which keeps it or takes it back (next row) | now and then, mostly at the evening peak or when the path changes; at most once per link per 30 s |
| `mtcp: link N: the other server resent 0.002% — the holes were reordering, not loss; download windows back 2.0 MiB → 8.0 MiB` (Iran) | a drain for holes taken back: the Kharej server's next record showed it resent no more than its usual level (within its spread, and under 0.05%), so the packets behind the holes were reordered by the path, not lost. The windows climb back to where they were (within the window pool), and holes as dense cut no more on that link for 10 minutes (the link's reordering level) | on a path that reorders packets (the Iran–Kharej path does, more the faster the download): a few times early in a fast download, then rarely; at most once per link per 30 s |
| `mtcp: link N: 0.4% loss with no queue — windows cover the resends (+18 ms)` (Iran) | random loss with no queue: the core does not back off for it | on lossy paths; at most once per link per 10 min |
| `mtcp: window pool full (64 MiB): 5 fast connection(s) on 3 link(s) held below what their path carries` (Iran) | several fast downloads at once took the whole window pool; each keeps what it has | rare (at most once in 5 min); harmless when frequent on a small server (report it) |
| `mtcp: link N: one connection's window is at its 16 MiB ceiling (round trip 480 ms)` or `… N connections' windows are at their 16 MiB ceiling …` (Iran) | a very fast or very long path: 16 MiB is the most a window gets | rare |
| `mtcp: link N: the path's round trip grew to ~400 ms (a route change) — the window core follows it` (Iran) | the link's path changed | rare |
| `mtcp: window core: N more link(s) …` (Iran) | the lines above past 8 links in a minute, in one summary line | only on a busy pool |
| `mtcp: window core: N more link(s) took back K drain(s) for holes in the last minute (the other server resent no more than its level: reordering)` (Iran) | the undo line above past 8 links in a minute | only on a busy pool on a reordering path |
| `mtcp: link from 1.2.3.4:5678: resending 6.1% at a pacing rate 3.9× what the path delivers — pacing held at 1.25× (~240 Mbit/s) until the resends stop` (Kharej) | the pacing net engaged on that link socket | now and then (a shallow queue, a policer, several downloads on one link); at most once per link per 5 min |
| `mtcp: pacing net: N more link(s) resent past 2% and had their pacing held in the last minute` (Kharej) | the same past 8 links in a minute | only while the path itself loses |
| the line of a link's end, Iran (`mtcp: link N down: …`, a retired or drained link, a reverse link): `… (while up: its receive buffer ran full N time(s), K of them grown from the window pool instead; …; window core: up to 9.1 MiB at 412 Mbit/s, drained 3 time(s) for queue, 5 for loss (2 of them for holes taken back: the other server resent no more than its level; reordering level 60%), held by the window pool 0 time(s))` (the parenthesis only when a drain for holes was taken back; `, N for a slower path` before `held` when the path slowed) | what the core did on that link while it was up; `for loss` counts every drain and cut for loss, those for holes later taken back too (their number in the parenthesis, with the link's reordering level as it ended, 0% once it expired); `K of them grown from the window pool instead` counts the times its full buffer borrowed from the pool instead of stalling | only on links that carried fast downloads |
| the line of a link's end, Kharej (`link down from 1.2.3.4:5678: … (now 5)`, or in reverse `mtcp: exit link down (slot N: …)`, `mtcp: exit slot N retired …`): `… (while up: pacing held 2 time(s) for resends (up to 2.8%))` | how often the pacing net held that link and the largest resend share it saw | only on links it engaged on |

Count per hour (Iran, then Kharej):

```
journalctl -u <unit> --since "-1h" -o cat | grep -c 'window held at'
journalctl -u <unit> --since "-1h" -o cat | grep -c 'stays off for'
journalctl -u <unit> --since "-1h" -o cat | grep -c 'download windows'
journalctl -u <unit> --since "-1h" -o cat | grep -c 'reordering, not loss'
journalctl -u <unit> --since "-1h" -o cat | grep -c 'loss with no queue'
journalctl -u <unit> --since "-1h" -o cat | grep -c 'window pool full'
journalctl -u <unit> --since "-1h" -o cat | grep -c 'pacing held at'
journalctl -u <unit> --since "-1h" -o cat | grep -c 'pacing net:'
```

Normal: the lines above at the rates in the table; large downloads faster
than on the previous release; no more cuts, `stuck:` or `degraded` lines than
the previous release had in the same hours. Known costs, accepted for this
step (CHANGELOG, Phase Z5 step 1): a user whose connection shares the link
(or the bottleneck) with a fast download may see more delay spikes (on the
rig p99 up to ~2.3×, and p50 up to 1.7× on a lossy path); on lossy, very
long (500 ms and more) or shallow-queue paths the gain is small; uploads gain
nothing yet.

Reasons to switch a part off (below) and, if it stays with both off, to roll
back:
- any user cut that the previous release did not have (connections reset in
  waves, users reporting drops during large downloads);
- `degraded` or `stuck:` lines the previous release did not have — above all
  on links whose end line shows `window core: up to …` (the core is never
  meant to bring the loss rule to a link) → `HS2_TUNE_WCORE=0`;
- `pacing held` on many links at once (the `pacing net: N more link(s)` line
  minute after minute) together with downloads slower than with the net off
  → `HS2_TUNE_PACENET=0`;
- interactive users (calls, games) next to heavy downloads clearly worse than
  on the previous release → `HS2_TUNE_WCORE=0`;
- `hs2 doctor` FAILs or a memory or CPU climb the previous release did not
  have.

*Switching a part off (no reinstall).* The switches are environment variables
read when hs2 starts; set them in a systemd drop-in of the tunnel's unit (find
it with `systemctl list-units 'hs2*'`):

```
systemctl edit <unit>
```

and add, in the editor:

```
[Service]
Environment=HS2_TUNE_WCORE=0
```

on the **Iran** server to turn the window core off (every download window
back within 2 MiB, as before), or `Environment=HS2_TUNE_PACENET=0` on the
**Kharej** server to turn the pacing net off — two separate switches, one per
server. Then `systemctl restart <unit>` (the tunnel's users reconnect at once)
and check the start lines: `journalctl -u <unit> --since "-2min" -o cat |
grep 'tuning: HS2_TUNE'` shows `tuning: HS2_TUNE_WCORE=0` (or `…PACENET=0`).
To turn it back on, delete that line with `systemctl edit <unit>` (or
`systemctl revert <unit>` when the drop-in holds nothing else) and restart. A
drop-in survives Upgrade in the menu (only the unit file is rewritten; it goes
only with the tunnel's removal), so remove it after the test. With both
switches off, what remains of this release on the data path is mostly the
window pool's borrowing (a link whose buffer runs full borrows from the pool
instead of stalling); to rule that out too, add
`Environment=HS2_TUNE_SMUX_POOL=0` on both servers (with `HS2_TUNE_WCORE=0`
on the Iran server). If a problem stays with all of them off, roll back the
binary (above).

## Phase R1: lab numbers only

Build `e8a84ccb17fa` went to `main` and to the main servers by the owner's
decision, with the old-against-new field comparison dropped (the "Release" line
below). So R1 has no field table here: **every number in this section is from the
lab** (the rig and the test suite). CHANGELOG, "Phase R1 — release", has the rest;
sources: `docs/architecture/wip/r1/merge/REPORT.fa.md`,
`docs/architecture/wip/r1/round4-judge.fa.md` and
`docs/architecture/wip/r1/review/SUMMARY.fa.md`. `N` is the link count.

| Lab result | R1 | Production `1c23629` |
|---|---|---|
| Probe minus floor on `fq_codel`, `N` = 1 to 300 (bound +20) | +10.5 to +11.0 ms | +230 to +354 ms |
| 25 users of 16 connections next to 25 players, `N=8` | 88.1 Mbit/s | 5.4 to 5.5 Mbit/s |
| Busiest QUIET link dies, `home`, `N=1`: connections / users hit | 13% / 30% | 100% / 100% |
| Scale, one run per `N`, `N` = 1 to 600: download | 89.3 to 83.7 Mbit/s | not in that table |

The test suite on `e4783fe` was green (`go test ./...` 1064 / 0 / 32, `-race` on
all of engine 738 / 0 / 29, fuzz 13 of 13, install 17 of 17), apart from one test's
own wait, fixed in `e22c2fe`, which is not in this build. The independent review
found nothing critical.

What the lab left open, so watch for it on real servers:

- calls next to heavy traffic: `home` at `N=8` has downlink p99 over 150 ms in 6
  of 26 runs, and under 1 to 2% random loss only 0.38 to 0.39 of a call arrives,
  p99 ~30 s;
- fairness at 64 links with every link carrying data: median user 0.31 to 0.36
  Mbit/s, top 10.3 to 16.0;
- latency at `N ≥ 300` with every link carrying data: the +20 bound holds for no
  binary (`N=300` +34 to +698 ms, `N=600` +443 to +740 ms).

Not in this build: the fix for the edge undercounting QUIET demand (`r1-fix1`) and
round 5 (`r1-r5`). Both ends must run the same build, so never use the menu's
Upgrade on one end alone, and a server with several tunnels restarts them together
(`docs/architecture/wip/r1/review/RUNBOOK.fa.md`).

## What to send back

For each scenario: the counts, the relevant log lines (Iran side, with
timestamps), the time of the action (V2–V4), and whether users noticed. For
V16 also: the six runs' Mbit/s and the interactive p50/p99 with the core on
and off, the round trip between the servers (`ping`), the Iran server's
`windows:` line, and the Kharej side's `pacing held` lines with timestamps.

Release: binary build `e8a84ccb17fa` (source commit `e8a84cc`, static,
`CGO_ENABLED=0`; `hs2 version` shows `build e8a84ccb17fa`).

This release: the redesign's phase R1 (candidate fin2 with the red-team and
independent-review fixes). Put on `main` at the owner's request on 2026-10-31,
before any field comparison: the remaining measurements run on his main servers
with real traffic. Rig evidence only; both ends of a tunnel must run this build.
Round 5 (latency lane under real-path loss at few links) is not in this build.
Rollback targets: `1c23629533c9` (Phase Z5 step 2; commit `6dd5a06`), and the
previous `main` at `ec701bf` (build `a372b1301121`).

Previous release: binary build `1c23629533c9` (source commit `1c23629`, static,
`CGO_ENABLED=0`; `hs2 version` shows `build 1c23629533c9`).

This release: Phase Z5 step 2 (the window core on the real path: reordering
answered only on loss the sender confirms, no false route change from slow
sources, a sticky accepted route change, fast-set membership only with a held
window, the pacing net's stale-cap fix and quiet sockets read once a second,
the judge's P1–P3 and the post-merge review fixes; V16 above, with its new
log lines and the new `windows:` line; `hs2 version` shows its build id), on
top of Phase Z5 step 1 and Phases Z, Z2 and Z3 (V15). Built on the work branch
`claude/smux-buffer-constants-fv9bu8` only: `main` stays on the previous release
(the field test showed no throughput regression but left a latency question
open; the next release to `main` is the redesign's phase R1). Production runs
this build; to pin it, set `HS2_REPO_RAW` to the commit that carries this
binary. Measured on the real-TCP rig and the
link model, and across the real Iran–abroad path against the previous release
(CHANGELOG, Phase Z5 step 2, "Field"). The previous release, binary build
`a372b1301121` (Phase Z5 step 1; commit `bfea5e8`), is the rollback target;
before it `ef2dffdbbb87` (Phases Z, Z2 and Z3; `c9825f2`). See CHANGELOG (Q7,
Q8, Phase V, Phase W, Phase X, Phase Y, Phase CA, Phase Z, Phase Z2, Phase Z3,
Phase Z5 step 1, Phase Z5 step 2) and `git log`.
