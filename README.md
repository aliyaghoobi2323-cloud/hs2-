# hs2-tunnel

A DPI-resistant tunnel between an Iran server and a foreign (kharej) server.
Runs **alongside Backhaul** without touching it — its own ports, its own subnet.

**v3 — stream core over multi-link TLS.** Every mode now works the way `mtcp`
did: a user's TCP connection ends on the Iran server and its bytes ride an
smux stream over a pool of real TLS 1.3 links (Chrome fingerprint, real
certificate, active-probe resistance). The kharej server opens its own
connection to the panel. There is never TCP inside TCP.

## What it does

- Opens user ports on the Iran server and forwards them to a panel inbound on
  the kharej server — the role Backhaul plays. Optionally UDP as well.
- Carries everything inside ordinary TLS 1.3 to a real domain. An active probe
  gets a real certificate and a plain web page.
- Survives link drops: user ports stay up, dead links are detected in about
  two seconds and rebuilt.

## The adaptive connection pattern (v3.2)

The number of parallel TLS links is not fixed. A dedicated controller (the
*autopilot*) sizes the pool continuously between **2 links and a ceiling sized
to the server** (32 on a small box, up to 300 on a big one — see *The
ceiling* below) — up when traffic needs more, and **back down when it does
not**:

- **active flows** — at least one link per `per_link` (default 8) connections
  that are actually moving data. Idle connections (an xray panel keeps
  hundreds open) do not count.
- **links at their limit** — a link whose sender is blocked by the network
  (measured on this side for uploads and reported by the other server for
  downloads) is "at its limit". When the links at their limit leave no free
  link for new connections, the autopilot tries ~25% more links and keeps them
  only if the traffic they carry *adds* to the total. If the path itself is
  full, more links do not add anything: the try is undone and retried later
  with an increasing pause (up to 8 minutes).
- **shrinking** — once demand has stayed below the current size for a minute,
  the pool steps down toward what the recent peak needs. A link that is no
  longer needed is marked *retiring*: it takes no new connections and closes
  by itself once its connections have ended. **Shrinking never cuts a
  connection that is still in use.** A connection on a retiring link that has
  been completely idle for `drain_idle_sec` (default 310 s, just above xray's
  300 s idle timeout; `0` = never) is closed so the link can finish.
- **a bad link** (resending more than 12% of the segments it sends while
  busy, and moving less than half of what the busy links get) is
  *degraded*: it takes no new connections and its replacement is brought up
  at once (on the reverse edge the exit is asked for it). After 45 s its
  connections that moved no data for 15 s are closed — idle ones, and on a
  link that got stuck every one — and the app reconnects onto a healthy link;
  a connection whose data still passes gets 45 s more to finish (a page, a
  video segment, a short download), then the link closes with what is left
  — users kept on a lossy link wait seconds per reply, so they are better off
  reconnecting.
  When the pool is at its ceiling and needs the slot, the bad link closes at
  45 s with what is left, as before. While the path is slow and right
  after (see a stuck link, below), no link is judged for loss until the
  links have had time to recover: TCP resends what the slowdown held back,
  and that says nothing about one link. A link that resends a lot but still
  moves what the other busy links get is running into a per-connection
  throttle that drops what goes over its rate, and is left alone: its users
  would get no more elsewhere. When most busy links resend that much below
  that rate, the path is lossy: none is drained. At most an eighth of the
  pool drains at a time. Log: `link N degraded (up-loss —, down-loss 25% of
  840 segments, moving 0.4 Mbit/s where the busy links get 2.8, rtt 120ms)
  — draining`; a lossy path: `N of M busy links resend more than 12% — the
  path is lossy, not those links: none is drained`.
- **a stuck link** — throttled to a few packets a second, the way DPI slows a
  flow without cutting it — moves too little for the loss rule and still
  gets a keepalive through, so it used to keep serving while its users got
  no answer. Each link's control ping travels behind its own traffic, so
  how long it waits is how long the users wait. A link is *stuck* when its
  ping has waited 6 s while it moves almost nothing (under 6 KB/s, or under
  half of what the links that answer promptly move), and other busy links
  answer at once. It is degraded like a bad link, and its connections that
  moved no data for 15 s close right away. When two or more links wait like
  that and they outnumber the links answering promptly — or the links that
  answer promptly take 4× their usual time (and over 0.5 s): a congested
  path — the path or the other server is slow: no link is drained, nor
  judged for loss, nor for as long again after (30 s to 2 min) while the
  links catch up on their own.
  A link that waits while it moves its share is waiting on its own users'
  load, not on a throttle, so it is left alone. At most an eighth of the
  pool drains as stuck at a time. Log: `link N stuck: its traffic has
  waited 8s for an answer …`, then `link N stuck — its connections that
  moved no data for 15s are closed now …`; a slow path: `N of M busy links
  have waited 6s+ for an answer and only K answer promptly — … none is
  drained`, or `… and the K that answer promptly take ~1300ms, 15× their
  usual ~88ms — the path is congested, not those links: none is drained`.

It comes up "warm" (8 links) so a burst of connections at start spreads
immediately — or, after a restart within 15 minutes, at the size it had
before (see *Restarts and outages* below). In **reverse** mode the edge
(which alone sees the users) drives the exit's link count over a control
channel, so the dial pool on the foreign side follows it in both directions.

