# hs2-tunnel

A DPI-resistant tunnel between an Iran server and a foreign (kharej) server.
Runs **alongside Backhaul** without touching it — its own ports, its own subnet.

**v3 — stream core over multi-link TLS.** Every mode now works the way `mtcp`
did: a user's TCP connection ends on the Iran server and its bytes ride an
smux stream over a pool of real TLS 1.3 links (Chrome fingerprint, real
certificate, active-probe resistance). The kharej server opens its own
connection to the panel. There is never TCP inside TCP.

**Operations.** Two read-only helpers for day-to-day running: the **server doctor**
(`hs2 doctor --deep [--lang fa]`, menu **9) Server doctor**) says what is wrong on
this server and what to do, and **alerts** (`hs2 alert`, tunnel manager
**a) Alerts (Telegram)**) watch each tunnel and send short alerts plus a daily
summary. See [Managing](#managing).

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

`max_links` caps the pool. It is a **soft cap** for live links: the pool
never *dials* past it, but with `HS2_RETIRE_KEEP_IDLE` (on by default on
the reverse `l3mtcp` edge) a link that is retiring stays up while it still
carries live idle user connections (reclaimed after 60 m idle), so for a
while after a shrink `links:` can show more links up than `max_links`. The
retiring links above `max_links` with the fewest connections still follow the
old 5-minute rule, and the reverse listener accepts up to about twice
`max_links` plus a margin, so the overshoot is bounded and short-lived. It
has three forms:

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
  `/run/hs2/<config>.warm`. A restart within an hour comes back at that
  size (`link pool: coming up at N links, the size it had before this
  restart`), within 3 hours at half of it; it only ever raises the start size, and it is written only after
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

**Required: `net.ipv4.tcp_mtu_probing=1`.** A path that drops large packets
without sending ICMP back (an MTU blackhole) stalls
every download on it with `0`; with `1` the kernel probes a smaller MSS only
after such a stall (one stall of 4–13 s per link, then it recovers; mt153).
`hs2 run` sets it as root together with the rest of the tuning, unless tuning
is `off` or `HS2_NO_TUNE` is set — then set it yourself
(`sysctl -w net.ipv4.tcp_mtu_probing=1` and a line in `/etc/sysctl.d/`).
`hs2 doctor` warns when it is `0` (and when it is `2`: probing on every
connection costs throughput).

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
- **Queue control with two reverse `l3mtcp` tunnels on one server.** The queue
  controller's tc shaper (`HS2_QUEUE_CTL` / `HS2_QUEUE_CTL_UP`, on by default for
  reverse `l3mtcp` since rel69a) is per network device, not per tunnel: a second
  tunnel on the same device joins the first one's tree (its own class `1fe1:N`
  + cake under the same htb), runs tc on that device every 10 s, and leaves its
  class behind when it stops (no filter points at it, so it is harmless, but it
  changes the other tunnel's tree). Keep the shaper on one tunnel per server and
  turn it off on the others with a drop-in, e.g.
  `/etc/systemd/system/hs2-out.service.d/60-noshaper.conf`:
  `[Service]` / `Environment=HS2_QUEUE_CTL=0` / `Environment=HS2_QUEUE_CTL_UP=0`
  (both servers; this is how the reference deployment runs its second tunnel).
  The installer does not add this drop-in by itself.

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
rebuilt / lost, the policer cap when one is active, drops (no carrier, carrier
queue full, waited >50 ms, refused by the kernel, receive queue), and the
daemon's CPU use next to the whole server's. The same fields are in the live
status file under `/run/hs2/` (`loss_pct`, `max_loss_pct`, `parity_pct`,
`fec_at_ceiling`, `fec_recovered`, `fec_lost`, `send_refused`, `rx_dropped`,
`tun_drops`, `policed`, `police_confirmed`, `police_cap_mbit`, `cpu_pct`,
`cpu_cores`), plus packet counts by stage — `tun_read`, `sent_pkts`,
`recv_pkts`, `tun_written` — drops by reason (`drop_no_carrier`,
`drop_queue_full`, `drop_aged`) and one `carriers` line with an entry per
carrier: `id:state:sent/loss% rRATE/bwBTLBW FLAGS sSENT qQUEUE/SRTT` —
packets this side handed the carrier, the loss of what it sends as the other
server reports it, its pacing allowance `r`, the most it was seen to deliver
`bw` and what it really sent `s` (Mbit/s), the flags `P` (it sent at least
80% of its allowance at the last report), `S` (still in its fast start), `C`
(its send stage — the CPU or socket — not the path held it back lately), `M` (mute:
it, or the other server, hears nothing on it) or `-`, and the queue
its rate control sees on the path and its smoothed round trip (ms). Above 32
carriers the line gives the counts and the 10 with the most loss. In the fast
start `r` is an allowance, not a measurement: a carrier flagged `S` without
`P` keeps the last value it reached, so `r` far above `s` there means the
sender sent less than it was allowed (short of data), not that the path
takes it; a carrier short of CPU shows `C` instead, its `r` within about
twice its `s`. So a field test can see exactly where packets are lost.

**Where the send stage waits.** `hs2 status`'s `sending:` line (status
file: `send_mbit`, `send_held_pct`, `fq_wait_ms`, `write_us`, `per_write`)
covers the last couple of seconds: what the carriers' pacers sent, the share
of the time a carrier's writer waited for room in its pacer, the mean wait in
the fair queue in front of it, and the mean socket write (the system call —
and the shared socket's lock on gre, ipip and ipx, or with `HS2_RAW_TX=0`)
with the datagrams each carried. `pool:`
(`share_mbit`, `busy_queue_ms`) is the fair share per busy carrier and the
queue those carriers see. Writers wait for pacer room whenever the carriers
are the limit — on a full path as much as on a sender short of CPU (lab:
55-74% of the time either way); what tells the two apart is the carriers:
a path-bound pool shows `P` (`s` close to `r`) and a `q` of 5-20 ms, a sender
short of CPU `C` without `P` (`s` well under `r`), `q` near 0 and the server
saturated on the `cpu:` line. On a saturated server a carrier flagged `C` is
paced less tightly: the scheduler puts its send goroutine aside for tens of ms
at a time when another program (a busy panel, another tunnel) holds a core, and
the ordinary pacing then let it drain only a couple of ms of backlog in the
slice it got, so it sent well under its rate (a 12-27% throughput loss against
the pool rules off). It now keeps a larger catch-up while saturated, so it fills
that slice; interactive packets still take the fast lane, so their latency is
not the cost. A residual gap to the rules-off path remains — the price of not
over-committing the rate, which would flood a shared path when the CPU frees.

**The server's CPU.** hs2's own CPU (`cpu_pct`, % of one core) does not say
whether the server has any left: a 2-core server shared with another tunnel
can be saturated while hs2 shows 60%, and then every carrier sends late
however good the path is. So the `cpu:` line also shows the whole server —
busy across all cores, softirq and steal (`host_cpu_pct`,
`host_softirq_pct`, `host_steal_pct`, `host_cores`) and the share of the
time tasks waited for a core (`psi_cpu10`, `psi_cpu60`: the kernel's CPU
pressure, absent on kernels without it) — and says `SATURATED` when the
server stays at 90% busy or 40% waiting (`host_saturated`; the log says when
it starts and ends). `net:` (`host_out_discards`) counts the IP packets the
kernel discarded on output since hs2 started, for the whole server and every
program on it: a full interface queue, a firewall's drop — and, on an icmp
listener, the kernel's own echo reply to every tunnel packet received, which
the tunnel suppresses on purpose. So it is shown, never alarmed on. A raw
socket's queue drops are silent (the kernel tells the sender they were
sent); over icmp the carriers' send-only socket asks the kernel to report
them, and `drops:`'s "refused by the kernel" (`send_refused`) counts this
tunnel's own exactly. `hs2 doctor`
measures the server for a second (`server cpu`) and warns when it is short
of CPU.

Each inner flow is pinned to one carrier for as long as it lives (a pool resize never moves a live flow), so inner TCP never sees reordering from the pool.

