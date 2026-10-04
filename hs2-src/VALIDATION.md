# Real-server validation — stuck links and the loss rule (Q7)

This is the checklist for validating the Q7 release (see CHANGELOG, "Q7 — stuck
links") on a real Iran/Kharej server pair with real users. The load rig showed
the rules work and do not cut users needlessly in the scenarios it can make;
what it cannot make is Iran's real DPI, real evening congestion and real user
traffic. Both servers must run the release build (`hs2 version` shows the
build id below).

- Release commit: see the "Release" line at the end of this file.
- Previous main (rollback target): `d310f79` (binary build `bad9d4be1f43`).

## Rollback

On each server, reinstall the previous binary pinned to its commit (no change
to the repository needed):

```
HS2_REPO_RAW=https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/d310f79 bash <(curl -fsSL https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/d310f79/install.sh)
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
journalctl -u <unit> -f -o cat | grep -E 'stuck|answer promptly|degraded|closed with its'
```

| Log line | Meaning | Expected |
|---|---|---|
| `link N stuck: its traffic has waited 10s for an answer while it moved 1.8 KB in 2s (the other links answer in ~85ms) — draining` | a link throttled to a few packets a second; its users are moved | rare; each should match a real throttle (DPI) |
| `link N stuck — its connections that moved no data for 15s are closed now …` | the drain step of that link | right after the line above |
| `N of M busy links have waited 6s+ for an answer and only K answer promptly — the path or the other server is slow, not those links: none is drained` | path-wide slowness (congestion, outage, the other server slow); no verdicts for as long as it lasts and as long again after (30 s–2 min) | at most once a minute during real congestion; never on a quiet path |
| `link N degraded (up-loss +a/bKB, down-loss +c/dKB, rtt …) — draining` | the loss rule: >12% resent while busy, 3 samples | as before; not in a wave right after a slow line |

Count per hour (Iran side):

```
journalctl -u <unit> --since "-1h" -o cat | grep -c 'stuck: its traffic'
journalctl -u <unit> --since "-1h" -o cat | grep -c 'answer promptly'
journalctl -u <unit> --since "-1h" -o cat | grep -c 'degraded (up-loss'
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
normally again within ~20 s after the block ends.

**V5 — the open question: drops after congestion.** On the rig, a squeeze
through a shallow queue (20 ms) gave this build more drops than main: 1730
and 1750 active connections in two runs against 1537 in main's one run (main
was not repeated), with 4 loss verdicts 1-2 min after the squeeze where main
had 1 — the downloads re-ramping into the rig's per-flow policer. Main was
blind there by accident (its control channel died in the squeeze). Watch V1's
evening peak: after each slow spell, count `degraded (up-loss` lines in the
following 3 minutes. If they come by the dozen and users complain, report the
lines (with the slow line before them) — the fix would be a longer quiet time
for the loss rule after a spell, not a change to the stuck rule.

## What to send back

For each scenario: the counts, the relevant log lines (Iran side, with
timestamps), the time of the action (V2–V4), and whether users noticed.

Release: see CHANGELOG (Q7) and `git log` on main.