Watch it live: `hs2 status -c /etc/hs2/config.json` (or the **Live pattern
monitor** in `hs2-menu` → tunnel manager) shows the links up (serving +
retiring), the target, the phase (steady / scaling / probing / holding /
shrinking), *why* the pool is that size, and the traffic, e.g.:

```
links:   7 up = 5 serving + 2 retiring / target 5 (shrinking, range 2–32)
why:     18 active of 251 open connections, 1 of 5 serving links at their limit, peak 6.8 Mbit/s; 2 retiring link(s) close as their connections end (held by 38 open, 1 active)
traffic: 251 connections (18 active) · 6.1 Mbit/s · 1 link at its limit (~2.4 Mbit/s each)
```

### The ceiling — how many links at most (auto, per server)

`max_links` caps the pool. It has three forms:

| `max_links` | meaning |
|-------------|---------|
| `0` — **auto** (new installs) | follows **this server's hardware**, re-derived at every start: one link per 48 MB of RAM, at most **300**, never below the old profile value (**32** under ~1.5 GB RAM, **48** medium, **64** high). A single-core server keeps its profile value; 2–3 cores allow at most 128. A 16 GB / 4-core server gets 300, a 2 GB / 2-core one 48. A server resized up or down gets the matching ceiling on its next start, with nobody editing the config. |
| a number | fixed by you; never changed automatically |
| absent | the historical fixed **32** — a config written before auto existed behaves exactly as before after upgrading |

(An explicit `0` used to mean "the default, 32" and now means auto — only a
hand-edited config can contain it; set a number to keep it fixed.)

**dgtun** uses the same auto ceiling. Its interim cap (64 carriers over a raw
encapsulation, 128 over udp) was lifted after the 300-carrier load test: over
gre, 300 carriers sharing one raw socket per peer carried 176 Mbit/s where one
socket per carrier (older builds) carried 28; over udp, 300 carriers ran with
echo p50 90 ms / p99 121 ms and no dropped connection, at ~5% more CPU than
128. An explicit `max_links` is used as written.

**Why the ceiling depends on RAM.** Under a stalled reader each link's session
may hold up to 8 MiB it could not deliver yet — about **12 MiB** with smux's
frame rounding. The rule keeps that worst case at **≤ 25 % of RAM** (≤ ~33 %
if every frame rounds badly): 300 links ≈ 3.5 GB worst case on a 17 GB server.
Each relayed user connection costs another ~80 KB (≈ 0.45 GB per side at
6,000 open connections), a UDP user flow 64 KB on the Iran side and 128 KB
on the Kharej side. hs2 sets Go's soft memory limit to half the RAM
(`GOMEMLIMIT` overrides it). A stalled reader no longer grows memory without
bound: see the stalled-reader guard below.

The ceiling is only headroom: the autopilot uses about one link per 8
*active* connections, so 400 active connections want ~50 links. Both the
ceiling and the memory limit are **per tunnel** and computed from the whole
server; with several pooled tunnels on one server `hs2 doctor` adds them up
and warns above 40 % of RAM (give each a fixed `max_links` of about its auto
value divided by the number of tunnels). In a container, or a unit with
`MemoryMax`/`CPUQuota`, the cgroup's limits are used instead of the host's.

**Which server's ceiling counts.** In **direct** mode the Iran server's ceiling
alone applies (the Kharej server accepts every link it dials). In **reverse**
the lower of the two applies (the Kharej server dials, clamped to its own max)
— so for more than 64 links in reverse, **upgrade both servers and set
`max_links` to `0` (auto) on both**. The two servers tell each other their
ceilings over the tunnel itself, so `hs2 status` on **either** server shows the
exact effective number and which side sets it, e.g.:

```
ceiling: 64 links — limited by the Kharej server (reverse: the lower of the two applies); this server: 300 (auto — 16.6 GB RAM, 20 cores: one link per 48 MB of RAM, at most 300), the Kharej server: 64
```

If the other server still runs an older hs2, the line says its ceiling is not
reported (the tunnel works as before). `hs2 doctor` warns when a fixed
`max_links` is above what the RAM comfortably holds, notes when the box could
use more, and — for auto — when the hardware changed under a running tunnel
and a restart would apply it; `hs2 check` warns about a `min_links` above 64
(that many links stay open at all times, idle or not). Change it in
`hs2-menu` → tunnel → **Tuning** → **Link pool** (type `auto` or a number) or
with `hs2 config set max_links auto`; `hs2 recommend-links --why` shows what
this server's hardware gives (`-c <config>` adds what the carrier changes).

**What hundreds of links look like from outside — the owner's decision.**
Hundreds of simultaneous TLS connections between one fixed pair of IPs are
more unusual to an observer than a handful. The pool opens them only under
load, at most ~10 new handshakes a second (a 300-link ramp takes ~30 s), and
closes them again in quiet hours — but at peak they are all visible at once,
each with its own small periodic traffic: an smux keepalive every 4–8 s, a
control ping every 3 s on active links (less on idle ones), and a target change
from the Iran server spread over 1.5 s across the links. For dgtun the same
holds for its carriers: UDP flows, ping sessions (one echo identifier each —
at 300 carriers ~3,000 echo requests a second even when idle) or GRE/IPIP flows
on one IP pair. Lowering `max_links` trades peak capacity for a smaller
pattern; `hs2 doctor` and the Link pool screen say this whenever the
ceiling is above 64.