**Ping under load.** Each carrier's send queue is fair across its flows and
serves interactive ones first: a flow with nothing queued and under 256
kbit/s lately (a game, a call's audio, DNS, ping, a remote shell) goes ahead
of the downloads, and through a fast lane in the carrier's pacer; downloads
share the carrier by bytes, and a full queue drops from the flow with the
most queued. On a 20 Mbit/s bottleneck with 8 downloads, ping through the tun
went from 66 ms (p50) to 12 ms at the same throughput. `HS2_DG_FQ=0` turns it
off (one FIFO per carrier, as before).

A high ping *on the client* while it downloads (a speedtest's "loaded" or
"download" ping) is usually the user's own access link — the LTE/Wi-Fi
router or modem queueing its own download (bufferbloat) — not the tunnel.
To tell them apart, ping the kharej server from the **Iran server** at the
same time: if that stays flat while the client's ping climbs, the queue is on
the client's side of Iran.

**Carriers share the bottleneck.** All carriers of a pool go between the same
two servers, so they meet at the same bottleneck and each one's delay signal
is the queue they all build. Three rules keep them from fighting over it: a
carrier leaves its fast start once a queue has stood for ~300 ms while it
carries a real part of the traffic (before, a carrier whose users' TCP
filled the path first could stay unpaced, and the queue then sat in the
bottleneck instead of in the fair queue above); while a queue stands every
busy carrier grows toward the pool's fair share, so a carrier that came late
is not left with a sliver; and the carriers' base-delay probes fall on one
shared clock, so the queue really empties when they measure. Behind a 30
Mbit/s bottleneck with 8 downloads over 4 carriers, ping through the tun
under load was 17-19 ms (p50) and at most 34 ms (p99) in every run, where it
had ranged from 11 to 114 ms with some runs losing up to a quarter of the
pings.

A fourth rule is for a sending server short of CPU, where the path is not
the limit at all. Its carriers' pacers cannot keep up with their own rate, so
no carrier ever used its allowance: all stayed in their fast start, with an
allowance up to 18 times what they sent. Now a carrier that does not use its
allowance while its send queue drops packets, or its writer waits for room
in its pacer for a quarter of the time or more, is held back by its send
stage (flag `C` in the `carriers` line): it leaves its fast start, its
allowance stays within twice what it really sends, its pacer keeps the
credit of a late wake-up (up to 10 ms) instead of losing everything past
2 ms, and once the CPU lets go it regrows 25% per report until a queue
appears. In the lab, with the sender held to 0.6 of a core, that carried 27%
more (212 → 270 Mbit/s) with about 23% less CPU per Mbit/s; a path-bound pool is
unchanged. `HS2_FAIR_SHARE=0` turns all four rules off.

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

Over icmp, the carriers to one server send through a send-only raw socket of
their own beside the shared receive socket, without the shared socket's lock:
on it every carrier waited for the others (Go's lock, then the kernel's), and
on a busy server one carrier's writer, put aside by the scheduler while
holding them, held all the others back. The packets on the wire are the same
bytes as before (the outer IP header is built from the receive socket's own
settings). A datagram too big for the device without DF still goes out
fragmented through the shared socket; if the send-only socket ever fails, its
carriers go back to the shared one (logged once). Over a udp listener, the
batch sends drop Go's lock the same way. `HS2_RAW_TX=0` turns both off.

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

- **Stronger timing/id camouflage, opt-in (`HS2_ICMP_CAMO=1`, set on BOTH
  ends).** The default shaping above leaves two timing tells a stateful
  classifier could key on: a fixed ~10 Hz feedback beat (at idle, the only
  packets on the wire) and a fixed ~0.25 Hz dip as the whole pool base-probes
  together every 4 s. With this flag the feedback cadence is jittered and an
  idle carrier falls quiet (keepalive, ~5 s jittered, keeps the link alive), so
  a near-idle tunnel stops beaconing; the shared probe schedule is jittered off
  its fixed grid without desynchronising the pool (so the min-RTT measurement
  stays clean); and link ids are drawn as a pid-like ascending cluster rather
  than uniform-random 16-bit values. It changes only SEND TIMING and the id
  draw — not the wire format — so it needs no agreement with the peer and an
  old peer is unaffected (but set it on both ends to shape both directions).
  Honest limits: during an active download the receiver→sender feedback
  direction is mostly feedback, so its cadence is only jittered, not removed;
  and the per-flow sequence tag and the request/reply correlation are NOT
  changed — making the reply echo the request (full ping correlation) was
  evaluated and deferred because every variant trades one tell for another
  (a new id mismatch, or silencing the host's own ping) while the filtering
  seen in the field keys on size and volume, not sequence correlation.

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
- `net.ipv4.tcp_mtu_probing=1` on both (hs2's tuning sets it when it runs as
  root; `hs2 doctor` warns when it is not 1).
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

The upgrade first saves a backup (`/root/hs2-backups/`: binary, units, their
drop-ins, configs, certificates) and then restarts each tunnel and checks the
**running** binary is the new one. A drop-in that runs another binary
(`ExecStart=/usr/local/bin/hs2-r31 run -c …`, common on hand-tuned servers) is
pointed at `/usr/local/bin/hs2` with the same arguments; the old file is kept
as `override.conf.pre-upgrade`. Check with `systemctl cat hs2` and
`readlink /proc/$(systemctl show -p MainPID --value hs2)/exe`. A server where
rel52's installer turned an alert check (`hs2-alert-<unit>.service`) into a
second tunnel daemon is healed by the next upgrade (rel53): the unit is the
alert check again and only one `hs2 run` per config is left. Upgrade with
rel53's `install.sh`, not rel52's.

**Roll back** the binary and units to before the upgrade:
`cd /root/hs2 && bash install.sh restore` (the newest backup; or name a file
from `/root/hs2-backups/`). **Uninstall:** `bash install.sh uninstall`.

Existing configs keep their mode. To switch, edit `"carrier"` in
`/etc/hs2/config.json` on both servers and `systemctl restart hs2`.

### Upgrading from r31 / r34 (rel40, rel52, rel53)

An r31 or r34 config runs **unchanged** on rel40: no new field is required,
nothing is rewritten, and a config without a `front` keeps the TLS front on
every IP (Reality is used only after you turn it on per IP, `hs2 reality
front` / new menu → 4). Behaviour changes only where a **new default** applies.

What changes for an existing deployment after the upgrade:

| what | where | before (r31 / r34) | after (rel52 / rel53 / rel54) |
|------|-------|--------------------|---------------|
| `HS2_LB_JUDGE_DATA` — the LB judge counts only dials that failed before TCP connected (a front's handshake failure no longer lowers a path) | both sides | off | **on** |
| `HS2_LB_RESCUE` — a light user whose only sub sits on a lowered / cut path gets a copy on a good path, at once | Iran (edge) | off | **on** |
| `HS2_LB_RESCUE_CALLFREE` — those rescue copies stay off the links a call is using | Iran (edge) | off | **on** |
| `HS2_LB_RESCUE_CUT` — a light user on a path the judge holds CUT (not merely lowered) gets its copy past the budget even while raw, and a dual one gives its sub on the cut path up through the close handshake (own bound, `HS2_LB_RESCUE_CUT_PER_SEC`, 600/s; stuck59, default since stuck61) | Iran (edge) | off | **on** |
| `HS2_LB_RESCUE_CUT_L3` — the TUN lane (ping, games, small packets) and its dup copy leave links on paths this tunnel's judge holds cut (stuck59/60; not a default: with both keys the lab download was 5.5% lower, see CHANGELOG stuck61) | both sides | off | off |
| `HS2_LB_CALL_LATE` — a call whose copy on one path runs >= 100 ms and twice behind its copy on another gets a fresh sub off that path and gives the late one up (acts only with `HS2_LB_RESCUE` on; since rel51 it also runs the stuck-call trigger: a call whose subs all sit on one path whose sockets stay in loss recovery / backlog for 500 ms, or that receives nothing for 600 ms while sending, gets a fresh sub off that path) | Iran (edge) | off | **on** |
| `HS2_REFILL_WAIT_MS` / `HS2_REFILL_WAIT_BYTES` — after a link loss the edge holds a new connection up to 300 ms for a link with the fewest bytes (branch start42) | Iran (edge) | off | **300 / 1** |
| `HS2_START_DIAL` — the first pool, and a pool back after an outage, dial through a start gate (32 at a time) | kharej (exit) | off | **on** |
| `HS2_MCONN_STRIPE_RESEND` — a striped connection's failed write is kept and resent when its link dies, with a 3 s last-sub grace (`HS2_MCONN_STRIPE_GRACE_MS`; rel49) | both sides | off | **on** |
| `HS2_MCONN_STRIPE_FAST` — a single upload's stripe widens fast (doubling chain, several pool links at once, 3 standing reserve links `HS2_MCONN_STRIPE_FAST_RESERVE`, 0 = no reserve; rel54). **On by default again since rel54c** by the operator's decision: one heavy upload gets +35–60 % single-connection upload; measured cost during that one heavy upload (whole lab, n=12): light p99 crowd +3 ms, echo +7.7 ms, ping +2 ms, game +1.6 ms. Turn off with `HS2_MCONN_STRIPE_FAST=0` on Iran | Iran (edge) | off | **on** |
| `HS2_MCONN_STRIPE_FAST_SOLO` — the fast rule only for an upload that stripes alone; while another upload stripes, uploads widen the rel53 way (rel54b) | Iran (edge) | off | **on** |
| `HS2_MCONN_STRIPE_FAST_EARLY_MS` — in a raw connection's first second the upload rate is sampled every 200 ms, so a fast upload upgrades to stripe in ~400 ms (rel54) | Iran (edge) | off | **200** |
| `HS2_MCONN_STRIPE_UP_PATHQ` — the upload stripe gate reads the light users' queue per path, not only the overall one (rel54) | Iran (edge) | off | **on** |
| `HS2_MCONN_STRIPE_UP_SPLIT` — the upload stripe gate grows on the median of 3 samples over the path's learned noise floor (its 30 s lower decile, capped at 10 ms) and cuts on the latest sample alone (10 ms floor, 67 % of the target), so the path's standing 3–7 ms queue no longer holds the stripe narrow while a queue spike still cuts at once (gate74: one upload +31 % in the production-like lab, light users' p99 unchanged). Turn off with `HS2_MCONN_STRIPE_UP_SPLIT=0` on Iran; tuning: `HS2_MCONN_STRIPE_UP_GROW_BASE_MAX`, `HS2_MCONN_STRIPE_UP_CUT_BASE_MAX` (ms), `HS2_MCONN_STRIPE_UP_CUT_PCT` | Iran (edge) | off | **on** |
| `HS2_LB_SILENT` — a blackholed path (its links have data waiting and get nothing acked while the other paths read) is read as 100 % loss and cut within ~7-12 s, held out while dark (only a byte acked ends the silence), and a path cooled for dials that got no SYN-ACK is probed every 2 s, so it is back 2-8 s after the blackhole clears (bh93-bh101); `HS2_LB_SILENT_REGROW` (lab key, off: no measured gain) lets the rebalance retire more idle links a round while such a path regrows | kharej (exit) | off | **on** |
| `HS2_MCONN_RESCUE` — a light connection (raw, and records-mode through `HS2_MCONN_RESCUE_REC`) whose path goes silent for 1.5 s is resent on a good path from a bounded per-connection ring (raw 128 KB / 64 MB total, records 32 KB / 48 MB total); blackhole lab: failed connections -74 %, 6.7 h soak memory flat, 0 failed rescues; costs: kharej +~80 MB in the bounded pools, crowd p99 +25 ms only inside blackhole windows (er91-er97, rel61). Both sides must run rel60+ (capability-negotiated: an older peer simply gets no rescue) | both sides | off | **on** |
| `HS2_MCONN_STRIPE_KEEPSTALL` — a striped heavy stream (download or upload) keeps each sub's chunks until the peer has read them, closes a sub whose link went dark (no ack progress, checked against its TCP state) and resends its chunks on a live sub; the receiver opens hole reorder room only after 1.5 s without progress. Blackhole lab: a heavy download/upload through a path that goes dark for 60 s survives (0 zero-throughput seconds) where it used to stall; quiet light p99 and throughput not worse than off (hs112-hs132, rel63). **Both sides need it** (rel63c+) for the full effect: the receiver opens its hole rooms only toward a peer that announces KEEPSTALL (capability ext bit10, hs133), so with one side on, a dark path behaves as before (r63d: an exit-only pair kept no dark protection, 3/6, and +120 MB exit RSS); a mixed pair is memory-safe either way. Roll out the exit (kharej) first, then Iran right after. `HS2_MCONN_STRIPE_KEEPSTALL_IDLE` (mb174, rel67b, on) frees what quiet connections keep, see the in-code defaults | both sides | off | **on** |
| `HS2_MCONN_RESCUE_JOINPICK` / `HS2_MCONN_RESCUE_SCALE` — a rescue join goes only to a link with fresh progress, on another Iran IP than the paths it leaves (a whole-IP outage darkens every path of that IP); the rescue ring budgets (records 48 MB, raw 64 MB) grow with the connections keeping a ring, capped at 4 % of MemTotal (`HS2_MCONN_RESCUE_RAMPCT`). Production-topology lab (6 Iran IPs × 2 kharej IPv4 + IPv6, whole-IP outages): JOINPICK cuts lost rescue joins, SCALE takes the exit's NAKs to ~0 for ~+100-270 MB on the exit (rs145, rs146). `HS2_MCONN_RESCUE_RETRY` (rel64): a rescue join lost before its answer (closed, or its link gone silent) is replaced once on another path, records lost joins ~106 -> ~19 a run with JOINPICK (rs146) | JOINPICK and RETRY Iran (edge), SCALE both sides | off | **on** |
| `HS2_LB_CUT_NONEW` — a path the dialing side reports cut gets no new connections (0 in the steering draw, and its links sort after every other link, before pressure, so a cut path's empty probe links no longer win new connections under a crowd's churn; with every path cut nothing changes). Flapping path lab (dark 25 s / clean 25 s): 0 vs 73-111 failed crowd connections in dark phases 2-5; single blackhole and quiet runs unchanged (fl107/fl108) | Iran (edge) | off | **on** |
| `HS2_RETIRE_KEEP_IDLE` — a pool shrink no longer resets live idle user connections (SSH, push, long-poll, IM keepalives, xray mux) on retiring links: they are reclaimed only after 60 m idle (`HS2_RETIRE_KEEP_IDLE_MIN`) instead of 5 m, and the 20 m trickle force-close is skipped; above max_links the surplus retiring links with the fewest connections still follow the old rules, cut-path drains unchanged. Production edge closed ~8.7k idle user conns a day this way; lab 0 vs 34 EOF on 900 idle conns (id125/rk127) | Iran (edge) | off | **on** |
| `HS2_MCONN_STRIPE_DL_FAIR` — the download fast-stripe slot is a 3 s lease taken only by a download's first growth (before, one long background download that once striped fast held it for its whole life), and a download at >= 80 Mbit/s may take one of 4 stripe slots past the 8-connection stripe cap (uploads and slow streams never). A single download among 3-10 background users: lab 427 -> 752 Mbit/s mean, p10 217 -> 393, striped at ~1.5 s in 10/10 runs; background users' throughput and light p99 unchanged (cs141/df142) | Iran (edge) | off | **on** |
| `hs2-menu` with no argument | installer | classic menu | **new menu** (classic: `c`, `hs2-menu --classic`, `HS2_MENU=classic`, `default_menu=classic` in `/etc/hs2/menu.conf`) |

These keys (eight since rel52, three more Iran-side upload keys in rel54) are part of the production default table, so (like every key
in it) they apply only on **reverse `l3mtcp`** tunnels and not under
`HS2_DEFAULTS=legacy`; every other carrier and direct mode keep all keys off,
exactly as in r31. Every other key of that table already had its r31 / r34
value. The service log names what is in force at start (`defaults: production
set on (…)`).

Still **off** by default (nothing changes unless you set them):
`HS2_LB_CALL_RESCUE`, (`HS2_LB_CALL_STUCK=0` turns the stuck-call trigger off; since rel51 it runs under `HS2_LB_CALL_RESCUE` or `HS2_LB_CALL_LATE`),
`HS2_LB_RESCUE_RAW`, `HS2_LB_EVAC`, `HS2_LB_FAST_VERDICT`,
`HS2_MCONN_STRIPE_REBUILD` (rel52, Iran: after the pool falls under half of its
recent peak, a striped upload uses one stripe sub beside its primary for
`HS2_MCONN_STRIPE_REBUILD_MS`, default 5000, while that pool rebuilds; the
window is per tunnel; a first lab u8 battery gained (n=6+6) but the
confirmation round did not replicate it on either lab, so it stays a lab
key — `docs/reality-architecture.md` §11.11.7), and the lab-only
trace keys `HS2_MCONN_TRACE`, `HS2_MCONN_TRACE_PORT`, `HS2_MCONN_TRACE_GAME`
(rel52: also traces game-like light two-way connections); the up54–up57 lab keys `HS2_MCONN_STRIPE_XDIR`, `HS2_MCONN_STRIPE_UP_NOSHARE` (Iran) and `HS2_STRIPE_UP_SUB_WIN_MB` (kharej) stay off; fl107's `HS2_LB_CUT_NONEW` is a default since fl108 (see the table).

Behaviour that changed without a key since r31: a whole-peer outage no longer
cools the non-primary LB paths (rel51); refused dials before the peer listens
do not start the start back-off (rel47); on Reality fronts only, the
EncryptedExtensions and the post-Finished records match the target (ee51,
§11.8.3 of `docs/reality-architecture.md`). Release notes for an r31
deployment: the rel52 entry of `CHANGELOG.md`.

Restore the old behaviour:
- **exactly r31 / r34:** turn the eight new keys off in the tunnel's unit (a
  drop-in, e.g. `systemctl edit hs2`):
  `Environment=HS2_LB_JUDGE_DATA=0 HS2_MCONN_STRIPE_RESEND=0` (both servers),
  `Environment=HS2_LB_RESCUE=0 HS2_LB_RESCUE_CALLFREE=0 HS2_LB_CALL_LATE=0 HS2_REFILL_WAIT_MS=0 HS2_REFILL_WAIT_BYTES=0`
  (Iran), `Environment=HS2_START_DIAL=0` (kharej), then
  `systemctl restart hs2`;
- **before rel58** (back to rel54c defaults): `Environment=HS2_MCONN_STRIPE_SHARE_COPIES=0 HS2_LB_RESCUE_CUT=0 HS2_QUEUE_CTL_UP=0 HS2_LB_EDGE_SYNC=0 HS2_RELAY_LAZY=0`
  (Iran), `Environment=HS2_QUEUE_CTL=0 HS2_RELAY_LAZY=0` (kharej); one at a
  time: `HS2_MCONN_STRIPE_SHARE_COPIES=0` (share63), `HS2_QUEUE_CTL_UP=0` /
  `HS2_QUEUE_CTL=0` (qctl63), `HS2_LB_EDGE_SYNC=0` (judge58),
  `HS2_RELAY_LAZY=0` (mem70, both sides); the mem70 frozen-session guard
  `HS2_SMUX_STALL_S` is off by default (`=15` turns it on);
  u8lazy's writeReady is not in rel58 (wr72 reverted it: download in
  scenario P −6.4 %, u8 call p99 +25 ms); `HS2_QUEUE_CTL_UP` was NOT a rel58 default (rel58v: echo p99
  +9 ms under 8 uploads); it is one since rel69a together with `_KEEP` (see **before rel69a**);
- **before stuck61** (light users off cut paths): `Environment=HS2_LB_RESCUE_CUT=0`
  (Iran);
- **before bh101** (blackholed paths read as silent): `Environment=HS2_LB_SILENT=0`
  (kharej);
- **before fl108** (a cut path gets no new connections): `Environment=HS2_LB_CUT_NONEW=0`
  (Iran);
- **before rel61** (silent-path rescue of light connections): `Environment=HS2_MCONN_RESCUE=0`
- **before rel63** (striped heavy streams survive a dark path): `Environment=HS2_MCONN_STRIPE_KEEPSTALL=0`
  (both sides);
  since hs133 the receiver's hole rooms open only toward a peer announcing keepstall (kindInfo ext bit10) and
  are capped (`HS2_MCONN_HOLE_ROOM_MB`, default 256 MiB per connection; `HS2_MCONN_HOLE_ROOM_GLOBAL_MB`, default
  512 MiB per process), so a rel63 side next to an older one stays memory-bounded in the dark;
- **before rel69m** (allocation/GC cuts, rescue ring pool, darkcall idle skip, exit stats scan outside the lock): `Environment=HS2_B296_DCSKIP=0`
  (both sides; the darkcall timer visits every conn on every 200 ms tick again, as in rel69l), `Environment=HS2_GC296_RINGPOOL=0`
  (both sides; rescue ring buffers are made and dropped per ring again, as in rel69l). gc295's allocation cuts
  (typed smux shaper heap, pooled smux request ctx, writer batch-list reuse, batch buffer pool, alloc-free TCP_INFO) and
  b296's exit stats copy, cs226 lite-gate order and cached link meter have **no key**: behaviour is identical by design;
  to undo them, roll back the binary (rel69l build, sha256 5674c6bb...);
- **before rel69l** (data-dark raw rescue, TUN ping stall fix, extra stripe slot for new fast uploads, call copy apart): `Environment=HS2_UL268_RESCUE=0` (Iran; the TCP_INFO watch,
  the dark-link rescue and the larger raw ring for fast uploads all go back to rel69k behaviour),
  `Environment=HS2_B278_L3STALL=0` (both sides; the TUN lane and the HS2_L3_DUP second copy pick links as in rel69k),
  `Environment=HS2_UL281_SLOT=0` (Iran; a new fast upload no longer takes an extra stripe slot past the cap),
  `Environment=HS2_CS286_CALLAPART=0` (both sides; a call's copy sub may again be placed on the path of its other sub, as in rel69k);
- **before rel69k** (cross-side call switch, darkcall switch coverage fixes): `Environment=HS2_XS261=0` (both sides;
  XS records are still parsed; the capability bit is no longer announced), `Environment=HS2_B262_COVER=0` (both sides);
  `HS2_B262_REPLACE` is off by default; `HS2_B262_WAIT_MS=0` gives back the unbounded darkwait (default 500 ms);
  `Environment=HS2_B270_TXRES=0` (both sides; stranded call-copy resend), `Environment=HS2_B274_REPAIR=0` (Iran;
  one-sub call repair), `Environment=HS2_JM271_RST=0` (kharej; missed-join RST);
  all together: `Environment=HS2_XS261=0 HS2_B262_COVER=0 HS2_B270_TXRES=0 HS2_B274_REPAIR=0 HS2_JM271_RST=0`;
- **before rel69j** (per-path download gate, stuck raw upload, dc254 send-frozen call switch, call copy hold):
  `Environment=HS2_DL259_PATHQ=0` (kharej), `Environment=HS2_UL257_STUCK=0` (Iran), `Environment=HS2_DC254=0`
  (both sides; the dc254 detector counters still run); b255's L7 cap: `Environment=HS2_HALF_CLOSE_MAX=4096`;
  all together: `Environment=HS2_DL259_PATHQ=0 HS2_UL257_STUCK=0 HS2_DC254=0 HS2_HALF_CLOSE_MAX=4096`;
- **before rel69h** (storm pacing of lite raw->dual upgrades): `Environment=HS2_ST244_UPG_RATE=0` (Iran);
  rel69i's grouped shaper filters and the restart adoption of a stale hs2 class have no switch;
- **before rel69f / rel69g** (young stripe sub's first hole re-sent on an older sub): `Environment=HS2_DL243_RESCUE=0`
  (kharej); the fx247 shaper self-heal, tc timeouts and the cs228 lock fix have no switch;
- **before rel69e** (queue-ctl shaper on the last mq child): `Environment=HS2_QUEUE_CTL_SHAPER=root` (both sides; the
  rel69d root-mode htb). Never run a pre-fx247 (before rel69g) root-mode hs2 on the same device as a mqchild one
  (its root replace re-grafts the mqchild htb as the root);
- **before rel69a** (edge upload queue controller, fast onset, kept caps, BBR refresh):
  `Environment=HS2_QUEUE_CTL_UP=0 HS2_QUEUE_CTL_FAST=0 HS2_QUEUE_CTL_KEEP=0 HS2_LINK_CC_REFRESH=0` (Iran),
  `Environment=HS2_QUEUE_CTL_FAST=0 HS2_QUEUE_CTL_KEEP=0 HS2_LINK_CC_REFRESH=0` (kharej); one at a time works too;
- **before rel68e** (call and lite dark-path fixes cs221..cs229, download rank mode):
  `Environment=HS2_CS221_DARKCALL=0 HS2_CS223_CALLRESEND=0 HS2_CS223_DARKRESEND=0 HS2_CS224_COPYIDLE=0 HS2_CS224_STALESWITCH=0 HS2_CS225_CALLPATH=0 HS2_CS226_LITEPATH=0 HS2_CS229_MINPLACE=0`
  (both sides; `HS2_CS226_LITEPATH=0` alone turns off LITEVISIT and MINPLACE's lite part),
  `Environment=HS2_FX212_DL_PRIM=1` (Iran, back to rel68d's refuse rule); one at a time works too;
- **before rel68d** (fx212 fixes): `Environment=HS2_FX212_DARKCALL=0 HS2_FX212_SUB_RESEND=0 HS2_FX212_AGE_GAP=0` (both sides),
  `Environment=HS2_FX212_DL_PRIM=0` (Iran); one at a time works too. The half-close fixes (hc213) and the abandoned-sub
  kept-chunk release (audit211 H2) have no switch;
- **before rel68c** (small-connection resend): `Environment=HS2_MCONN_SUB_RESEND=0` (both sides);
- **before rel68b** (dark-call switch, call resend, stripe steering, copy-sharing downloads and raw-upgrade uploads):
  `Environment=HS2_MCONN_DARKCALL=0 HS2_MCONN_CALL_RESEND=0 HS2_LB_STRIPE_STEER=0` (both sides),
  `Environment=HS2_MCONN_STRIPE_DL_COPIES=0 HS2_MCONN_STRIPE_UP_RAW_COPIES=0` (Iran); one at a time works too;
- **before rel64** (lost rescue join replaced once): `Environment=HS2_MCONN_RESCUE_RETRY=0` (Iran);
- **before rs146** (rescue join pick + scaled rescue rings): `Environment=HS2_MCONN_RESCUE_JOINPICK=0 HS2_MCONN_RESCUE_SCALE=0`
  (Iran), `Environment=HS2_MCONN_RESCUE_SCALE=0` (kharej);
- **before rel62k** (idle user connections kept on retiring links): `Environment=HS2_RETIRE_KEEP_IDLE=0`
  (both sides);
- **before rel62k3** (download fast-stripe slot as a lease, extra stripe slots for fast downloads): `Environment=HS2_MCONN_STRIPE_DL_FAIR=0`
  (Iran);
- **rel54's fast upload** (on by default since rel54c; it costs light users
  about +3 ms crowd / +7.7 ms echo p99 while one heavy upload runs): on Iran
  `Environment=HS2_MCONN_STRIPE_FAST=0` turns FAST, its reserve, SOLO and
  EARLY_MS off; or, keeping it on, one part at a time: `HS2_MCONN_STRIPE_FAST_RESERVE=0`,
  `HS2_MCONN_STRIPE_FAST_EARLY_MS=0`, `HS2_MCONN_STRIPE_UP_PATHQ=0`,
  `HS2_MCONN_STRIPE_UP_SPLIT=0` (gate74's split gate);
  `HS2_MCONN_STRIPE_FAST_SOLO=0` gives every parallel upload the fast rule
  again (rel54 before rel54b);
- **the whole default table off** (as before `defaults22`, older than r31):
  `Environment=HS2_DEFAULTS=legacy`;
- **the classic menu:** `default_menu=classic` (new menu → 9 → 4).

Mixed versions: rel54c and rel69j interoperate in both directions (2-tunnel lab
rehearsal of the reference tunnel's upgrade: new kharej + old Iran and old kharej + new
Iran, reverse `l3mtcp` with a Reality front, traffic at every step, config files
unchanged). rel40 and r31 interoperate in both directions (tested: new
kharej + old Iran and old kharej + new Iran, reverse `l3mtcp` with lb, 130 s
of traffic each, 0 loss), so either server may be upgraded first; upgrade the
other one soon after so both run the same defaults.

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
- **Best cover: a real nginx behind the tunnel port.** The installer does not
  set this up for you; it is a manual, optional step on the server that
  *listens* for links (Kharej in direct mode, Iran in reverse). Install nginx,
  make it listen on loopback only (so it does not take the public port), give
  it an ordinary site, and point hs2 at it:

  ```nginx
  # /etc/nginx/sites-enabled/cover — plain HTTP on loopback; hs2 terminates TLS
  server {
      listen 127.0.0.1:8081 default_server;
      server_tokens off;
      root /var/www/cover;          # any real static site
      index index.html;
      location / { try_files $uri $uri/ =404; }
  }
  ```

  then set `"backend_addr": "127.0.0.1:8081"` in `/etc/hs2/config.json` and
  restart hs2. Every probe that fails the tunnel auth is then answered by
  that nginx (its headers, error pages, keep-alive and timeouts are the real
  thing). Keep it on loopback: hs2 already holds the public port, and the
  backend must not be reachable from outside directly.
- Without a real backend, `HS2_COVER_NGINX=1` (environment of the hs2
  service) makes the built-in cover answer like a stock nginx — `Server:
  nginx`, nginx's header order, its HTML 404/400/405 pages and keep-alive —
  instead of like Go's net/http. The TLS layer in front is still Go's either
  way (for example its 7-day session-ticket lifetime), so a real nginx
  backend remains the stronger cover.
- **Answers to active probes, nginx-identical (since P31).** With
  `HS2_COVER_NGINX` (on by default) the built-in cover also copies how
  nginx 1.24 treats *malformed* and *stalled* requests, measured side by
  side against a real nginx with certbot's default TLS config:
  - the request line is checked as the bytes arrive; the first byte that
    cannot belong to one (random bytes, a lowercase method, …) gets nginx's
    `400 Bad Request` at once and the connection is closed — before, random
    bytes without a `\n` got no answer at all;
  - a request line over 8 KB → `414 Request-URI Too Large`; a header line
    over 8 KB or a request head over 32 KB → nginx's `400` *"Request Header
    Or Cookie Too Large"* (nginx's `large_client_header_buffers 4 8k`);
  - a connection that completes the handshake and sends nothing is closed
    after **60 s** with a TLS `close_notify` (was 30 s); a partial request
    (e.g. `GET / HTT` and then silence) is closed **silently** after 60 s,
    without a response — nginx's `client_header_timeout`;
  - `/robots.txt` and `/favicon.ico` are served (an ordinary site has them);
  - **HTTP/2 is deliberately not offered** (ALPN stays `http/1.1`): nginx
    with certbot's default config offers only `http/1.1`, and Go's h2
    `SETTINGS` frame would fingerprint differently from nginx's.

  All of this only affects connections that have **not** authenticated as a
  tunnel link — an hs2 link sends its auth record first and never reaches
  the cover — so tunnel traffic and speed are unchanged. Only the side that
  **listens** for links answers probes (the Iran edge in reverse mode,
  Kharej in direct mode), so only that side needs the new build; the other
  side may stay on its current version. With `backend_addr` set, your own
  backend answers instead, as before.

  | Probe (over TLS) | nginx 1.24 | hs2 before P31 | hs2 P31 |
  |---|---|---|---|
  | 300 random bytes, no `\n` | `400`, close | no answer (open) | `400`, close |
  | lowercase method `get / HTTP/1.1` | `400` | `405` | `400` |
  | 9 KB URI | `414` | `404`, keep-alive | `414` |
  | 10 KB header line / 40 KB of headers | `400` too large | `200` | `400` too large |
  | idle after the handshake | `close_notify` at 60 s | `close_notify` at 30 s | `close_notify` at 60 s |
  | partial `GET / HTT`, then stall | silent close at 60 s | `400` at 60 s | silent close at 60 s |
  | ALPN `h2,http/1.1` | `http/1.1` | `http/1.1` | `http/1.1` |
  | `GET /`, `/nonexistent`, `HEAD`, `OPTIONS`, `FOO`, plain HTTP | same as nginx | same | same |

  The remaining differences are intended: the cover's own page body and
  sizes, and `/robots.txt`/`/favicon.ico` returning `200` where a bare
  nginx with no such files returns `404`.

### Production defaults (on by default for reverse `l3mtcp`)

Since build `defaults22`, a plain install needs **no** `Environment=` lines:
on the `l3mtcp` carrier in **reverse** mode, every key the reference tunnel
runs is **on by default**, per side, exactly as that tunnel runs it. Every
other carrier (`mtcp`, `tls`, `udp`, `auto`, `dgtun`, `reality`, `noise`)
and direct mode keep the old defaults (all off). The service log says what is
in force at start: `defaults: production set on (edge side, N keys from
defaults; ...)`.

- **Turn one key off:** set it explicitly, e.g. `Environment=HS2_QUIET_IDLE=0`
  (an explicit value always wins: `0`, `off` or an empty `HS2_X=` mean what
  they always meant — off).
- **Change one value:** set it, e.g. `Environment=HS2_MCONN_STRIPE=4`.
- **Old behaviour, all keys off:** `Environment=HS2_DEFAULTS=legacy` (or `0`);
  then only the keys you set explicitly apply, as before `defaults22`.

| key | default | side |
|-----|---------|------|
| `HS2_SHAPE_FAST`, `HS2_SHAPE_SMALL`, `HS2_SHAPE_BULK` | `1` | both |
| `HS2_L3_LANE`, `HS2_HEAVY_BACKLOG`, `HS2_L3_DUP`, `HS2_FAST_LINK` | `1` | both |
| `HS2_MCONN` | `lite` | both |
| `HS2_MCONN_BATCH`, `HS2_MCONN_DEADLINE_FIX`, `HS2_MCONN_CLOSE_HS` | `1` | both |
| `HS2_MCONN_CALL`, `HS2_MCONN_CALL_AVOID` | `1` | both |
| `HS2_MCONN_STRIPE` | `8` | both |
| `HS2_MCONN_STRIPE_UP`, `HS2_MCONN_STRIPE_SHARE`, `HS2_MCONN_STRIPE_REL` | `1` | both |
| `HS2_MCONN_STRIPE_RESEND` (resend + 3 s rejoin when a striped connection's link dies; `HS2_MCONN_STRIPE_GRACE_MS`, default 3000, 0 = no rejoin) | `1` | both |
| `HS2_MCONN_STRIPE_QWIN_MS` | `8000` | both |
| `HS2_QUIET_IDLE`, `HS2_TLS_RESUME`, `HS2_AUTH_PAD`, `HS2_COVER_NGINX` | `1` | both |
| `HS2_LB_AUTO2`, `HS2_LB_CUT_DRAIN`, `HS2_LB_FAST_RECOVER` | `1` | both |
| `HS2_LB_START_GRACE_S` | `20` | both |
| `HS2_LB_JUDGE_DATA` | `1` | both |
| `HS2_SMUX_AUTOWIN`, `HS2_SMUX_AUTOWIN_REL` | `1` | both |
| `HS2_SMUX_AUTOWIN_SHARE_MB` | `32` | both |
| `HS2_MCONN_RESCUE` (rel61; silent-path rescue of light connections, records-mode included unless `HS2_MCONN_RESCUE_REC=0`; `=0` turns it off) | `1` | both |
| `HS2_MCONN_STRIPE_KEEPSTALL` (rel63; a striped heavy stream keeps unacked chunks and resends them off a dark sub; `=0` turns it off) | `1` | both |
| `HS2_MCONN_RESCUE_SCALE` (rs146; rescue ring budgets grow with the ring connections, capped at `HS2_MCONN_RESCUE_RAMPCT`=4 % of MemTotal; `=0` = fixed 48/64 MB) | `1` | both |
| `HS2_AP_SHRINK_HYST` (sp176; the autopilot shrinks only a link's worth under the grow line, no boundary flap; `=0` turns it off) | `1` | both |
| `HS2_MCONN_DARKCALL` (cs193/cs194, rel68b; a 200 ms timer finds a call's dark sub — no ack progress, repeated RTO and nothing received for `HS2_MCONN_DARKCALL_MS`, in-code `1000`, clamped 300-5000 — and moves the call's primary to a live sub on another socket and path with a ring resend; Iran starts a records rescue when there is none; each side acts on its own sending direction, no wire change; `HS2_MCONN_DARKCALL_TRACE` stays off; `=0` turns it off) | `1` | both |
| `HS2_LB_STRIPE_STEER` (st149/st150, rel68b; stripes keep off paths the steering reports cut — Iran: stripe picks and the upload sender, kharej: the judge steers the download sender; never stalls a stripe that has only cut-path subs; in production by drop-in since 2026-11-14; `=0` turns it off) | `1` | both |
| `HS2_MCONN_SUB_RESEND` (r68fix2..r68fix4, rel68c; a small light connection — at most `HS2_MCONN_SUB_RESEND_SCOPE_KB`, in-code 256, sent — keeps its subs' unacked chunks and resends them on a fresh live sub when one dies instead of a lost-copy reset; keep age max(300 ms, 3 x srtt), per-sub healing gaps, 32 MiB global cap; `=0` turns it off) | `1` | both |
| `HS2_FX212_DARKCALL` (fx212, rel68d; the DARKCALL in-flight cap applies only on a stalled link — bytes in flight, no ack progress for half the dark time — a call with 2+ live subs never has them all capped, and the cap fits the resend ring; `=0` gives back the rel68c cap) | `1` | both |
| `HS2_FX212_SUB_RESEND` (fx212, rel68d; SUB/CALL_RESEND light-path cost: a given-up sub of a small connection resends only when it holds undelivered bytes, one shared 1 s keep sweeper instead of per-connection polls, keptMax noted per tick, the resend skips chunks the target sub already holds; `=0` gives back rel68c) | `1` | both |
| `HS2_FX212_AGE_GAP` (fx212 / audit211 M2, rel68d; a sub-resend age trim on the write path, without TCP proof, opens the sub's gap so a death in that window takes the fast reset instead of a resend with a hole and a 30 s stall; `=0` turns it off) | `1` | both |
| `HS2_CS221_DARKCALL` (cs221, rel68e; a darkcall switch resends only what the target sub lacks; a call pair never puts both copies of a chunk on one path while another path has room; the darkcall rx rescue waits while a live sub reads; `=0` gives back rel68d) | `1` | both |
| `HS2_CS223_CALLRESEND` (cs223, rel68e; a given-up call sub's chunks are resent at give-up on a live sub instead of at the sub's death, and every resend skips chunks the target already delivered; `=0` gives back cs221) | `1` | both |
| `HS2_CS223_DARKRESEND` (cs223, rel68e; a call sub that goes dark while not the primary resends at once the chunks the other subs lack (needs CALLRESEND); `=0` turns it off) | `1` | both |
| `HS2_CS224_COPYIDLE` (cs224, rel68e; a copy-only sub that moves bytes is not idle for the drain reclaim; `=0` gives back the old rule) | `1` | both |
| `HS2_CS224_STALESWITCH` (cs224, rel68e; no ring resend off a sub an earlier darkcall switch already left; `=0` gives back the old rule) | `1` | both |
| `HS2_DC254` (dc254, rel69j; darkcall also switches a call primary whose sends sit frozen in RTO backoff (no ack progress for darkcallAfter, backoff >= 2 or >= 2 retransmits) while its link still receives (call visits only; a cs226 lite visit keeps rel69i's DARK-only switch/rescue), and a copying call keeps both copies while it was call-like within callFreeTTL under mCallMaxRate; the silent-rx / tx-stall / late-tick detector (`dc254` part of the darkcall log line, its own 10 lines/min budget) runs with the key on or off; `HS2_DEFAULTS=legacy` turns it off unless `=1`; `=0` gives back rel69i) | `1` | both |
| `HS2_XS261` (xs261, rel69k; cross-side call switch: a side that moves a call off a dark/txDark sub, or whose call receive is silent >= 800 ms while another sub's link receives, sends an XS record (kind 0x15 + seq) on the sub the peer should make its primary; the receiver switches through darkcall (resend of what the peer may lack), at most one act per 500 ms, stale seq ignored, never echoed; sent only to a peer announcing ext bit 14 (older peers, incl. r54c, never see the record); also b258's cs224 fix (a sub a darkcall switch chooses loses its "left" mark); `=0` gives back rel69j) | `1` | both |
| `HS2_B262_COVER` (b262, rel69k; a darkcall switch trusts "the target holds it" only for bytes the target provably wrote or still queues, a ring that misses provably unconsumed bytes is short (not covered), a chunk's only copy never goes to a dark sub, and a call pair (3+ subs) never leaves a chunk on dark subs only (the writer waits, "darkwait"); `=0` gives back b261) | `1` | both |
| `HS2_B262_WAIT_MS` (b262, rel69k; upper bound in ms of one darkwait episode; at the bound the chunk goes out as b261 sent it (counted "give"); `0` = no bound; default 500 since b266: unbounded, Kharej held sends up to 5 s in lab1) | `500` | both |
| `HS2_B278_L3STALL` (b278, rel69l; the TUN lane moves an interactive flow off a link whose TCP has data in flight and no ack progress — RTO backoff, or bytes_acked still for max(400 ms, 3 srtt) — and the dup's second copy skips such a link; lab3 data-dark ping gaps; `=0` gives back the old pick) | `1` | both |
| `HS2_B270_TXRES` (b270; a 2-sub call's copies that went only on a non-primary sub whose link is half dead — dc254 txDark (our sends frozen in RTO), or the link received nothing for darkcallAfter while the conn's receive advances over the other sub (the peer->us direction dead) — are resent on the live sub at the edge and every 300 ms while it lasts, repeats only past what was covered; data-dark lab3 ab270: 7-10 s call stalls on rel69j/b266 gone; `=0` gives back rel69k-pre) | `1` | both |
| `HS2_B274_REPAIR` (b274, rel69k; a call conn left with one live sub (its second died) re-opens the second at the next manager tick when it was call-like within 5 s, instead of waiting for the lite sampler's two steady call-like windows (lab1 R1: ~48 s on one sub; a data cut on that sole sub had no darkcall candidate, 12.3 s stall); call budget and the 2 s reopen backoff still apply; `=0` gives back the sampler only) | `1` | Iran (edge) |
| `HS2_B270_RECENT_MS` (b274, rel69k; a b270 resend takes only the half-dead sub's chunks sent within this many ms (plus its queue): lab1 one-direction cuts, the rx edge misfired on subs whose own direction worked and pushed 450-1260 old chunks in front of the live sub's call data; `0`/unset = default 600000 (in effect unbounded = b270); lab1 TH1 with 3000 resent 0 chunks in 15 edges, so turn it down only after a lab3 data-dark check) | `600000` | both |
| `HS2_JM271_RST` (b271, rel69k; the exit answers a join for a conn it does not know (missed join, e.g. after an exit restart) with an RST record (base record 0x05, understood by every mconn peer since part A, incl. rel69i/rel69j/r54c) so Iran ends the conn at once instead of re-joining it forever; 500/s, burst 2000, over the bucket the sub is closed bare as before; lab3 rs271: 1000 long-lived conns after an exit restart 70-700 hung -> 0, all ended by RST within p50 2 s; `HS2_DEFAULTS=legacy` turns it off unless `=1`; `=0` gives back rel69j) | `1` | kharej (exit) |
| `HS2_B262_REPLACE` (b262, rel69k; Iran closes (CLOSE-REQ) a call sub a darkcall switch left that has had no TCP ack progress for >= 2 s with >= 16 KB in its socket while the primary is live; its link goes on the dead-key avoid list and the call opens a fresh sub; one per conn per 10 s, 20/s per process; lab1 ABAB not proven, so off; `=1` turns it on) | `0` | Iran (edge) |
| `HS2_CS225_CALLPATH` (cs225, rel68e; a call's copy/pair sub goes to another healthy path than its other subs first; replaced by `HS2_CS229_MINPLACE` while that is on; `=0` gives back cs224) | `1` | both |
| `HS2_CS226_LITEPATH` (cs226, rel68e; master switch of the lite dark-path rules (LITEVISIT, MINPLACE's lite part, LITEPLACE); `=0` gives back cs225) | `1` | both |
| `HS2_CS228_LITEVISIT` (cs228, rel68e; darkcall visits a non-call lite connection only while a live sub has bytes the peer has not consumed and its link is silent while pending or lowered/cut with writes > reads, and moves it off; `=0` turns it off) | `1` | both |
| `HS2_CS228_LITEPLACE` (cs228; a lite second sub only on another path whose RTT/loss matches the primary's; off in rel68e (MINPLACE replaces it); `=1` turns it on when MINPLACE is `0`) | `0` | both |
| `HS2_CS229_MINPLACE` (cs229, rel68e; a lite second sub and a call's extra sub keep the original pick and are re-picked on another healthy path only when that pick shares a path with the connection's live subs; replaces LITEPLACE and CALLPATH while on; `=0` turns it off) | `1` | both |
| `HS2_LINK_PROBATION`, `HS2_PARTIAL_REFILL` | `1` | Iran (edge) |
| `HS2_MCONN_STRIPE_SHARE_COPIES` (an upload's stripe may share links that carry lite's copies; uploads only, the game's lane links last — `HS2_MCONN_STRIPE_SHARE_COPIES_LANEQ=0` turns that order off) | `1` | Iran (edge) |
| `HS2_PICK_LANE` | `on` | Iran (edge) |
| `HS2_LM_FASTPICK` (fp185; new user connections and mconn subs are placed under the pool's read lock with a CAS-checked placement, so a connection storm no longer queues on the pool's write lock; `=0` turns it off) | `1` | Iran (edge) |
| `HS2_MCONN_RESCUE_JOINPICK` (rs146; a rescue join only on a fresh link of another Iran IP; `=0` = the old pick) | `1` | Iran (edge) |
| `HS2_MCONN_RESCUE_RETRY` (rel64; a rescue join lost before its answer is replaced once on another path; `=0` = no retry) | `1` | Iran (edge) |
| `HS2_STRIPE_AW_SOLO` | `1` | Iran (edge) |
| `HS2_STRIPE_AW_HEAVY` | `2` | Iran (edge) |
| `HS2_LB_RESCUE`, `HS2_LB_RESCUE_CALLFREE` | `1` | Iran (edge) |
| `HS2_LB_CALL_LATE` | `1` | Iran (edge) |
| `HS2_LB_RESCUE_CUT` (stuck61) | `1` | Iran (edge) |
| `HS2_LB_EDGE_SYNC` (judge58; the edge keeps a cut the dialing side's fresh report still holds — no ping-pong) | `1` | Iran (edge) |
| `HS2_LB_SLOW_SKIP_COPY` (lb89; the force close of trickling streams on retiring / cut-drained links skips mconn copy subs, whose rate is never measured) | `1` | Iran (edge) |
| `HS2_LB_CUT_NONEW` (fl108; a path the dialing side reports cut gets no new connections: 0 in the steering draw, its links sorted after every other link; `=0` turns it off) | `1` | Iran (edge) |
| `HS2_RETIRE_KEEP_IDLE` (rel62k; a pool shrink keeps live idle user connections on retiring links, reclaimed after 60 m idle; `=0` turns it off) | `1` | Iran (edge) |
| `HS2_MCONN_STRIPE_DL_FAST` (dr116/rel62; a single download stripes from its first 200 ms samples — 80 Mbit/s twice in a row — and widens by doubling, solo; `=0` turns it off) | `1` | Iran (edge) |
| `HS2_MCONN_STRIPE_DL_FAIR` (rel62k3/cs141; the download fast-stripe slot is a 3 s lease for a download's first growth, fast downloads may take 4 stripe slots past the cap; `=0` turns it off) | `1` | Iran (edge) |
| `HS2_STRIPE_HEAVY_FLOOR_MB` (dl79; a download's sub windows never under 4 MiB once it proves heavy; `=0` turns the floor off) | `4` | Iran (edge) |
| `HS2_STRIPE_HEAVY_FLOOR_GATE` (dl79; the floor drops while the stream's own loop shows this many queued probes in a row; `0` = floor without the gate) | `2` | Iran (edge) |
| `HS2_REFILL_WAIT_MS` | `300` | Iran (edge) |
| `HS2_REFILL_WAIT_BYTES` | `1` | Iran (edge) |
| `HS2_MCONN_STRIPE_FAST` (rel54, default again since rel54c; reserve `HS2_MCONN_STRIPE_FAST_RESERVE`, default 3; `0` turns it off) | `1` | Iran (edge) |
| `HS2_MCONN_STRIPE_FAST_SOLO` (rel54b; acts only while `HS2_MCONN_STRIPE_FAST` is on) | `1` | Iran (edge) |
| `HS2_MCONN_STRIPE_FAST_EARLY_MS` (rel54; acts only while `HS2_MCONN_STRIPE_FAST` is on) | `200` | Iran (edge) |
| `HS2_MCONN_STRIPE_UP_PATHQ` (rel54) | `1` | Iran (edge) |
| `HS2_MCONN_STRIPE_UP_SPLIT` (gate74) | `1` | Iran (edge) |
| `HS2_MCONN_STRIPE_DL_COPIES` (dl186/dl188, rel68b; a download's stripe picks may share links that carry lite's copies, as uploads do under SHARE_COPIES, at most `HS2_MCONN_STRIPE_DL_COPIES_MAX` (in-code `4`, `0` = no bound) such subs; `=0` turns it off) | `1` | Iran (edge) |
| `HS2_FX212_DL_PRIM` (fx212 rel68d, rank mode dlp217 rel68e; `2`: a download grades the links carrying lite copy-only subs — no open primary first, then idle primaries, then at most `HS2_FX212_DL_PRIM_HOTK` (in-code `1`) sub on a link whose primaries moved bytes in the last `HS2_FX212_DL_PRIM_QUIET_MS` (in-code `1000`); `1` gives back rel68d (refuse any link with a primary), `0` rel68c) | `2` | Iran (edge) |
| `HS2_MCONN_STRIPE_UP_RAW_COPIES` (ul197, rel68b; an upload's raw upgrade takes the share-copies hint too, so its first stripe sub may share a link with lite's copies; `HS2_MCONN_STRIPE_UPGATE_HYST` stays off; `=0` turns it off) | `1` | Iran (edge) |
| `HS2_START_DIAL` | `1` | kharej (exit) |
| `HS2_LB_FILL_PCT` | `25` | kharej (exit) |
| `HS2_LB_SPARE_PER_ROUND` | `3` | kharej (exit) |
| `HS2_LB_SPARE_EDGE` (sp176; a spare is asked only where the edge would serve it, with a per-path backoff when it did not stay; `=0` turns it off) | `1` | kharej (exit) |
| `HS2_LB_REC_CONFIRM` | `1` | kharej (exit) |
| `HS2_LB_FLAP_PEN` | `1` | kharej (exit) |
| `HS2_LB_SILENT` (bh101; a blackholed path — data waiting, nothing acked — reads as 100 % loss, is cut in ~7-12 s and probed back 2-8 s after it clears) | `1` | kharej (exit) |
| `HS2_QUEUE_CTL` (qctl63; the path-queue controller for what kharej sends) | `1` | kharej (exit) |
| `HS2_QUEUE_CTL_UP` (qctl63; the path-queue controller for what Iran sends; a default since rel69a with _KEEP; `=0` turns it off) | `1` | Iran (edge) |
| `HS2_QUEUE_CTL_FAST` (lat58; a controller caps a big queue after `qFastTicks` instead of `qStartTicks`; rel69a; `=0` gives back the slow onset) | `1` | both |
| `HS2_QUEUE_CTL_KEEP` (hl204..hl232, rel69a; an idle cap is kept at the path's measured rate, a new burst is capped at its onset, the cap probes past the measured rate, Iran's upload stripe gate takes the edge cap's target; `=0` gives back rel68e's controller; sub-switches `HS2_QUEUE_CTL_SEEN=0`, `HS2_QUEUE_CTL_STRIPE=0`, `HS2_QUEUE_CTL_STRIPE_SPIKE=0`) | `1` | both |
| `HS2_QUEUE_CTL_SHAPER` (pk239, rel69e; where the queue-ctl shaper (htb + cake) lives: `mqchild` = on the last child of a multi-queue NIC's mq root, links steered there only under a cap (clsact skbedit queue_mapping), the mq root kept; a 1-queue NIC, a non-mq root or a kernel < 5.18 falls back to root mode by itself; `root` gives back rel69d (htb as the root qdisc); `ifb` is lab-only, it breaks TSQ) | `mqchild` | both |
| `HS2_DL243_RESCUE` (dl243/dl244, rel69f; a young stripe sub whose own TCP link has acked nothing for max(300 ms, 2 x srtt) gets its first hole (<= 256 KiB) re-sent on an older sub, 1 per sub, 2 per conn per 10 s; `=0` turns it off) | `1` | kharej (exit) |
| `HS2_DL259_PATHQ` (dl259, rel69j; the download stripe's 3 ms queue gate is judged per path: a path whose other busy links queue over the target is cut for that download (no chunk, no width slot) until under half the target after 2 s, and the width is judged on the uncut stripe paths only, so a download whose primary loads its own path can still widen onto unloaded paths; `=0` gives back rel69i) | `1` | kharej (exit) |
| `HS2_ST244_UPG_RATE` (st244..st246, rel69h; the edge paces lite raw->dual upgrades through one token bucket so a connection storm's second-sub upgrades spread over a few seconds; `auto` = max(500, 100 x GOMAXPROCS) a second, burst rate/10; call upgrades and interactive flows (`HS2_ST246_INTERACTIVE_BPS`, in-code 1000 B/s, `0` = off) are not paced; a number sets the rate, `=0` turns it off) | `auto` | Iran (edge) |
| `HS2_LINK_CC_REFRESH` (us200/cc203, rel69a; a light link whose BBR left STARTUP with a collapsed estimate is re-inited by a reno->bbr toggle; `=0` turns it off) | `1` | both |

The table is `hs2-src/envdef/envdef.go` row for row (111 keys). Not in it on
purpose: `HS2_MCONN_STRIPE_JUMP_MS` (no proven gain). `HS2_QUEUE_CTL_UP` was
opt-in until rel69a (rel58v measured echo p99 +10 ms without _KEEP); rel69a
turns it on together with `_FAST` and `_KEEP` (CHANGELOG rel69a). Opt-in keys, off unless set
(see CHANGELOG): `HS2_LB_UPLOSS` (Iran; judge of upload-only loss). Off unless set
since rel68: `HS2_MCONN_RESCUE_RST` (rr166), `HS2_SMUX_SENDBATCH` (lk179),
`HS2_MCONN_STRIPE_DL_SHARE` / `_HEADROOM` (dl18x); since rel68b:
`HS2_MCONN_STRIPE_UPGATE_HYST` (ul197), `HS2_MCONN_DARKCALL_TRACE` (cs194). The lab
key `HS2_MCONN_RESCUE_AGAIN` is not in this build.

**In-code defaults (every carrier and mode, not in the table).** These keys
are read straight from the environment, so `HS2_DEFAULTS=legacy` does not
touch them; each acts only while the feature it refines is on (on reverse
`l3mtcp`, through the table above). `=0` (or the value shown) restores the
older behaviour.

| key | default | side | what |
|-----|---------|------|------|
| `HS2_MCONN_RESCUE_REC` | on (`=0` off) | both | records-mode rescue rides `HS2_MCONN_RESCUE` |
| `HS2_MCONN_RESCUE_IDEM` (rs169, rel67b) | on (`=0` off) | both | idempotent rescue under `HS2_MCONN_RESCUE_RETRY`: a RESCUE repeated by the edge's retry is answered again (the answer kept 60 s) instead of refused with a reset; a late answer on a replaced join counts; no raw retry when no records ring covers the bytes; a peer that read past this side's raw count needs nothing resent. No wire change |
| `HS2_MCONN_STRIPE_KEEPSTALL_IDLE` (mb174, rel67b) | on (`=0` off) | both | a striped connection that keeps chunks (KEEPSTALL) is polled every 1 s so what the peer acked is freed; one quiet for 30 s (10 s while the process keeps over half of the 320 MiB global cap) drops its unacked tail. Before, quiet keep-alive connections pinned their last in-flight chunks and filled the global cap, and an active connection then lost its dark-path hole chunk (reset after 30 s) |
| `HS2_MCONN_RESCUE_KB` / `HS2_MCONN_RESCUE_MB` | `128` / `64` | both | raw rescue ring per connection / total |
| `HS2_MCONN_RESCUE_MS` | `1500` | both | silence before a rescue starts |
| `HS2_MCONN_RESCUE_REC_KB` / `HS2_MCONN_RESCUE_REC_MB` | `32` / `48` | both | records ring base size / total for the base rings |
| `HS2_MCONN_RESCUE_REC_SECS` / `HS2_MCONN_RESCUE_REC_MAXKB` | `3` / `1024` | both | cd152: a records ring holds 3 s of its connection's write rate, at most 1 MB (`MAXKB=32` = the old fixed ring) |
| `HS2_MCONN_RESCUE_REC_BIGMB` | unset: **sized from RAM** (rel67b; before: `64`) | both | budget for ring growth past 32 KB (a 2 Mbit/s call's ring is ~0.75 MB). Unset, rel67b takes max(min(256 MB, 10 % of RAM), 2 % of RAM), at most 1 GB — RAM is MemTotal or the service's cgroup memory limit when smaller (23 GB → ~470 MB, 4 GB → 256 MB, 1 GB → ~102 MB); the startup log line `mconn rescue-rec: big ring budget N MB (source …)` shows the value, and the `mconn rescue-rec:` counters line shows `big X KB of N MB`. Setting the key (MB) overrides it. mb174: a ring whose rate wants at most half its size for 3 s shrinks back; growth is tiered — calls up to 100 % of the budget, sustained writers 60 %, bursts 40 %. Production (~4.2k connections on rel66b/rel67) sat at the old 64 MB cap. A cap, taken only while used |
| `HS2_MCONN_RESCUE_RAMPCT` | `4` | both | with `HS2_MCONN_RESCUE_SCALE`, the ring budgets' cap in % of MemTotal |
| `HS2_HALF_CLOSE` (hc183, rel68) | on (`=0` off) | both | TCP half-close through the tunnel: a user's or target's FIN ends one direction only, the other keeps flowing (curl --data, nc -N, SMTP/RPC clients). Only toward a peer announcing ext bit13; an older peer gets the old full close. A half-closed relay waits at most 2 min (edge) / 3 min (exit) without progress |
| `HS2_HALF_CLOSE_MAX` (hc183b, rel68; 16384 since rel69j, b255 L7; was 4096) | `16384` (`0` = no cap) | both | at most this many relays wait half-closed per process; over it a relay ends at once with a reset (hc213 abandon mark; before rel69j a FIN) |
| `HS2_LB_REC_RAMP`, `HS2_LB_LOWER_EVID` (wj178, rel68) | on (`=0` off) | both | a recovered path climbs back to 1.0 on a ramp; a weight is lowered only on evidence (two low readings or real loss) |
| `HS2_LB_CALL_STUCK_GAP_STRICT` (wj178, rel68) | on (`=0` off) | Iran (edge) | a receive gap alone (talk-spurt pause) no longer counts a call as stuck |
| `HS2_MCONN_CALL_RESEND` (r68fix, rel68b) | on (`=0` off) | both | a lite call's chunks are kept until TCP acked them and a dead sub's unacked ones are sent again on a live sub (with `HS2_MCONN_STRIPE_RESEND`); on Iran a sub given up by the stuck-call trigger whose close handshake never completed counts as unclean. Before, a call whose subs went dark was reset 10-30 s after the stuck-call give-up ("missing bytes never arrived"; rel67b too). The receiver drops what it already has by offset; no wire change |
| `HS2_MCONN_DARKCALL_MS` (cs194, rel68b) | `1000` (clamped 300-5000) | both | the dark-evidence window of `HS2_MCONN_DARKCALL` |
| `HS2_MCONN_STRIPE_DL_COPIES_MAX` (dl188) | `4` (`0` = no bound) | Iran (edge) | the cap of `HS2_MCONN_STRIPE_DL_COPIES` |
| `HS2_UL268_RESCUE` (ul268, rel69l) | on (`=0` off) | Iran (edge) | data-dark path rescue: a link whose data is frozen (TCP_INFO: unacked and bytes_acked unchanged for >= max(1 s, 8 x srtt), or backoff >= 2) on 2 consecutive 250 ms watch ticks, while a link on another path still acks, joins the rescue set; only the Iran conns with a non-bulk sub on that link are visited (link index), a raw one is rescued only when its ring covers the link's SIOCOUTQ + 64 KB, calls are left to darkcall (dc254), the two exclude each other per conn for 1.5 s; a fast raw writer's ring grows to half a second of its rate (<= 8 MB per conn, <= min(256 MB, RAM/16) per process); logs "ul268: dark ..." and "ul268 scale: ..." |
| `HS2_UL268_HOLD_MS` (ul268, rel69l) | `2000` | Iran (edge) | after a dark link's visit started a rescue, that link is not visited again for this long (it stays in the join avoid set) |
| `HS2_UL281_SLOT` (ul281, rel69l) | on (`=0` off) | Iran (edge) | a NEW fast upload (raw sample >= 80 Mbit/s, raw conn younger than 20 s, first growth only) whose growth finds all 8 stripe slots held may take one of the 4 extra stripe slots that `HS2_MCONN_STRIPE_DL_FAIR` gives fast downloads (shared, bounded at 12 per process); before, every upload was refused and stayed on its raw first link (~200 Mbit/s at 80 ms once autowin cut the window to the 2 MiB base). lab2 ul281d, 10 interleaved off/on pairs: upload 229 -> 327 Mbit/s (+98, CI [+72, +123], 10/10), light p99 unchanged, 0 fails; logs "ul281: new upload took stripe slot ..."; no wire change |
| `HS2_CS286_CALLAPART` (cs286, rel69l) | on (`=0` off) | both | a call's copy sub never rides the path of the call's other sub while another path exists: re-picked on a healthy other path, else a lowered/cooled one (a lowered path beats both copies on one path), else a degraded one; also when the call placement falls back to the plain pick; logs "cs286 kept/moved/nocand" in the "mconn call" 10 s line |
| `HS2_B296_DCSKIP` (b296, rel69m) | on (`=0` off) | both | the darkcall timer (every 200 ms) skips a non-call conn proven idle: its last visit found nothing to do and every live sub fully consumed by the peer (written == consumed, none dark or capped, no outcome check due, no dc254 episode), and no write, new sub or call roll happened since (the conn's write sequence is unchanged); call conns are never skipped and every 25th tick (5 s) visits all conns. Cuts the timer's m.mu and ack-mark reads on idle conns (5-6 % CPU and late ticks at 2x load). lab3 sc4x: 7 interleaved 1x pairs neutral (all CIs straddle 0), 1.5x equal or better (crowd p99, stall seconds, 0 darkcall switches vs 3); the darkcall stats line ends "b296 skip N"; no wire change |
| `HS2_GC296_RINGPOOL` (gc296, rel69m) | on (`=0` off) | both | rescue ring buffers (rescue/raw rings and rrBig rings) are taken from a sync.Pool per power-of-two size class (4 KB-8 MB) and returned when the ring is dropped, instead of a fresh make each time; the ring bytes still count against the existing budgets (rescue mem cap, `HS2_UL268_MEM_MB`, rrBig RAM-scaled) and a pooled buffer survives at most 2 GC cycles, so the pool is bounded by those budgets, not by user count. lab2 storm pairs: alloc -26..-50 %, kharej GC cycles -32 %, Iran RSS +137 MB, latency within noise; no wire change |
| `HS2_UL257_STUCK` (ul257, rel69j) | on (`=0` off) | Iran (edge) | a raw upload whose writer sat blocked in its first sub's Write for >= 60 % of a full 1 s sample, under 30 Mbit/s, after >= 1 MB, opens its second stripe sub at once instead of waiting for 4 good samples (a sample that moved nothing counts as an upload); app-limited flows never block, so light users never trigger it; logs "ul257 stuck raw upload" |

**Rescue: what it covers and its limits.** `HS2_MCONN_RESCUE` saves a
*light* connection (web, API, IM, calls) whose path goes silent for 1.5 s
(`HS2_MCONN_RESCUE_MS`): the edge opens a rescue join on a good path and
both sides resend, from a bounded per-connection ring, what the other did not
get. Heavy striped streams are covered by `HS2_MCONN_STRIPE_KEEPSTALL`
instead. Two kinds: **raw** (a lite connection still in raw mode, ring
128 KB) and **records** (`HS2_MCONN_RESCUE_REC`, ring 32 KB up to 3 s of the
connection's rate, at most 1 MB). Limits, by design:
- **once per connection**: a connection is rescued at most once; a second
  outage on the same long-lived connection is not rescued (it ends as
  before);
- **one retry**: under `HS2_MCONN_RESCUE_RETRY` (edge) a rescue join lost
  before its answer is replaced **once** on another path; with
  `HS2_MCONN_RESCUE_IDEM` (rel67b) the exit answers the repeated RESCUE
  again instead of resetting, and the edge skips a raw retry no records ring
  could serve;
- **the ring must cover the gap**: when more was written since the cut than
  the ring holds (a fast writer, a long silence, or a budget at its cap), the
  rescue is answered NAK / `nocover` and the connection is reset — the user
  sees a reset, never wrong or missing bytes;
- **rare duplicate request**: when the exit had already finished and
  forgotten a connection whose answer the edge never saw, the rescue join
  opens it afresh (role 4) and the request bytes reach the target a second
  time (a repeated POST is possible: at-least-once in this corner case). It
  needs a path to die exactly between the target's answer and its arrival.

What to watch: the service log prints, whenever they change,
`mconn rescue:` (started / acked / served, `nocover`, `failed`, `lostjoin`,
`refused`, `nak`), `mconn rescue-rec:` (the same for records rings, plus
`big` / `call` KB, `bigno`, `shrunk`), `mconn rescue145:` (`retry`,
`retryfail`, `retrysilent`, ring caps) and `mconn rescue169:` (`repeat`,
`lateans`, `noring`, `overread`, `fresh`). Rising `nocover` / `failed` /
`refused` mean resets users saw; `big` near the cap with `bigno` growing
means the budget is too small (raise `HS2_MCONN_RESCUE_REC_BIGMB`).

**A cut connection ends with a reset, not a clean close (rel66b, rel67b).**
When the tunnel loses part of a connection's data (every sub died, a rescue
that could not cover, a relay error), the program at the other end now gets a
TCP reset (RST) instead of a FIN, so a truncated upload or download is never
taken as complete: the exit resets the target (fx168, rel66b), the edge
resets the user (fx170, rel67b), and a link's EOF in smux reads as `io.ErrUnexpectedEOF`, not as
the stream's end. The live status file (`/run/hs2/<config>.status.json`) counts
them since start (rel67b): `mconn_target_rst`
(exit targets of a multi-link connection), `mconn_user_rst` (edge users of
one) and `stream_rst` (either end of the one-stream path). A steady trickle
follows path outages; a jump without one is worth a look. Known gap: a
*target's* own reset on the one-stream path and on a raw lite connection
still reaches the user as a FIN (smux has no reset frame; needs a protocol
change).

`HS2_LB_CALL_LATE` (since 8.12): a call whose copy on one path runs >= 100 ms
and twice behind its copy on another (socket srtt, 1 s) gets a fresh sub off
that path and no more copies on the late one, which is then given up: call
p99 after a path degrades -29% (c) and -69% (k), never fired on equal/steady
paths; `=0` turns it off.

The queue controller (since qctl63): each side measures the queue its own
links build on the path (TCP_INFO, each tick) and, only while a heavy flow
keeps that queue big, caps this server's egress on that path a little under
the rate it gets (a tc shaper that exists only while the cap does), so light
users are not stuck behind the heavy one's queue. Lab (8 uploads, n=13 per
arm): call p99 -34%, game p99 -11%, crowd/call/game p50 -5/-20/-14%, heavy
-2%; no change in 1 upload, 1 or 8 downloads (where it does not cap) or idle
(`docs/reality-architecture.md` 11.16). It is local to the server (no wire
change), so it works against any peer version. Off: `HS2_QUEUE_CTL_UP=0`
(Iran) and/or `HS2_QUEUE_CTL=0` (kharej). Since rel69a `HS2_QUEUE_CTL_UP`,
`HS2_QUEUE_CTL_FAST` and `HS2_QUEUE_CTL_KEEP` are defaults too (CHANGELOG rel69a). Since rel69e
the shaper sits on the last mq child (`HS2_QUEUE_CTL_SHAPER=mqchild`, CHANGELOG rel69e-pre).

The start window (since 8.13, branch start42): after a start the kharej dials
its first pool through a start gate (`HS2_START_DIAL`, 32 at a time, 10-35 ms
apart, the first 4 s only, again after an outage at most once a minute), so
the pool is up ~2.5 s after the first link instead of ~10 s; on Iran a new
connection leaves the start hold after 300 ms for the link that moved the
fewest bytes under 2x the fair share (`HS2_REFILL_WAIT_MS=300`,
`HS2_REFILL_WAIT_BYTES=1`), and the hold stays on at least 5 s. Lab: crowd p99
of the first 5 s 1419 -> 157 ms, whole-run ping / call / heavy equal, busiest
link 72 -> 64. Off: `HS2_START_DIAL=0` (kharej), `HS2_REFILL_WAIT_MS=0` (Iran).

Not on by default: `HS2_MCONN_STRIPE_JUMP_MS` (no measurable gain), `HS2_LB_EVAC`
(light-user evacuation off cut paths: no measurable gain in the lab, see
`docs/reality-architecture.md` 8.5), `HS2_LB_FAST_VERDICT` (floor a path at
its first clearly bad reading: no measurable gain, the plain rule already does
it, 8.6), `HS2_LB_CALL_RESCUE` (a call whose subs are all on a lowered path gets a fresh sub off it and gives the lowered ones up: k call p99 529 -> 211 ms, but c not better, 204 vs 355, the path's verdict comes ~7 s after the cut, 8.10; with its stuck-call trigger `HS2_LB_CALL_STUCK`, on under it, a call all on one path whose sockets are in loss recovery or backlog for 500 ms moves at once: it ended the one stuck c run in ~2 s and fired 0 times on equal/steady paths, but c by median is still not better, 212 vs 196 ms, n=9, 8.11), `HS2_LB_RESCUE_RAW` (the rescue also for raw single-sub connections when the duplication budget binds, which the default budget did not in the lab; call p99 not yet proven unchanged, 8.8, 8.9) and every
key not in this table. The side is the config's `mode`: `dial` = Iran (edge),
`listen` = kharej (exit). Every default-on key works against an older peer
(new behaviour is announced by a capability bit or carried in a field older
builds already parse), so either server may be upgraded first.

### Optional DPI hardening keys (environment)

On reverse `l3mtcp`, `HS2_QUIET_IDLE`, `HS2_TLS_RESUME`, `HS2_AUTH_PAD` and
`HS2_COVER_NGINX` are **on by default** on both servers (see *Production
defaults* above); turn one off with `=0`. On other carriers they are off
unless set in the service's environment (e.g. a systemd drop-in with
`Environment=HS2_QUIET_IDLE=1`). With all of them off the tunnel's bytes on
the wire are exactly as before.

- `HS2_QUIET_IDLE=1` — idle links (no users) are nearly silent like a
  browser's idle connection: TCP keepalive 45 s, one control ping/pong every
  20–40 s, the rest only if nothing else went. Links with users keep today's
  cadence and dead-link detection. Goes quiet only towards a peer that runs it
  too.
- `HS2_TLS_RESUME=1` — links resume TLS sessions with single-use tickets (as
  Chrome does) instead of a full handshake each time. The tunnel auth stays
  bound to each connection, so a ticket or a recorded auth is useless on
  another connection.
- `HS2_AUTH_PAD=1` — the first exchange inside TLS is sized like an HTTP
  request and a response with a body. Wire compatible with older peers.
- `HS2_UTLS_HELLO=auto|133|131|120` — which Chrome ClientHello to send
  (default: the link carrier's Chrome 133, the newest in the bundled uTLS;
  the reality carrier's Chrome 120). Unset it to roll back.
- `HS2_COVER_NGINX=1` — see above.

### Throughput and load-balancing keys (environment)

These keys came out of a field-tuning round on a busy multi-IP tunnel
(reverse `l3mtcp`, ~12k users, ~80 ms RTT, 0.5–1.5 % path loss). Each was
A/B-tested in a production-like lab (≥ 6 interleaved runs with a crowd of
light users) and then measured in production; a key was kept only if light
users (web, calls, games, ping) got no slower. On reverse `l3mtcp` they are
**on by default** with the values and sides below (except
`HS2_MCONN_STRIPE_JUMP_MS`); turn one off with `=0`, or all of them with
`HS2_DEFAULTS=legacy`. On other carriers they stay off unless set.

**Window (smux) — single-connection speed.** The receive window of a stream
used to be a fixed 2 MiB, which caps one connection at ~200 Mbit/s at 80 ms.

- `HS2_SMUX_AUTOWIN=1`, `HS2_SMUX_AUTOWIN_REL=1`, `HS2_SMUX_AUTOWIN_SHARE_MB=32`
  — on **both** servers. The window of a heavy stream grows on the side that
  *receives* the data (kharej for uploads, Iran for downloads) while probes
  show no queue, and shrinks at once when a queue or a light user on the same
  link appears. All grown windows share a 32 MiB budget. Production: upload
  on one connection 155 → ~1300 Mbit/s, upload on 8 connections 1.2 → 2–3
  Gbit/s, light users unchanged.
- `HS2_STRIPE_AW_SOLO=1` — **Iran** only. A connection that striping has not
  spread over several links keeps the adaptive window (it used to be pinned at
  2 MiB). Production: download on one connection ~190 → ~550 Mbit/s.
- `HS2_STRIPE_AW_HEAVY=2` — **Iran** only. Every sub-stream of a stripeable
  connection (the primary and the stripes) keeps the adaptive window from the
  start, with the normal light-neighbour rule. Lab: download on one connection
  +43 % (16 runs, no light-user cost); production at peak hour: 126–174 →
  744–1090 Mbit/s. (`=1` adds a strict neighbour cap — rejected, no gain.)
- `HS2_MCONN_STRIPE_JUMP_MS=1500` — kharej. Harmless; no measurable gain in
  production. Kept for parity with the lab.
- `HS2_STRIPE_HEAVY_FLOOR_MB=4`, `HS2_STRIPE_HEAVY_FLOOR_GATE=2` — **Iran**
  only (dl79). Once a download proves heavy its sub windows never go under
  4 MiB, while its own probes show no queue; 2 queued probes in a row drop
  the floor (0.5–8 s hold). Lab: single download +53 %, d8 +51 %, crowd
  p99 within noise.

**How the adaptive window (autowin) behaves.** smux's window is the most a
sender may have unread at the receiver; at 80 ms a fixed 2 MiB caps one
connection at ~200 Mbit/s. With `HS2_SMUX_AUTOWIN` the *receiving* side
sizes each heavy stream's window on its own:
- **grows** while the stream's probes show no standing queue (the path is
  not yet full), in steps, up to what the budget allows;
- **shrinks at once** (`HS2_SMUX_AUTOWIN_REL`) when probes show a queue or a
  light user starts receiving on the same link, so a download never builds
  a queue that games, calls or web pages wait behind;
- **shares one budget**: all grown windows of the process together stay
  under `HS2_SMUX_AUTOWIN_SHARE_MB` (32 MiB); a stream past the base window
  takes from it and gives it back when it shrinks or ends. This bounds the
  memory heavy users can pin (the receiver must be able to buffer a whole
  window per stream); 16 trades single-download speed for less queueing,
  64 is the experimental `max-throughput` preset;
- the Iran-side keys (`HS2_STRIPE_AW_SOLO`, `HS2_STRIPE_AW_HEAVY`, the heavy
  floor) only choose *which* streams use it and how low it may fall.

Every window change goes to the sender as a window update (UPD). Since rel66
(aw163) the updates are numbered and sent in order, and the sender ignores an
update older than the last one it took: before, a shrink's update could
overtake a newer one, the sender's limit went backwards and a heavy stream
could stall for good with healthy links (no watchdog saw it). Each dropped
stale update is counted in the live status file
(`/run/hs2/<config>.status.json`, read by `hs2 status`) as `obs_smux_stale_upd`; a non-zero
value is a stream that would have hung before the fix, not an error.
rel66b (fx168) also lets a reader blocked on that update lock give up at its
deadline or close. Either half (receiver ordering, sender guard) alone
prevents the stall, so one rel66+ side already protects both directions;
the counter grows only on a rel66+ sender.

**Load balancing (lb auto2, several IPs).**

- `HS2_LB_FAST_RECOVER=1` — both servers. A path whose raw loss stays under
  the bar for 3 readings in a row has its smoothed loss reset, and its first
  recovery hold is 10 s instead of 30 s. Lab: links on a recovered IP 22 → 38.
- `HS2_LB_START_GRACE_S=20` — both servers. No whole-IP cut in the first 20 s
  after start (a restart's own loss is nobody's fault). A restart no longer
  cuts paths.
- `HS2_LB_FILL_PCT=25` and `HS2_LB_SPARE_PER_ROUND=3` — kharej (the side that
  dials). Retiring links on a path that lost its share no longer block new
  links on the healthy paths (slots may go up to target + 25 %), and a
  rebalance round may ask for up to 3 spares instead of 1. Lab: healthy paths
  reach 90 % of their target in ~150 s (before: never within the run);
  production: under-filled paths 14/20 → 19–20/20 in 10 min.
- `HS2_LB_REC_CONFIRM=1` — kharej. While a path is cut, one bad reading from a
  tiny sample (a cut path keeps only ~2 links) no longer restarts its recovery
  clock: that takes 2 bad readings in a row, or one with ≥ 5000 segments.
  A path that is really still bad stays cut. Production: a cut path that took
  45 min to come back now returns in 30–80 s once its loss is gone.
  (`HS2_LB_REC_DEBUG=1` logs every reading of a cut path.)
- `HS2_LB_REC_RAMP` — both sides, **on by default** (`0` = old rule). A cut
  path that recovers climbs back to full weight in steps all the way (+0.15 a
  reading, +0.30 when the reading is clean; a path cut 2+ times within
  `HS2_LB_FLAP_WIN_S` climbs ≥0.07 a reading, ~65 s to full). Before, the
  climb ended with a jump from ~0.55 to 1.00 the moment the path's verdict
  turned "ok".
- `HS2_LB_LOWER_EVID` — both sides, **on by default** (`0` = old rule). A
  healthy path is not lowered on noise: the judge counts loss above 0.3 % and
  queue above 20 ms only, and a path at "ok" is lowered on its second reading
  in a row under the bar (at once when the reading's loss is ≥ 2 %).
- `HS2_LB_CALL_STUCK_GAP_STRICT` — Iran, **on by default** since wj178 (`0` =
  old rule). A call's receive gap triggers the stuck-call move only with
  evidence on its path (a socket in loss recovery / backlog, or a lowered
  link): a pause between talk spurts no longer opens a fresh sub and gives up
  the call's subs.
- `HS2_LB_FLAP_PEN=1` — kharej (the side whose judge decides; on by default
  since build `rel29`). A *flapping* path — one local→remote IP pair that
  reads clean while cut and breaks again as soon as its users return — is
  kept out for a long time instead of being let back every few minutes. A
  pair with ≥ 4 **relative** cuts (lossy and clearly worse than the best
  path) within 60 min is penalized: it stays cut for 30 min, doubling on each
  repeat up to 2 h. Whole-IP verdicts, forced cuts and "every path is lossy"
  never count. Safeguards: at most ~1/3 of the paths are penalized at once
  (none with fewer than 3 paths), never leaving fewer than 2 healthy paths or
  under 60 % of the paths healthy; if the other paths degrade (half of them
  cut or lossy, the best one over the cut bar, all lossy) every penalty lifts
  at once. A penalized path keeps its probe links, so its loss is still read:
  clean for a third of its penalty, it returns early on probation (one more
  relative cut within 30 min → the next, doubled, penalty at once). Tuning
  keys (rarely needed): `HS2_LB_FLAP_N` (4), `HS2_LB_FLAP_WIN_S` (3600),
  `HS2_LB_FLAP_HOLD_S` (1800), `HS2_LB_FLAP_MAX_S` (7200),
  `HS2_LB_FLAP_PROB_S` (1800), `HS2_LB_FLAP_EARLY_DIV` (3). Lab (6
  interleaved A/B runs with a flapping path and a light-user crowd): cut
  cycles and seconds of users on a lossy path fell in 6/6 runs, crowd p99
  −12.7 ms, no light-user cost. In production on the reference tunnel's
  kharej since 2026-11-10.

The set running on the reference tunnel (on reverse `l3mtcp` the defaults
now apply it with no `Environment=` line; listed for other carriers and
older builds):

```
# Iran (edge)
HS2_SMUX_AUTOWIN=1 HS2_SMUX_AUTOWIN_REL=1 HS2_SMUX_AUTOWIN_SHARE_MB=32
HS2_STRIPE_AW_SOLO=1 HS2_STRIPE_AW_HEAVY=2
HS2_LB_FAST_RECOVER=1 HS2_LB_START_GRACE_S=20
# kharej (exit)
HS2_SMUX_AUTOWIN=1 HS2_SMUX_AUTOWIN_REL=1 HS2_SMUX_AUTOWIN_SHARE_MB=32
HS2_MCONN_STRIPE_JUMP_MS=1500
HS2_LB_FAST_RECOVER=1 HS2_LB_START_GRACE_S=20
HS2_LB_FILL_PCT=25 HS2_LB_SPARE_PER_ROUND=3 HS2_LB_REC_CONFIRM=1
HS2_LB_FLAP_PEN=1
```

Measured on that tunnel at peak hour (iperf3, 8 s each; raw = the same two
servers without the tunnel):

| test | tunnel | raw path |
|------|--------|----------|
| download, 8 connections | 1587 Mbit/s | 1973 Mbit/s |
| upload, 8 connections | 2727 Mbit/s | 7093 Mbit/s |
| upload, 1 connection | 948 Mbit/s | 1560 Mbit/s |

The raw path itself carries only ~2 Gbit/s down, so the tunnel's download is
at ~80 % of what the path allows.

**Tried and rejected** (the keys stay in the binary, off): a larger fixed
window for every stream or for stripes only (`HS2_STRIPE_SUB_WIN_MB`), finer
window updates (`HS2_STRIPE_SUB_DIV`), letting the window grow on probes that
saw loss, a looser stripe gate, extra spare links (`SPARE_FIX/SPARE_MAX`),
and a gradual ramp back for a recovered path. Each either gained nothing or
made light users slower (crowd p95 +2 to +15 ms, calls or ping p99 +7 to
+45 ms).

## Managing

```bash
hs2-menu           # 3 = tunnel manager, 4 = status/logs, 5 = upgrade, 8 = uninstall, 9 = server doctor
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
- **`hs2 doctor --deep [-c config] [--lang fa]`** — the server doctor: in
  about 10 s it inspects the whole server and explains each finding in plain
  language (OK / WARN / BAD, what, why it matters, what to do; `--lang fa`
  for Persian): NIC queues and the core the NIC's rx softirq lands on (and
  whether hs2/xray still run there), CPU pressure (PSI) and the top
  processes, each path's plain ping next to the tunnel's own RTT/loss (a
  provider problem vs. one on the servers), network-wide congestion, a
  Reality inbound whose dest is a shared CDN (others can use your IP as a
  clean IP), x-ui user churn and node-sync errors, and hs2's own health
  (links, LB cuts, restarts, panics). Read-only; a missing tool is skipped
  with a note. In the menu: **9) Server doctor**, and **Diagnose** per tunnel.
- **`hs2 ports -c /etc/hs2/config.json`** — the user ports and where each one
  goes, as this server knows it (see *per-port targets* above); with `add`,
  `remove`, `default` or `udp` it changes this server's half (also in the
  tunnel manager as **Ports**).
- **`hs2 version`** prints a build stamp (`… [build <rev> <date>]`); compare it
  on both servers to confirm they run the same build (the `hs2-menu` banner
  shows it too).

### Safe config changes: history, preview, trial apply, rollback

Every config change can go through a preview + trial + automatic-rollback
path (the tunnel manager's **Edit config** uses it when the binary supports
it; `hs2 apply --supported` prints `apply-v1`):

```
hs2 config history  -u hs2-de1                 # stored versions (last 50)
hs2 config show     -u hs2-de1 [--version N]
hs2 config diff     -u hs2-de1 --new new.json  # or --version N; marks [bilateral] fields
hs2 config validate -u hs2-de1 --new new.json  # hs2 check + host cross-checks
hs2 apply    -u hs2-de1 --new new.json [--confirm-within 5m] [--health-gate]
hs2 apply    --confirm -u hs2-de1              # keep it
hs2 apply    --status  -u hs2-de1
hs2 rollback -u hs2-de1 [--to N]
hs2 config lock | unlock | lock-status         # one admin at a time
```

- **History:** `/var/lib/hs2/history/<unit>/NNNN-<time>-<reason>.json` (+ a
  `.dropins.json` snapshot of `/etc/systemd/system/<unit>.service.d`).
- **Diff** is semantic (JSON keys, `Environment=` keys of drop-ins); secrets
  are never printed. Fields that must match the other server (`shared_key`,
  `carrier`, `encap`, `proto`, `sni`, `mtu`, `reverse`, `forward_ports`, the
  `addr` port, `lb`, noise keys) are marked **bilateral**, and `hs2 apply`
  says what the other server must change.
- **Validate** adds: port conflicts with the other tunnels in `/etc/hs2` and
  with services already listening, IPs that must exist on this server
  (`bind_local_ip`, `user_listen_ip`, a listening `addr`, own `lb` list),
  readable `cert_file`/`key_file`, `lb` weights summing to 100.
- **Apply:** snapshot → atomic write → restart in a transient systemd service
  (an SSH drop changes nothing) → health gate (unit active, no `panic:` in the
  log, steady for 8 s; with `--health-gate` also ≥ 1 serving link, all within
  60 s) → trial. Not confirmed in time, or unhealthy during the trial →
  automatic rollback to the snapshot + restart. A transient timer
  (`hs2-apply-expire-*`) is a second, independent guard. State:
  `/var/lib/hs2/apply/<unit>.json`.
- **Lock:** `/run/hs2/admin.lock` (owner, pid, tty, time); a lock whose
  process is gone or that is older than 2 h is stale and taken over;
  `--force` breaks a live one with a warning.
- **Audit log:** `/var/log/hs2-changes.log` (who, tty, unit, action, reason,
  changed keys, result).

### New menu (the default since M7) and live dashboard

A bare `hs2-menu` (or `bash install.sh`) opens the redesigned menu; it became
the default after the M7 lab test (fresh install, both tunnel directions,
join, every section in fa/en, simple/advanced, 80 columns, no colour,
non-root, two admins, rollback, upgrade from the old release, uninstall —
checklist in `docs/menu-m7-checklist.md`). The classic menu stays reachable:

```
hs2-menu --classic    # the classic menu    (or HS2_MENU=classic hs2-menu)
hs2-menu --new        # force the new menu  (or HS2_MENU=new hs2-menu)
```

**Live load-balancing screen** (rel69m installer): new menu **3) Load balancing & IPs → l) Live view**, the classic
LB screen (9), or straight from a shell with `hs2-menu lb [tunnel] [--once] [--view V]` or the one-word wrapper
`hs2-menu-lb` (written to `/usr/local/bin/hs2-menu-lb` by install/upgrade, removed by uninstall). One row per Iran IP
sorted by traffic, with share, links live/target, down/up Mbit/s, streams, loss (the worse of down and up; `N↑` marks
upload loss), RTT, path MTU (lowest direction, red under 1280) and the automatic cuts of the last 24 h; cut or idle IPs
on one dim line, then the Kharej IPs and the LB verdicts. Keys: `p` per-path table, `m` matrix, `k` Kharej IPs, `h` help,
Enter back. It reads the same data as `hs2 lb show --status` and changes nothing.

`c` in the new menu opens the classic one and `n` there comes back;
**9) Menu settings → hs2-menu opens** switches the default
(`default_menu=classic|new`). Order of precedence: the flag, then `HS2_MENU`,
then `menu.conf`, then the new menu. On a fresh server **2) Tunnels** first
offers to install the binary, then runs the wizard. Every change made from a
screen ends with **"Keep the change? y = confirm"** right there (it waits for
the health check if needed); Enter leaves the automatic rollback running
(`hs2 apply --confirm -u U` from a shell still works). Colour off (setting or
`NO_COLOR`) is plain text everywhere, including separators and notes.

Header: `server · N tunnels · users · Mbit · health · active warnings`. Items:
1) Live dashboard 2) Tunnels 3) Load balancing & IPs 4) Security &
camouflage 5) Speed & performance 6) Monitoring & alerts 7) Server doctor
(`hs2 doctor --deep`) 8) Maintenance (upgrade, backup, restore, **change
history** = `hs2 config history`, **roll back the last change** =
`hs2 rollback`, status, uninstall) 9) Menu settings. Item 2 is the Tunnels
screen, 3 Load balancing & IPs and 5 Speed & performance have their own
screens (sections below), and 4 Security & camouflage too (M5). Screens that change a config hold the
admin lock (`hs2 config lock`) while they are open; another admin's lock is
shown with its owner and can be broken only after a question.

**Menu settings** (`/etc/hs2/menu.conf`, `key=value`): `lang=fa|en` (Persian
needs a UTF-8 locale; otherwise English is used), `mode=simple|advanced`
(advanced shows a description under the items and the build stamp; later
milestones hide expert options in simple mode), `color=on|off`,
`default_menu=classic|new`, and the optional history sampler below.

**Live dashboard** (`hs2 dashboard`, read-only, never takes the lock):

```
hs2 dashboard [--lang fa] [--width N] [--color off] [--ascii]   # live, 3 s refresh
hs2 dashboard --once                                           # one frame
hs2 dashboard --header                                         # the header line only
hs2 dashboard --paths                                          # with vs without tunnel ping per LB IP
```

One block per tunnel (from `/etc/hs2/*.json`, `/run/hs2/*.status.json`,
`systemctl`): role, mode, carrier, dialing/listening side, state (running /
starting / stale / stopped / failed / unknown), users, Mbit (down/up per
direction when LB reports it), links serving/target, link RTT p50/p99, LB
paths ok/lowered/cut, restarts, active alerts (`/var/lib/hs2/alert-*.json`) and
a 24 h text sparkline of RTT and Mbit. Keys: `q`/Enter back, `l` logs,
`d` server doctor, `p` path check (pings each LB IP without the tunnel, ~3 s;
only on demand). Not a tty: one frame without escape codes. Below 60 columns
one value per line; without a UTF-8 locale ASCII and English.

**24 h history:** `hs2 stats sample` folds each running tunnel's status into
hourly aggregates (RTT p50 average, worst p99, Mbit average/peak, peak users)
kept for 7 days in `/var/lib/hs2/stats-<unit>.json` (a few KB). It is taken
by the alert check (`hs2 alert`, every 60 s when alerts are on) and,
optionally, by `hs2-stats.timer` (every 5 min, `Nice=19`; **9) Menu settings
→ 24 h history sampler**). `hs2 stats show -u <unit>` prints the hours.
Without either, the dashboard says "no data yet".

### Tunnels: wizard, connection code, edit, renew, clone (new menu → 2)

Every action of the Tunnels screen is an `hs2 tunnel` command (the screen only
renders its output; a binary without `hs2 tunnel` falls back to the classic
screens):

```
hs2 tunnel list [--json|--names] [--lang fa]     # state, role, direction, carrier, address, ports
hs2 tunnel create [--advanced] [--dry-run]       # interactive wizard
hs2 tunnel create --from-json spec.json --yes    # non-interactive (same checks)
hs2 tunnel join '<code>' [--user-ports P] [--expose A] [--cert self|PEM --key PEM] [--yes]
hs2 tunnel join --update '<code>'                # update an existing tunnel from a newer code (safe apply)
hs2 tunnel code  -u U [--show]                   # connection code for the other server
hs2 tunnel clone -u U --to-code                  # code that recreates THIS side on another server
hs2 tunnel edit  -u U [--section network|transport|security|performance|lb]
hs2 tunnel edit  -u U --set key=value [--set …] [--confirm-within 5m] [--dry-run]
hs2 tunnel renew -u U [--pick-ip] [--upgrade-from BIN] [--dry-run]   # backup, binary, rebuild, best IP, safe apply
hs2 tunnel start|stop|restart -u U [--yes]
hs2 tunnel logs  -u U [-n 80] [-f]
hs2 tunnel delete -u U                           # two-step confirm, backup first
hs2 tunnel preflight --from-json spec.json
```

**Wizard steps** (`q` cancels at any step; nothing is written before the
last one): 1) role of this server (Kharej / Iran) 2) direction (direct;
reverse = Kharej dials in — helps when outgoing connections from Iran are
filtered or Kharej is behind NAT) 3) carrier (mtcp, tun+mtcp; advanced mode
also dgtun over udp / icmp) 4) front (TLS; Reality is shown as "coming" and
not selectable yet) 5) addresses and ports (this server's public IP or the
peer's IP, tunnel port, user ports on Iran, default target + per-port targets
on Kharej, domain, certificate: a PEM path, the Let's Encrypt live directory,
or `self` = a self-signed certificate made at create) 6) "enable 24 h charts?
(cheap)". Then a summary, the new config as a diff, and **pre-flight
checks**: ports free (also against the other tunnels on this host), local
IPs exist, peer ping and a TCP connect to the peer's tunnel port when this
side dials, the domain's DNS points at this server (TLS listener),
certificate present, kernel tuning state. A `[FAIL]` stops without changes;
`[warn]` lines only inform (e.g. no domain: works with a self-signed cover
certificate; better a domain + ACME HTTP-01 / DNS-01, or the Reality front
later). Create writes the config (mode 600) and
`/etc/systemd/system/<name>.service`, starts it and waits 6 s: a unit that
does not stay up is removed again, so a failed or aborted create leaves
nothing behind. The first history version is saved
(`hs2 config history -u <name>`).

`--from-json` takes the same answers: `name`, `role`, `direction`,
`carrier` (`mtcp` | `l3mtcp`/`tun+mtcp` | `dgtun` + `encap` `udp`|`icmp`),
`front` (`tls`), `public_ip`, `peer`, `bind_ip`, `tunnel_port`, `domain`,
`cert`/`key`, `user_ports`, `expose`, `targets`, `subnet`, `iface`, `mtu`,
`udp`, `shared_key`, `stats`, `lb`. Name, `/30` subnet, interface and shared
key default to free / fresh values, so several tunnels on one host never
collide (`hs2`, then `hs2-t2`, …; `hs0`, `hs1`, …).

**Connection code** — `hs2://v2.<base64url JSON>`: version, tunnel name,
creator's role, direction, carrier/encap, front, the listener's `host:port`
(IPv6 as `[addr]:port`), SNI, shared key, MTU, `/30` subnet, UDP flag, the
creator's user ports or default target (informational), LB lists, and
`"secrets": ["shared_key"]` (treat the code like a password; `code --show`
prints it decoded with secrets masked). The wizard also prints a short check
line (`name role→role direction endpoint digest`) to compare both servers
over the phone — it is not enough to join. `hs2 tunnel join` takes the other
role with the same direction, so in reverse mode the Iran side is the
listener and holds the certificate. A code from a newer hs2 is refused with
"upgrade hs2 on this server"; a classic installer link (`hs2://…`, v1) is
still read (tcp/tun carriers).

**Edit** shows a section (secrets masked, bilateral fields marked), and
`--set` goes through the semantic diff + `hs2 apply --confirm-within`
(health gate, automatic rollback unless `hs2 apply --confirm -u U`). A change
to a bilateral field prints what the other server must change and a ready
`hs2 tunnel join --update '<code>'` for it. Restart, stop, edit, renew and
delete warn when your SSH session arrives on one of the tunnel's user ports or
is routed through its interface.

**Renew / migrate** backs up the current config to the history, shows the
binary version (`--upgrade-from /path/to/new/hs2` installs a new binary first:
it must answer `version`, the old one is kept as `/usr/local/bin/hs2.prev`
and put back if the tunnel does not stay up after its restart), fills keys newer releases expect (`max_links` auto, a missing
`cover_seed`), with `--pick-ip` pings the peer from each local public IP and
picks the lowest RTT as `bind_local_ip` (the current one is kept within
1 ms), then applies safely. **Clone** prints a code with `"clone": true`:
joining it on another server recreates this side (same role, ports, targets);
point the peer at the new server afterwards.

**24 h charts:** the sampler timer stays **off by default** (the alert check
already samples when alerts are on); the wizard's last step offers it and
`hs2 stats timer on` writes and enables `hs2-stats.service` +
`hs2-stats.timer` itself (`off` disables and removes them, `status` prints
on/off), so it no longer depends on the installer having written them; the
menu setting uses the same command.

### Load balancing & IPs (new menu → 3)

Every action is an `hs2 lb` command (a binary without it falls back to the
classic LB screen). Changes go through the semantic diff + `hs2 apply
--confirm-within` (health gate, automatic rollback) under the admin lock:

```
hs2 lb show    -u U [--lang fa]          # config summary + live path table + cuts in the last 24 h
hs2 lb history -u U [--hours 24]         # cut / recovering / flapping events per path (journal)
hs2 lb suggest -u U [--peer IP]          # ping the peer from every local public IP, rank, suggest the primary
hs2 lb add-ip  -u U --ip X [--weight W] [--sni S] [--cert F --key F] [--front tls]
hs2 lb remove-ip -u U --ip X
hs2 lb weights -u U ip=w,ip=w,… | --equal   # every IP of this side, sum 100
hs2 lb mode    -u U fixed|auto
hs2 lb park    -u U --ip X --hours N     # weight 0 until then, restored by itself
hs2 lb unpark  -u U --ip X
hs2 lb parks                             # parked IPs of all tunnels
```

Common flags (before or after the positional value): `--confirm-within 5m`,
`--yes`, `--dry-run`, `--lang fa`.

- **Path table** (the live LB page as columns): path, configured share /
  effective share (auto), links live/target, users, Mbit down/up, loss
  down/up, RTT p50/p99, auto factor + reason, state (cooling, degraded,
  draining, primary, …) and the number of cuts of that path in the last
  24 h (from `journalctl -u <unit>`: the `lb auto2: path … cut` lines).
- **Suggest**: 5 pings from each local public IP to each peer IP (the peer's
  primary + its lb list, or `--peer`), ranked by loss (1 % steps) then RTT;
  the current primary stays first within 1 ms / 1 %. It only suggests.
- **Add IP**: the IP must exist on an interface of this server; on the first
  add the primary joins the list explicitly (a wildcard listener: its first
  public IP) and the weights are split equally (or `--weight W`, the others
  scaled to the rest). Per-IP `--sni` / `--cert` / `--key` (TLS) are the lb
  stage-3 fields; `--front` accepts `tls`; switch the IP to reality / both afterwards with
  `hs2 reality front` (screen 4, Security & camouflage, M5). This works on a
  wildcard listener (`addr 0.0.0.0:P`, the `hs2 tunnel create` default) too:
  it announces its explicit lb list. `hs2 reality front … tls` keeps the
  reality key, so a later switch back needs no new key.
- **Reality target by listener side**: the censor sees the SNI and the IP
  that answers it, so the target depends on which server is the TLS listener
  (any carrier: mtcp, l3mtcp, tls). **Reverse** mode: the Iran server listens;
  pick an **Iranian** site hosted on Iranian IPs (the splice to it leaves from
  Iran too). **Direct** mode: the kharej server listens; pick a **foreign**
  site, best on the same provider / ASN (e.g. Hetzner for a Hetzner server).
  `hs2 reality targets -u U` reads the side from the tunnel's config (or
  `--side iran|kharej`), checks that side's vetted list and shows per row the
  country / ASN and why it was accepted or rejected; `hs2 reality check-target
  HOST -u U` and `hs2 reality front` reject a target whose IPs are in the
  wrong country (MISMATCH) and any site on a multi-tenant CDN: Cloudflare,
  Fastly, Akamai, CloudFront and the Iranian ArvanCloud, Sotoon CDN and Derak
  (by IP range / ASN, response headers or a shared certificate). Country / ASN
  come from a built-in table plus, when reachable, the Team Cymru DNS whois
  (1.5 s timeout; `--offline` turns it off); when neither knows the IP, a TCP
  connect under 10 ms from the listener counts as "same region". The doctor's
  front category warns when a configured target does not match the side.
  Vetted lists and evidence: `docs/reality-architecture.md` §9.1.
- **Pure `reality` and a restarting dialer**: the dialing server learns
  each IP's reality key from the announcement and keeps it on disk
  (`<config>.peer-fronts.json`, 0600), so after a restart it dials the
  remembered key (as `both`) until the announcement arrives. For a dialer
  that never heard it (new install, or down during a key change), run
  `hs2 reality pin-code -u U` on the listening server and paste the printed
  `hs2 reality pin -u U hs2rp1.…` on the dialing one (menu screen 4, `p`).
  `hs2 reality front … reality` refuses to make the last tls-capable IP pure
  reality until the dialer has shown it has the key (or `--dialer-has-key`);
  the doctor's `reality cold start` check covers both sides.
- **`both` and resets** (rel50): only a refusal that the key or the
  listener explains (the certificate did not prove the key; our listener
  closed because its target is down) moves a path to tls for 10 minutes; a
  reset or timeout of the reality handshake counts as a path failure for the
  LB instead, so a censor resetting Reality cannot push the tunnel onto tls.
  The classic installer now writes the tunnel's name (`sni`) on the
  listening side too. The old `"carrier": "reality"` is still accepted but
  warned about in the tunnel list, on upgrade and in `hs2 doctor`.
- **Weights** must name every IP of this side and sum to 100 (`--equal`
  splits evenly, the remainder to the first).
- **Park** sets the IP's weight to 0 and gives its share to the others in
  proportion; never the only active IP. The park is stored in
  `/var/lib/hs2/lb-park-<unit>.json` and `hs2-lb-park.timer` (a real unit,
  every minute, so it survives a reboot) runs `hs2 lb park --sweep`: an
  expired park is restored through the safe apply (the exact weights from
  before the park when the set of IPs did not change, else an equal share),
  the next run confirms the restore once its trial is healthy and only then
  drops the park; after three restores that rolled back it gives up and
  `hs2 lb show` says to run `hs2 lb unpark` by hand. The timer removes itself
  when nothing is parked.
- **Bilateral**: the lb lists live on both servers; after a change the
  screen prints which fields the other server must change and a ready
  `hs2 tunnel join --update '<code>'` for it.

Before M4 the validator took the wrong "own lb list" on the kharej side of a
reverse tunnel (it treated the side that *dials* as the Iran side), so a
`join --update` that carried an lb section failed there with "… is not an
address of this server"; fixed (Iran is mode `dial` in both directions).

### Speed & performance (new menu → 5)

Every action is an `hs2 perf` command (a binary without it: the classic
kernel-tuning / live-monitor / ports items, which stay reachable as item 6).

```
hs2 perf show    -u U [--advanced]           # effective keys: set (which drop-in) / default / off
hs2 perf presets                             # what each preset trades
hs2 perf preset  -u U production|low-cpu|max-throughput|min-latency
hs2 perf set     -u U KEY=VALUE [KEY=VALUE…] # documented keys only
hs2 perf unset   -u U KEY [KEY…]
hs2 perf nic                                 # queues, rx-IRQ core, softirq/idle per core (~2 s)
hs2 perf cores   -u U --keep N | --clear     # CPUAffinity for an hs2 unit (never x-ui / xray)
hs2 perf kernel  [--budget 600 [--measure 10s]] [--revert-budget]
hs2 perf speedtest --port P [--secs 5] [--gap 3s]
```

- **Presets and toggles** are `Environment=` lines in one drop-in,
  `/etc/systemd/system/<unit>.service.d/50-hs2-perf.conf`, applied with the
  safe apply: the drop-ins are part of the diff and of the history snapshot,
  and a rollback (not confirmed, unhealthy) restores the previous drop-ins.
  Only the keys that act on this side are written (an Iran-only key never
  lands on kharej).

  | preset | keys | trade |
  |--------|------|-------|
  | `production` (recommended) | none — the code defaults (table above) | the A/B-tested set; removes `50-hs2-perf.conf` |
  | `low-cpu` | `HS2_MCONN_STRIPE=0 HS2_SMUX_AUTOWIN=0 HS2_STRIPE_AW_SOLO=0 HS2_STRIPE_AW_HEAVY=0 HS2_L3_DUP=0` | less CPU per Mbit; one connection tops out near ~200 Mbit/s at 80 ms, no call duplication |
  | `max-throughput` | `HS2_SMUX_AUTOWIN_SHARE_MB=64`, kharej `HS2_MCONN_STRIPE_JUMP_MS=1500` | **experimental, not recommended** (labelled so in the menu): more window memory; light users may queue a little more under very heavy load — not lab-proven beyond the defaults |
  | `min-latency` | `HS2_SMUX_AUTOWIN_SHARE_MB=16 HS2_MCONN_STRIPE=4 HS2_STRIPE_AW_HEAVY=0` | big downloads slower, less queueing for calls / games |

  `show` lists each documented key with a one-line explanation and where its
  value comes from (`set: <drop-in>` — the last drop-in in name order wins —,
  `default` when the production defaults apply to this carrier/side and
  `HS2_DEFAULTS=legacy` is not set, else `off`). Simple mode hides the keys
  without a plain explanation; `--advanced` shows all.
- **Cores & NIC** reuses the server doctor's detection (sysfs queues, the
  current `Combined:` of `ethtool -l`, two `/proc/interrupts` and
  `/proc/stat` samples). With one rx queue (or ≥ 90 % of the rx interrupts
  on one core) it offers to keep that core free for the hs2 units:
  `60-hs2-cpu.conf` with `CPUAffinity=<all other cores>`, previewed as a diff
  and applied with the safe apply (auto-rollback). Only units named `hs2…`
  are accepted; x-ui / xray are **never** changed — the screen only repeats
  the optional, manual `taskset` advice for a panel's xray.
- **Kernel** shows congestion control, qdisc, buffers, `netdev_budget`,
  `netdev_budget_usecs`, `netdev_max_backlog`, and offers
  `net.core.netdev_budget=600`: it measures the rx core's softirq/idle for
  `--measure` (10 s), writes `/etc/sysctl.d/90-hs2-netdev-budget.conf`
  (with the previous value as a comment) + `sysctl -w`, measures again and
  reverts by itself if idle dropped or softirq grew by more than 3 points.
  `--revert-budget` removes the file and restores the previous value. RFS is
  **not** offered (tested and rejected, see "Single-queue virtio NIC").
- **Speed test** through the tunnel: prerequisites — `iperf3` on both
  servers, `iperf3 -s -B 127.0.0.1 -D` on kharej, and a user port on Iran
  whose target is `127.0.0.1:5201` on kharej (e.g. `hs2 tunnel join …
  --expose 127.0.0.1:5201` on kharej, or a per-port target). Then
  `hs2 perf speedtest --port P` runs down1 / down8 / up1 / up8
  (`iperf3 -c 127.0.0.1 -p P [-R] [-P 8]`, 5 s each, 3 s apart). It only
  ever connects to `127.0.0.1:P`, i.e. through the tunnel — never a raw
  multi-Gbit test to another host.
  A user's own speedtest (speedtest.net, fast.com) through the tunnel shows
  a "loaded ping" (ping during the download/upload) that mostly measures
  the client's access link — an LTE modem or home router queueing its own
  traffic (bufferbloat) — not the tunnel: the tunnel serves light flows
  ahead of downloads. Compare with a ping from the Iran server to kharej at
  the same moment; if that one is flat, the extra delay is client-side.

### Alerts (Telegram, optional)

`hs2 alert -c /etc/hs2/config.json` checks one tunnel and sends short alerts.
Turn it on from the tunnel manager: **a) Alerts (Telegram)** installs a
systemd timer (`hs2-alert-<unit>.timer`, one check every 60 s); the same menu
sets the bot token, chat id, proxy, language and summary time, sends a test
message and shows the last alerts. It only reads (status file, `systemctl`,
`journalctl`, `/proc/pressure/cpu`, the cert file); it writes only its own
state (`/var/lib/hs2/alert-<unit>.json`, one per tunnel) and log.

Every alert goes to the journal (`journalctl -t hs2-alert`) and to
`/var/log/hs2-alert.log`; to Telegram as well when `/etc/hs2/alert.conf`
(mode 600 — it holds the token, which is never logged) has a token and chat
id. Iran servers usually cannot reach `api.telegram.org`: set a `proxy`
(`http://…` or `socks5://…`) or configure the token on the Kharej server only.

| Rule | Fires when | Guards against false alarms |
|---|---|---|
| service down | unit `failed`/`inactive` | `activating` (a restart in progress) must last 2 checks |
| restarted / panic | MainPID or NRestarts changed; `panic:`/`fatal error:` in the journal | a panic line is reported once; no separate restart line when an outage just resolved |
| no serving link | users connected and 0 serving links | — (immediate, critical) |
| links low | serving < 50 % of the autopilot's **current** target, 3 checks | ignored for target ≤ 4, in the 3 min after a (re)start, and while the pool is scaling/probing/shrinking; retiring links do not count |
| IP fully cut | every LB path using an IP (v4 or v6) is cut, 3 checks | a single cut path of an IP that still has another path, and draining paths, are quiet |
| path flapping | ≥ 4 cuts of one path within an hour | cuts during the 3 min after a restart are not counted |
| RTT high | p50 > max(1.6 × baseline, baseline + 30 ms) for 5 checks | baseline = the tunnel's own median for that hour of day (after 2 days), else its 24 h median; needs an hour of history; all paths up together → **one** "network-wide" message |
| CPU pressure | `some avg60` > 25 %, 3 checks | — |
| certificate | cert_file expires in < 10 days | a side without a cert file (the dialing side) is "unknown", never an alert |

A missing or stale status file (the daemon restarting) is *unknown*: active
alerts are neither resolved nor raised. Each alert is sent once, repeated at
most every 30 min while it lasts, and followed by one "resolved" message.
A daily summary (default 09:00) lists peak users, peak Mbit/s, the worst RTT
hour, cuts per IP and restarts.

`/etc/hs2/alert.conf` (key=value):

```
bot_token=123456:ABC…      # from @BotFather
chat_id=123456789
proxy=socks5://127.0.0.1:1080   # optional
lang=fa                    # fa | en
summary_time=09:00         # HH:MM | off
resend_min=30
rtt_factor=1.6
rtt_min_add_ms=30
cpu_pressure_pct=25
cert_days=10
flap_cuts=4
probe=off                  # off | tcp | tls: connect to the tunnel's own listen port
```

### Monitoring & alerts and Server doctor (new menu → 6 / 7)

**Screen 6** renders `hs2 alert conf show` (the token only as "set (hidden)")
and the alert timer state of every tunnel (`hs2-alert-<unit>.timer`, one per
tunnel; alerts are per tunnel, the settings are shared). Items: bot token
(read with hidden input and handed to hs2 **on stdin** — never on a command
line, never echoed, never logged), chat id, proxy, language, Telegram
reachability check + test message, alerts on/off per tunnel, one alert kind
on/off, thresholds, quiet hours, daily summary time and content, the last 7
days of alerts, and "send the summary now". Behind it:

```
hs2 alert conf show | set KEY=VALUE … | token      # validated, atomic, mode 600; bot_token only via `token` (stdin)
hs2 alert rule NAME on|off                         # unit restart panic dead links ipcut flap rtt psi cert probe
hs2 alert history [--days 7] [-u UNIT]             # from /var/log/hs2-alert.log, with a per-day count
hs2 alert summary-now [-u UNIT]                    # the day so far, every watched tunnel (does not reset the day)
hs2 alert telegram-check                           # api host reachable directly? the proxy? (Iran: explains the proxy)
```

New `alert.conf` keys (an invalid value is refused with its line number and
nothing is written):

```
disable=rtt,psi                 # alert kinds never sent
quiet_hours=23:30-07:00         # may cross midnight; off = none
summary_items=users,mbit,rtt,cuts,restarts,alerts   # what the daily summary shows (default: all)
```

In quiet hours only **critical** alerts (and the "resolved" of a critical
one, the daily summary at the chosen time, a test) reach Telegram; the others
are still written to the journal and the log, marked `[quiet hours: not
sent]`. An Iran server that cannot reach `api.telegram.org` is detected by
the reachability check, which says to set a `proxy` (socks5/http, e.g. a
local xray socks inbound). Without root the screen says so and changes
nothing (`alert.conf` and the timers belong to root).

**Screen 7** runs `hs2 doctor --deep --fixes` for all checks or one category:

```
hs2 doctor --deep [--only network|cpu|tunnel|reality|panel] [--fixes] [--report FILE]
hs2 doctor --fix N [--yes] [--hours H] [--confirm-within 5m]    # fix N of the last --fixes run (≤ 30 min old)
```

Categories: network (NIC/softirq, path quality without the tunnel), cpu
(pressure, `netdev_budget` vs. `time_squeeze`), tunnel (health, restarts and
panics, monitoring, certificate expiry), reality (xray dest), panel (x-ui).
A finding gets a numbered **fix** only when a safe automatic one exists, and
every fix shows a preview and runs an hs2 command that has its own safety net:

| Finding | Fix | Safety |
|---|---|---|
| single rx core full, hs2 on it | `hs2 perf cores -u U --keep N --yes` per tunnel | safe apply, auto-rollback unless `hs2 apply --confirm` |
| `netdev_budget` < 600 and `time_squeeze` grew | `hs2 perf kernel --budget 600 --yes` | measured before/after, reverted when not better |
| 24 h sampler off | `hs2 stats timer on` | harmless; off again in menu 9 |
| alerts off for a tunnel | opens screen 6 | — |
| an LB IP bad even without the tunnel | `hs2 lb park -u U --ip IP --hours H` (the menu asks H) | restored by `hs2-lb-park.timer`, also after a reboot |
| certificate expires in < 21 days | `certbot renew --dry-run` + the renewal command | dry run changes nothing |
| x-ui / xray / Reality / panel | none — advice text only | never changed |

A fix whose tunnel no longer exists is skipped ("does not exist on this
server any more"); a fix of a run older than 30 min is refused (run the
doctor again); non-root is refused with an explanation. **r)** saves the
report to `/root/hs2-doctor-<date>.txt` (non-root: `$HOME`) for support;
findings carry no keys or passwords.

Only real tunnel units are counted in the doctor's "restarts / panics"
lines: a unit whose `ExecStart` is `hs2 run -c /etc/hs2/<name>.json`.
`hs2-coreguard`, `hs2-alert-*`, `hs2-stats`, `hs2-lb-park` are not tunnels.
A stale status file that belongs to no config in `/etc/hs2` (a deleted tunnel,
a test run) is reported as INFO with the `rm` command, not as a stopped tunnel.

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
- `net.ipv4.tcp_mtu_probing=1` is required on both servers (see "Automatic
  kernel tuning"); `hs2 doctor` warns otherwise.
- Building from source, the architecture, and the test lab: `hs2-src/BUILD.md`.

### Single-queue virtio NIC (one rx interrupt core)

Many VPSes have a virtio NIC with a single rx/tx queue: every received packet
raises its interrupt (and its softirq work) on **one** CPU core. At peak that
core can run out long before the others do, and the tunnel's own threads
landing on it make it worse. These are **optional manual steps** — hs2 and
the installer apply none of them (its automatic tuning covers sysctls sized
from RAM/cores only, not IRQ or CPU placement).

How to see it:

- `ethtool -l eth0` — `Combined: 1` under *Current hardware settings* means
  one queue.
- `grep -E 'virtio|eth0' /proc/interrupts` — the `input.0` (rx) counter grows
  on one CPU column only; that is the rx-IRQ core.
- `mpstat -P ALL 1` at peak — that core shows a high `%soft` and little
  `%idle` while the others have room.

What helped on the reference kharej server (say the rx-IRQ core is CPU 0 of
4; adjust to yours):

```
# 1. keep the rx-IRQ core free for the NIC: run the hs2 units on the others
#    (a drop-in per tunnel unit, e.g. /etc/systemd/system/hs2.service.d/cpu.conf)
[Service]
CPUAffinity=1 2 3
#    then: systemctl daemon-reload && systemctl restart hs2
#    optionally keep other heavy processes off it too, e.g. a panel's xray:
#    taskset -a -cp 1-3 $(pidof xray)

# 2. a larger softirq budget per poll
sysctl -w net.core.netdev_budget=600
```

Result there: the rx-IRQ core's idle at peak went from ~22 % to ~45 %. To
keep the sysctl across reboots put it in a file of your own under
`/etc/sysctl.d/` (not `99-hs2.conf`, which hs2 rewrites). With several queues
(`Combined:` > 1) none of this is needed.

Tried and **not** kept: RFS (`net.core.rps_sock_flow_entries` +
`rx-0/rps_flow_cnt`). On the reference server single-stream upload got
worse with it (one connection 20–90 Mbit/s with RFS vs 177–590 without;
eight connections mixed), so it was turned back off.
