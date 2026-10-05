# Real-server validation — stuck links and the loss rule (Q7, Q8), tun over icmp (V)

This is the checklist for validating the Q7 and Q8 releases (see CHANGELOG,
"Q7 — stuck links" and "Q8 — the health rules at high bandwidth") on a real
Iran/Kharej server pair with real users. The load rig showed
the rules work and do not cut users needlessly in the scenarios it can make;
what it cannot make is Iran's real DPI, real evening congestion and real user
traffic. Both servers must run the release build (`hs2 version` shows the
build id below).

- Release commit: see the "Release" line at the end of this file.
- Previous main (rollback target): `6ae3492` (binary build `f9b668c4e1e4`);
  before it `d310f79` (binary build `bad9d4be1f43`).

## Rollback

On each server, reinstall the previous binary pinned to its commit (no change
to the repository needed):

```
HS2_REPO_RAW=https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/6ae3492 bash <(curl -fsSL https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/6ae3492/install.sh)
```

then Upgrade the tunnel(s) in the menu. To roll `main` itself back, revert the
release commits on main (do not force-push).

Roll back at once if any of these happens: users report being cut in waves;
more than a handful of `stuck:` lines an hour that no real throttle explains;
a burst of `stuck:` lines at the same second (more than an eighth of the
pool); `degraded (up-loss` lines by the dozen right after a congestion spell;
`hs2 doctor` FAILs that the old build did not have.

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

## What to send back

For each scenario: the counts, the relevant log lines (Iran side, with
timestamps), the time of the action (V2–V4), and whether users noticed.

Release: see CHANGELOG (Q7, Q8) and `git log` on main.