### Restarts and outages

- **Warm start.** The Iran side keeps its last target in
  `/run/hs2/<config>.warm`. A restart within 15 minutes comes back at that
  size (`link pool: coming up at N links, the size it had before this
  restart`); it only ever raises the start size, and it is written only after
  a minute of uptime, so a crash loop does not keep a high value alive.
- **Paced handshakes.** Every new link takes a turn from one gate per
  process: at most 8 handshakes in flight, starts spaced 40–160 ms — a
  restart under load is ~10 new links a second, never a storm. A few failed
  handshakes do not stop a ramp; only 3 in a row, or a failure with no link up,
  drop the queued dials until the next tick.
- **Outages.** While no link is up, the Kharej side (reverse) lets one slot
  retry (2 s connect, every ≤2 s) while the others wait; the first link is
  back ~3 s after the path returns (measured 2.8–3.2 s after a 40 s
  black-hole) and the rest follow at the gate's pace. Kharej log:
  `mtcp: no link up to the edge — dials fail (…)` when it begins, and
  `mtcp: a link to the edge is back after Xs with none up (K dial(s) failed
  meanwhile)` when it ends.
- **Refill hold.** After a start or a total loss, users reconnect within
  seconds while links come back at ~10 a second, and a connection stays on the
  link it was opened on. So for up to 10 s a new TCP connection waits for a
  link with room — fewer **open** connections than the fair share (open
  connections ÷ the links the pool wants, never below `per_link`). This cap
  counts every open connection and exists only during the hold; `per_link`
  stays the number of **active** users a link is sized for. When the pool has
  its links (on the reverse edge: as many as the Kharej server's max_links
  lets it dial), when no new link has come for 3 s, or after 10 s, everyone
  still waiting goes onto the existing links — **no connection is refused** — and the log says so plainly if that
  is more per link than the cap. With a pool of hundreds the hold normally
  ends at its limit (300 links take ~30 s at the gate's pace) and the line
  says so (`links open at the dial pace … — expected, not a fault`); only a
  pool well behind that pace is blamed on the path. Lines: `refill: …`; `hs2 status` /
  the live monitor / `hs2 doctor` show a `refill` line during the hold and
  for 5 minutes after. UDP flows are not held.
- **Stalled readers.** When users' apps stop reading and fill a link's whole
  receive buffer, every other connection on that link would stop too (and the
  other server's TCP would kill the link). hs2 resets only the connections
  whose app took nothing for 6 s while the buffer was full: `mtcp: reset N
  connection(s) on K link(s) whose app had taken nothing for 6s …`. Slow
  readers that still read are left alone. This is per server — **upgrade both
  servers** for both directions. A few stalled downloads spread over many
  links fill no link's buffer, but each holds megabytes of kernel socket
  buffers: twenty of them took a 2 GB server past the kernel's TCP memory
  pressure mark, where every socket is squeezed. So while either server is
  past that mark (each tells the other in its link stats) the stalled
  connections are reset without waiting for a full link (`reset N
  connection(s) whose app had taken nothing for 6s while kernel TCP memory
  was above its pressure mark …`), and no link is judged lossy or stuck
  (`kernel TCP memory on … is above its pressure mark — … none is judged`):
  every link resends and waits then, none of it theirs.
- **l3mtcp** side channel: a link whose session hears nothing for 12 s hands
  its TUN flows to the other links (it used to take 24–30 s).
- **dgtun** carriers closed on purpose are closed on the other side at once;
  a restarted peer's dead carriers are dropped as soon as a fresh one comes
  up; when every carrier is silent the dialing side sends one scout dial
  every 5 s; a retiring carrier's flows move off it after 30 s at the latest,
  so a shrink finishes even under a download that never pauses.

**What the guard cannot see** (documented, not changed): up to four streams
waiting on slow panel dials can hold a link for up to 5 s; an older hs2 on
the other server keeps its own unguarded buffers. (A UDP user flow whose
link is slow no longer stalls the other UDP flows on the same Iran port:
each flow has its own queue — 256 datagrams, 512 KB — and writer, and a
full queue drops, as a full UDP socket would.)

## Automatic kernel tuning

hs2 sizes kernel network tuning from the server's **RAM and CPU cores** and
re-applies it every time the service starts (so a resized VPS is picked up on
restart). It picks BBR + fq_codel by default, scales socket buffers and
backlogs to a low/medium/high profile, and sets the multi-IP-friendly knobs
and `tcp_tw_reuse=1` (outgoing connections — the dgtun forwarders, a panel not
on 127.x — may reuse TIME_WAIT ports instead of running out at a few hundred
new connections a second).
See exactly what it chose with `hs2 tune -c /etc/hs2/config.json`. It is fully
adjustable from `hs2-menu` → tunnel → **Tuning** (auto / manual with size
presets / off, and the congestion control and queue discipline), or in the
config's `"tuning"` section — nothing is hidden in a stray sysctl file.

## Certificate renewal without downtime

Let's Encrypt certificates renew 30 days before expiry (certbot's default,
so a few days with port 80 or Let's Encrypt unreachable never matter) and are
**hot-reloaded** (SIGHUP / `systemctl reload`) — new connections pick up the
fresh certificate while existing ones keep running, so a renewal never drops
the tunnel.

**Whether it renews by itself depends on how the certificate was issued:**

| issued with | renews | what it needs |
|---|---|---|
| Let's Encrypt **HTTP-01** (standalone) | automatically | port 80 free and reachable from the internet at renewal time (or a certbot `pre_hook` that frees it) |
| Let's Encrypt **DNS-01** (TXT record) | **by hand** — certbot cannot publish a new TXT record by itself | renew before it expires: tunnel manager → the tunnel → **c) Certificate → Renew now** |
| your own certificate | by you | replace the two files; hs2 switches to them within a minute, no restart |

Why this matters: the tunnel keeps working on an **expired** certificate (the
client authenticates with the shared key, not the certificate), so users never
notice — but every probe and browser then sees an expired certificate. To keep
that from happening silently, the tunnel screen shows each certificate's expiry
**and** how it renews (DNS-01, a busy port 80 or an overdue renewal are flagged
in yellow), **c) Certificate** can test the automatic renewal (a dry run that
changes nothing) or renew now, and `hs2 doctor` checks it too.

## Several tunnels on one server (service names)

Every tunnel is its own systemd service, so one server can run several side by
side — to different servers, or over different transports — and each one is
started, stopped, edited and deleted on its own.

- **Naming.** The server that *makes* the setup link asks for a service name.
  Enter keeps the default `hs2` (config `/etc/hs2/config.json`, as always); a
  name like `de1` runs as `hs2-de1` (config `/etc/hs2/hs2-de1.json`). The
  prefix means a tunnel can never overwrite an unrelated system service.
- **The other side needs nothing.** The name travels in the link, so the
  server that pastes it creates `hs2-de1` too — direct or reverse. If a tunnel
  with that name already exists there, it asks: *replace it* (the default when
  it talks to the same server — the same tunnel set up again) or *keep it and
  run this one under another name* (the default otherwise).
- **No clashes.** Each tunnel gets its own tun interface (`hs0`, `hs1`, …) and
  its own /30 in `10.77.0.0/16`, which also travels in the link so both sides
  agree. A link whose subnet is already used on the pasting server is refused
  with a clear message and nothing is changed there. A tcp/mtcp tunnel has no
  tun device and never reserves or deletes an interface name.
- **Setting a tunnel up again** (same name) asks before replacing it, keeps
  its subnet and interface, and stops only that tunnel; if the setup is
  cancelled halfway, it is started again on its old config.
- **Tunnel manager** (`hs2-menu` → 3) lists every tunnel by service name, with
  start / stop / restart / edit / logs / live pattern / tuning / diagnose /
  **certificate** / **delete**. Edit opens your own editor when `SUDO_EDITOR`,
  `VISUAL` or `EDITOR` names a terminal editor (vim, nano, …), else nano; the
  copy being edited — it holds the tunnel key — lives in a private directory
  that is removed afterwards together with any swap/backup files the editor
  made. The link-pool screen (Tuning → 6) says which server's values actually
  count: the Iran server decides the link count; in reverse the Kharej server
  also caps it at its own max; on a direct Kharej server the values do nothing.
  Delete asks you to type the name, saves a backup first, and removes only
  that tunnel's service, config and interface — the others keep running.
- **Upgrade** restarts every tunnel on the new binary and checks each one
  reconnects. **Backup / restore** cover all tunnels. **Uninstall** removes all
  of them (delete one in the manager) and what they left in `/run/hs2`, then
  asks whether to remove the `hs2` program and `hs2-menu` too — by default
  they stay, so setting up again needs no download. The certificate-renewal hook reloads
  every hs2 tunnel, so no renewal ever points at a deleted service.

## Several user ports, each to its own panel inbound (per-port targets)

The Iran server can open several user ports (`8443,2053,2083`). Each one can
reach its **own** panel inbound on the Kharej server — or all of them the one
default panel, exactly as before.

- **Who decides what.** The **Iran** server *opens* the user ports
  (`forward_ports`, and UDP on them). The **Kharej** server *decides where each
  one goes*: a port with its own target (`port_map`) goes there, every other
  port goes to the default panel (`expose`); with no default, a port without
  its own target is refused (and the Kharej server says which, in its log and
  in `hs2 status`).
- **The default is the same port.** A `port_map` entry `2053` means
  `127.0.0.1:2053` — the same port on the Kharej server; `2053=10.0.0.5:443`
  sends it anywhere else. Example Kharej config:
  `"expose": "127.0.0.1:8443", "port_map": "2053,2083=127.0.0.1:2096"`.
- **Why the table lives on the Kharej server.** On every user connection the
  Iran server says only *which of its user ports* the user came in on — a
  number. It never names an address, so a compromised Iran server can reach
  only what the Kharej operator listed, never `127.0.0.1:22` or a database.
- **Setup** asks it on the Kharej server: the default panel, then *"Iran user
  ports with their OWN inbound here"* (Enter = none, all to the default).
- **Tunnel manager → `p) Ports`**, on either server, shows the whole table as
  that server knows it — its own half from its config, the other half as the
  other server reported it over the live link — and edits this server's half:
  on the Iran server add / remove user ports and turn UDP on or off; on the
  Kharej server give ports their own target (Enter = the same port on
  127.0.0.1), remove one, or change the default panel. After every change it
  says what — if anything — has to change on the other server, and applies it
  with the usual restart and automatic rollback. `hs2 status` shows the same
  table, `hs2 doctor` warns about a user port the Kharej server has no target
  for and about a target nothing listens on, and `hs2 ports -c <config>
  [add|remove|default|udp …]` is the command-line form.
- **Every transport with user ports, both directions, TCP and UDP:** mtcp,
  l3mtcp, tls and the datagram tun (udp/icmp/gre/ipip/ipx), direct and
  reverse; UDP (when forwarding UDP is on) reaches the same target as TCP.
  The port number costs 2–3 bytes once per connection (inside TLS on the TCP
  transports); on a datagram tun it rides on each UDP datagram only when the
  Kharej server has a `port_map` (or no default), so a tunnel without one sends
  its datagrams exactly as before. On a datagram tun the per-port traffic uses
  the tunnel-internal port 28444 on the Kharej server's tun address (28443 for
  the rest) — if a firewall there filters it, both servers say so and every
  port keeps reaching the default panel.
- **Mixed versions keep working exactly as before.** A newer Iran server in
  front of an older Kharej server: every port reaches its one panel (the
  Kharej side needs the new build for per-port targets — `hs2 status` on the
  Iran server says so). An older Iran server in front of a newer Kharej server:
  it does not say the port, so every connection reaches the default panel. An
  existing config without `port_map` behaves exactly as it always did.

## Datagram tunnels on throttled paths (tun over udp / icmp)

All carriers of a datagram tunnel go to the same server IP, and a policer on
the way (an ICMP rate limit, a per-IP throttle) sees their **sum**. hs2 watches
the pool as a whole: loss episodes far above the pool's usual loss that hit
most carriers at once with no queue building are a policer's signature (steady
random loss is the usual loss itself; congestion shows a queue first).

- Two such episodes within 30 s start a **test**: the whole pool is capped at
  90% of what got through on average, and stepped down on further episodes.
- If the episodes stop under the cap (a clean spell twice the old gap), the
  policer is **confirmed**: the pool is held there, re-probed 5% every 10 s,
  FEC parity is sized for the path's own loss (not the policer's drops, which
  more parity would only feed), parity counts inside the cap, and the
  autopilot adds no links for it.
- If the episodes keep their old rhythm even with the cap well down, the loss
  does not depend on our rate (a flapping path): the cap is lifted and the
  next test waits 5 min, then 10, 20 … up to an hour.

The log says what happens (`dg: policer suspected … testing`, `policer
confirmed`, `lowered to …`, `cap lifted`, `FEC at its ceiling …`, `cpu: … the
CPU is the bottleneck`).

`hs2 status -c <config>` and the tunnel manager show, per tunnel: loss of what
this side sends (pool-wide and the worst carrier), FEC parity and what it
rebuilt / lost, the policer cap when one is active, drops (pacer, receive
queue, tunnel queue) and the daemon's CPU use. The same fields are in the live
status file under `/run/hs2/` (`loss_pct`, `max_loss_pct`, `parity_pct`,
`fec_at_ceiling`, `fec_recovered`, `fec_lost`, `pacer_dropped`, `rx_dropped`,
`tun_drops`, `policed`, `police_confirmed`, `police_cap_mbit`, `cpu_pct`, `cpu_cores`), plus packet counts by stage — `tun_read`, `sent_pkts`, `recv_pkts`, `tun_written` — drops by reason (`drop_no_carrier`, `drop_queue_full`, `drop_aged`) and one `carriers` line (`id:state:sent/loss%` per carrier), so a field test can see exactly where packets are lost.

Each inner flow is pinned to one carrier for as long as it lives (a pool resize never moves a live flow), so inner TCP never sees reordering from the pool.

**Ping under load.** Each carrier's send queue is fair across its flows and
serves interactive ones first: a flow with nothing queued and under 256
kbit/s lately (a game, a call's audio, DNS, ping, a remote shell) goes ahead
of the downloads, and through a fast lane in the carrier's pacer; downloads
share the carrier by bytes, and a full queue drops from the flow with the
most queued. On a 20 Mbit/s bottleneck with 8 downloads, ping through the tun
went from 66 ms (p50) to 12 ms at the same throughput. `HS2_DG_FQ=0` turns it
off (one FIFO per carrier, as before).

**Carriers share the bottleneck.** All carriers of a pool go between the same
two servers, so they meet at the same bottleneck and each one's delay signal
is the queue they all build. Three rules keep them from fighting over it: a
carrier leaves its fast start once a queue has stood for ~300 ms while it
carries traffic (before, a carrier whose users' TCP filled the path first
could stay unpaced, and the queue then sat in the bottleneck instead of in
the fair queue above); every busy carrier grows by the same small step of the
pool's fair share, so a carrier that came late is not left with a sliver;
and the carriers' base-delay probes fall on one shared clock, so the queue
really empties when they measure. Behind a 30 Mbit/s bottleneck with 8
downloads over 4 carriers, ping through the tun under load was 16-19 ms
(p50) and at most 37 ms (p99) in every run, where it had ranged from 11 to
70 ms with some runs losing pings. `HS2_FAIR_SHARE=0` turns the three rules
off.

**Less CPU per gigabyte.** On a small server the datagram tun's limit is
usually the CPU, and most of it went on system calls: one per datagram on the
socket and one per packet on the tun. Now:
- datagrams go and come several per system call (`sendmmsg`/`recvmmsg`) on
  the raw and udp sockets;
- the tun is opened with TCP offload: the kernel hands hs2 one TCP packet of
  up to 64 KB, which hs2 cuts into the usual MTU-sized packets itself, and on
  the other server consecutive packets of one connection are written to the
  tun as one. Nothing changes on the wire, and an older hs2 on the other
  server works with it.

`hs2 status` shows it: `tun: TCP offload on · N reads gave M packets (41.3
each) · … writes carried … packets (21.6 each)` (or why it is off), plus any
malformed packets the kernel handed over and merged packets it refused (those
go in one by one). In the lab (icmp, reverse,
2 cores a side) a download went from ~450 to ~650 Mbit/s with 27-39% less CPU
per GB on each server. `HS2_TUN_OFFLOAD=0` and `HS2_RAW_BATCH=0` turn each
part off; a kernel that refuses the offload gets plain packets (the start
line says so). `HS2_PPROF=127.0.0.1:6060` serves CPU profiles on the loopback
address for measuring a busy server.

**A carrier cut on its own heals in about a second.** The other server sends
feedback on every carrier ten times a second, so a carrier that has heard
nothing for 1 s while another carrier still hears the other server has lost
its own way through (its icmp echo id or its port dropped on the path, its
state lost on the other server). New flows avoid it, its flows move at their
next packet, and the other server is told so its flows leave it too (a cut in
one direction only heals on both sides); at 3 s it is closed and replaced.
The log says `dg: carrier N has heard nothing from the other server for 1.2s
while 5 other carrier(s) still do — …` and `dg: carrier N heard nothing for
3.2s — closed; a new carrier replaces it`; the `carriers` line flags it `M`
and `hs2 status` counts the closed ones (`mute_closed`). When no carrier hears
the other server it is the path or the other server, not one carrier: then
the pool dials a scout every 5 s instead.

## How the icmp tunnel looks on the wire (and what it cannot hide)

The icmp encapsulation is shaped so a passive or stateful classifier cannot pick
it out of ordinary ping by its *form*:

- **No fixed constant in the carrier header.** There is no framing magic. The
  carrier's own structured header — its tag, wire sequence and FEC fields — sits
  after an 8-byte per-packet nonce and is XOR-masked with a ChaCha20 keystream, so
  on the wire it is never a stable value or a readable counter. Above it, the ICMP
  **id** and **echo sequence** stay in the clear *on purpose*, shaped to look
  exactly like an ordinary ping: a per-link id (like a ping's pid), and a sequence
  whose high byte is a keyed per-direction tag and whose low byte is a small
  ascending counter. Direction is told apart by that keyed sequence byte — not the
  id, which NAT may rewrite.
- **Ping-like exchange.** Requests carry DF=1 and replies DF=0, exactly as the
  kernel's own ping does. Each reply takes its own ascending echo sequence (never
  a repeat), and the pool keeps roughly **one reply per request** like a real
  ping: whichever direction is light is topped up with cheap filler echoes — a
  request on the side that sends requests, a reply on the side that sends replies
  — so the heavy, data-carrying direction pays nothing (this holds in both direct
  and reverse).
- **Size.** Small packets, keepalives and fillers are padded to a handful of size
  buckets, so a frame's size no longer tracks its payload. Bulk packets (already
  near the MTU) are left as they are, so the bulk direction carries no padding
  overhead. This applies to every datagram encap (udp/icmp/gre/ipip/ipx); set
  `HS2_DG_PAD=0` to turn padding off entirely for the last few percent of goodput.

The data itself is always end-to-end AEAD-encrypted (ChaCha20-Poly1305); the
obfuscation is cosmetic and keyed separately — it only removes patterns, it is
not the security boundary. This release changed the icmp wire format: it is **not
compatible with older hs2 icmp tunnels**, so upgrade both ends together.

**What it cannot hide — volume.** You cannot move real throughput and still look
like an ordinary ping. A normal ping is a slow trickle of tiny, low-entropy
packets; encrypted bulk is a stream of large, high-entropy ones. Framing erases
*patterns*, not raw rate, so a single server IP exchanging ICMP echo with one peer
at hundreds or thousands of packets per second is itself the tell — no amount of
header shaping changes that. The real mitigation is **deployment, not framing**:
spread the load across several server IPs so no single IP pair carries a
ping-unlike rate — `bind_local_ip` and multiple exit IPs let you split it across
IPs (several carriers to one IP do not help here, since they share the pair —
which is why a tun over icmp runs at most **8** carriers unless `max_links`
fixes a number: past that, more echo ids to one host add no bandwidth, only a
pattern ping never makes). And
reserve icmp for paths that pass *only* ICMP — where a path also passes udp or
tls, those carry far more per IP without pretending to be ping.

## What the installer checks for you

- **"Ready" means packets cross.** The server that pastes the link starts
  second, so the installer waits (up to 40 s) until the tunnel really carries
  traffic — a live authenticated link, or the other side's tun IP answering —
  before it says ready. If nothing comes back (typical: GRE / IP-in-IP / a raw
  protocol dropped on the path) it says so, names the likely cause and exits
  with an error; the service stays installed and connects by itself if the
  path opens later.
- **No tunnel port for raw encapsulations.** tun over icmp / gre / ipip / ipx
  rides a bare IP protocol, so no port is asked or put in the link.
- **The icmp tunnel keeps normal ping working.** Only the kernel's replies to
  the tunnel's own packets are dropped — by an nftables rule, or iptables — so
  the server keeps answering ordinary ping. If neither nftables nor iptables is
  usable it falls back to `net.ipv4.icmp_echo_ignore_all=1`, which *does* silence
  all ping replies while the tunnel runs (it logs a warning asking you to install
  nftables; `HS2_ICMP_SUPPRESS=nft|iptables|global` forces a method). The rule is
  removed when the tunnel stops; one left by a killed daemon is removed by the
  next start or by `hs2 cleanup` (never another running tunnel's). The kernel
  still builds each reply before the rule drops it (a copy of every tunnel
  packet received as an echo request — in reverse, the download on the Iran
  server); on a server that runs only the tunnel, `systemctl edit <service>`
  with `Environment=HS2_ICMP_SUPPRESS=global` stops it answering ping at all
  and saves that work.
- **The download is checked** against `hs2-linux-amd64.sha256` (catches a
  broken or altered download; it is not a signature — protect the GitHub
  account with 2FA, and pin a reviewed commit with
  `HS2_REPO_RAW=https://raw.githubusercontent.com/<owner>/<repo>/<commit>` if
  you need that). The full sha256 is printed so both servers can be compared.
- **Backups** (`/root/hs2-backups`, root-only, they contain the tunnel key)
  keep the newest 10 (`HS2_KEEP_BACKUPS`). Uninstall stops only this tunnel's
  process, never another hs2 tunnel on the same server.

## What changed in v3

- **One data path for all modes.** `tls` and `l3mtcp` used to carry IP packets
  (TCP inside TCP) and built multi-second queues under load; they now use the
  stream core. The tun interface remains in those modes as a **side channel**
  for ping and light non-TCP traffic, with a 60 ms queue-time limit — it is
  not a bulk path: under load it drops (the log then says `l3: dropped N
  packets in 30s on the tun side channel`), while the user ports, which ride
  the streams, are unaffected. For bulk traffic over a routed tun, use tun
  over udp/icmp (the datagram pool).
- **Connections spread across links.** Connections that arrived together all
  landed on the first link, so per-connection throttling capped the whole
  burst. They are now spread evenly.
- **Latency tuning measured in a lab** (lossy, throttled, long-haul paths):
  BBR on every link socket, a 32 KiB unsent-data limit, 16 KiB stream frames,
  and an adaptive link pool (2 up to a hardware-sized ceiling) sized live by
  the autopilot (see above).
- **Authenticated links.** Both ends prove the shared key bound to the exact
  TLS session (TLS exporter), so an interceptor with a forged certificate can
  neither read nor hijack the tunnel. v2 had no protection against that.
- **UDP forwarding** (`"udp": true`), `bind_local_ip` and `user_listen_ip`
  now work in every mode.

Lab results, v2 → v3 (median of 3 runs; 80–120 ms RTT; "throttled" = 0.5% loss
and a 10 Mbit/s cap on every single connection):

| path, mode           | throughput (Mbit/s) | latency under load (ms) | new connection (ms) |
|----------------------|---------------------|-------------------------|---------------------|
| throttled, `mtcp`    | 37.3 → **47.2**     | 234 → **202**           | 340 → **329**       |
| throttled, `l3mtcp`  | 34.8 → **47.1**     | 256 → **203**           | 526 → **343**       |
| clean 50 Mbit, `tls` | 42.2 → **46.3**     | 201 → **114**           | 399 → **138**       |
| slow 8 Mbit, `mtcp`  | 7.5 → **7.6**       | 1078 → **628**          | 1576 → **394**      |

`tls` is a single link, so under per-connection throttling it cannot exceed
the per-connection cap; use `mtcp` there.

## Requirements

- Two Linux servers (tested on Ubuntu 22.04+), root access.
- **TLS modes only** (`mtcp` / `l3mtcp` / `tls`, and tun-over-tcp): a domain whose
  A record points to the **kharej** server, and port 80 free on kharej during the
  first install (for the certificate) — or an existing Let's Encrypt certificate.
  The `udp` / `auto` transports and the datagram tun encaps (udp/icmp/gre/ipip/ipx)
  need **no domain and no certificate** — they authenticate with the shared key.
- A free tunnel port on the kharej server (default 2096) for the port-based modes;
  the raw encaps (icmp/gre/ipip/ipx) ride a bare IP protocol and use no port.

## Install

### 1. Kharej (foreign server)

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/main/install.sh)
```

Choose **1**, pick a transport, then answer its prompts — for a TLS transport:
domain, tunnel port, panel inbound (the default for every user port, plus
optional per-port targets), mode, UDP; for a datagram transport
(`auto`/`udp` or a raw tun encap): the encapsulation and tunnel settings (no
domain or certificate). At the end it prints a **`hs2://…` setup link** — copy it.

### 2. Iran server

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/main/install.sh)
```

Choose **2**, paste the `hs2://` link, pick the user port(s).

> If GitHub is unreachable from the Iran server, copy `hs2-linux-amd64` and
> `install.sh` from the kharej server into `/root/hs2/` and run
> `bash install.sh` there; the installer falls back to the local binary.

### 3. Point clients at Iran

In your panel, take a client config and change only its **address** to the Iran
server's IP (and port, if you chose a different one). Everything else — UUID,
SNI, security — stays the same.

## Upgrade an existing install (one command)

v3 is **not** wire-compatible with v2, and this release also changed the **icmp**
wire format — so both ends of a tunnel must run the same build. Upgrade the
**kharej server first, then Iran**; the tunnel is down only between the two.
Config and the `hs2://` link stay the same:

```bash
curl -fsSL https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/main/install.sh | bash -s upgrade
```

If the Iran server cannot reach GitHub, copy the new binary from kharej first
(`scp /usr/local/bin/hs2 root@IRAN_IP:/root/hs2/hs2-linux-amd64`), then on Iran
run `cd /root/hs2 && bash install.sh upgrade`.

Existing configs keep their mode. To switch, edit `"carrier"` in
`/etc/hs2/config.json` on both servers and `systemctl restart hs2`.

## Stream modes (TLS)

These ride TLS and need a domain + certificate (see Requirements). The installer's
recommended transport is **`auto`** (it probes udp first, then falls back to
tcp/TLS); these are the TLS carriers it can land on:

| mode     | links     | hs0 tunnel IPs | use it when                              |
|----------|-----------|----------------|------------------------------------------|
| `mtcp`   | 2–ceiling | no         | fastest TLS mode, beats per-connection caps  |
| `l3mtcp` | 2–ceiling | yes        | you also need 10.77.0.x (ping, non-TCP)      |
| `tls`    | 1         | yes        | you want a single connection on the wire     |

For the **datagram** transports — `auto` / `udp`, and tun over udp/icmp/gre/ipip/ipx
(no domain, no certificate) — see *Datagram tunnels on throttled paths* above.

On a slow path that is **not** throttled per connection, fewer links give
lower latency under full load, because several parallel flows keep a standing
queue in the path. The autopilot handles this automatically — it keeps added
links only when they raise throughput — but you can also cap it by lowering
`max_links` (on the Iran server; in reverse mode the lower of the two servers'
applies, so either one caps it).

## Security

- TLS 1.3 with a real Let's Encrypt certificate; the client looks like Chrome.
- Links are authenticated in both directions with HMAC-BLAKE2s over the shared
  key and the TLS session's exporter secret. The client sends no traffic until
  the server has proven the key, so certificate forgery or interception gets
  nothing. Auth records are padded to normal HTTP sizes.
- The port shows **one consistent identity** to every unauthenticated probe —
  an ordinary HTTPS web server — so no single probe stands out. A completed TLS
  handshake that is not an authenticated hs2 client (a browser, a probe, a short
  or malformed request) is served a plain, self-contained cover website; a
  plain-HTTP request on the TLS port gets the exact *"Client sent an HTTP request
  to an HTTPS server"* reply a real HTTPS server gives (not a silent close); and
  anything that is neither TLS nor HTTP is closed just as a TLS server closes on
  garbage. No response reveals the tunnel.
- The built-in cover page is **different on every install**: a per-install
  random seed (`cover_seed`, written by the installer and never derived from the
  key) varies its brand, text, colours, layout and size, so no two servers share
  a page hash and a bulk scan cannot find every hs2 server by one known hash.
  The seed is stable, so a server shows the same page across restarts. This
  defeats cheap hash/structural enumeration; it is **not** a disguise against a
  determined prober (the TLS stack is still Go's, and a classifier trained on
  several pages could still recognise the family). For the strongest cover, set
  `backend_addr` in the config to a real local web server of your own — probes
  are then served your own site and `cover_seed` is ignored.

## Managing

```bash
hs2-menu           # 3 = tunnel manager, 4 = status/logs, 5 = upgrade, 8 = uninstall
systemctl status hs2
journalctl -u hs2 -f
```

- **`hs2 doctor -c /etc/hs2/config.json`** — an on-box health check: config
  validity, whether the tunnel is running, endpoint reachability (the common
  "edge can't reach exit" failure), certificate expiry **and whether it will
  actually renew** (renewal method, the HTTP-01 port and any certbot pre-hook
  that frees it, an overdue renewal, the certbot timer or cron job), the tun device, kernel tuning vs. what is actually applied,
  and a clock reminder (link auth is minute-bound). Also in the tunnel manager
  as **Diagnose**. It only reads — safe to run any time (it never runs certbot).
- **`hs2 ports -c /etc/hs2/config.json`** — the user ports and where each one
  goes, as this server knows it (see *per-port targets* above); with `add`,
  `remove`, `default` or `udp` it changes this server's half (also in the
  tunnel manager as **Ports**).
- **`hs2 version`** prints a build stamp (`… [build <rev> <date>]`); compare it
  on both servers to confirm they run the same build (the `hs2-menu` banner
  shows it too).

## The three ports (they are different things)

| where | what | example |
|-------|------|---------|
| Iran | user port(s) clients connect to (`forward_ports`) | 8443, 2053 |
| Kharej | panel inbound the tunnel delivers to (`expose`, the default) | 127.0.0.1:8443 |
| Kharej | a user port's own panel inbound (`port_map`, optional) | 2053 → 127.0.0.1:2053 |
| Kharej | tunnel port Iran dials (clients never see it) | 2096 |

The Iran user port and the kharej panel port may share a number (different
servers); the tunnel port must differ from the panel port on the kharej server.

## Notes

- The installer applies BBR/fq kernel tuning system-wide
  (`/etc/sysctl.d/99-hs2.conf`); hs2 also sets BBR on its own sockets.
- Building from source, the architecture, and the test lab: `hs2-src/BUILD.md`.
