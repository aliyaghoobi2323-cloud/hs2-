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
*autopilot*) sizes the pool continuously between **2 and 32 links** — up when
traffic needs more, and **back down when it does not**:

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

It comes up "warm" (8 links) so a burst of connections at start spreads
immediately. In **reverse** mode the edge (which alone sees the users) drives
the exit's link count over a control channel, so the dial pool on the foreign
side follows it in both directions.

Watch it live: `hs2 status -c /etc/hs2/config.json` (or the **Live pattern
monitor** in `hs2-menu` → tunnel manager) shows the links up (serving +
retiring), the target, the phase (steady / scaling / probing / holding /
shrinking), *why* the pool is that size, and the traffic, e.g.:

```
links:   7 up = 5 serving + 2 retiring / target 5 (shrinking, range 2–32)
why:     18 active of 251 open connections, 1 of 5 serving links at their limit, peak 6.8 Mbit/s; 2 retiring link(s) close as their connections end (held by 38 open, 1 active)
traffic: 251 connections (18 active) · 6.1 Mbit/s · 1 link at its limit (~2.4 Mbit/s each)
```

## Automatic kernel tuning

hs2 sizes kernel network tuning from the server's **RAM and CPU cores** and
re-applies it every time the service starts (so a resized VPS is picked up on
restart). It picks BBR + fq_codel by default, scales socket buffers and
backlogs to a low/medium/high profile, and sets the multi-IP-friendly knobs.
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
  start / stop / restart / edit / logs / live pattern / tuning / **delete**.
  Delete asks you to type the name, saves a backup first, and removes only
  that tunnel's service, config and interface — the others keep running.
- **Upgrade** restarts every tunnel on the new binary and checks each one
  reconnects. **Backup / restore** cover all tunnels. **Uninstall** removes all
  of them (delete one in the manager). The certificate-renewal hook reloads
  every hs2 tunnel, so no renewal ever points at a deleted service.

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
  the tunnel's own packets are dropped (an nftables rule, iptables as a
  fallback) — the server still answers ordinary ping on every IP.
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
  stream core. hs0 (10.77.0.1/2) remains in those modes as a side channel for
  ping and non-TCP traffic, with a 60 ms queue-time limit.
- **Connections spread across links.** Connections that arrived together all
  landed on the first link, so per-connection throttling capped the whole
  burst. They are now spread evenly.
- **Latency tuning measured in a lab** (lossy, throttled, long-haul paths):
  BBR on every link socket, a 32 KiB unsent-data limit, 16 KiB stream frames,
  and an adaptive 2–32 link pool sized live by the autopilot (see above).
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
- A domain whose A record points to the **kharej** server.
- Port 80 free on the kharej server during first install (for the certificate),
  or an existing Let's Encrypt certificate for the domain.
- A free tunnel port on the kharej server (default 2096).

## Install

### 1. Kharej (foreign server)

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/main/install.sh)
```

Choose **1**, answer the prompts (domain, tunnel port, panel inbound, mode,
UDP). At the end it prints a **`hs2://…` setup link** — copy it.

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

v3 is **not** wire-compatible with v2. Upgrade the **kharej server first, then
Iran**; the tunnel is down only between the two. Config and the `hs2://` link
stay the same:

```bash
curl -fsSL https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/main/install.sh | bash -s upgrade
```

If the Iran server cannot reach GitHub, copy the new binary from kharej first
(`scp /usr/local/bin/hs2 root@IRAN_IP:/root/hs2/hs2-linux-amd64`), then on Iran
run `cd /root/hs2 && bash install.sh upgrade`.

Existing configs keep their mode. To switch, edit `"carrier"` in
`/etc/hs2/config.json` on both servers and `systemctl restart hs2`.

## Modes

| mode     | links | hs0 tunnel IPs | use it when                                  |
|----------|-------|----------------|----------------------------------------------|
| `mtcp`   | 2–32  | no             | default — fastest, beats per-connection caps |
| `l3mtcp` | 2–32  | yes            | you also need 10.77.0.x (ping, non-TCP)      |
| `tls`    | 1     | yes            | you want a single connection on the wire     |

On a slow path that is **not** throttled per connection, fewer links give
lower latency under full load, because several parallel flows keep a standing
queue in the path. The autopilot handles this automatically — it keeps added
links only when they raise throughput — but you can also cap it by lowering
`max_links` in the Iran config.

## Security

- TLS 1.3 with a real Let's Encrypt certificate; the client looks like Chrome.
- Links are authenticated in both directions with HMAC-BLAKE2s over the shared
  key and the TLS session's exporter secret. The client sends no traffic until
  the server has proven the key, so certificate forgery or interception gets
  nothing. Auth records are padded to normal HTTP sizes.
- Anything that is not an authenticated hs2 client is served the cover
  website, including short or malformed requests.

## Managing

```bash
bash install.sh    # 3 = uninstall, 4 = status/logs, 5 = upgrade
systemctl status hs2
journalctl -u hs2 -f
```

## The three ports (they are different things)

| where | what | example |
|-------|------|---------|
| Iran | user port clients connect to | 8443 |
| Kharej | panel inbound the tunnel delivers to | 127.0.0.1:8443 |
| Kharej | tunnel port Iran dials (clients never see it) | 2096 |

The Iran user port and the kharej panel port may share a number (different
servers); the tunnel port must differ from the panel port on the kharej server.

## Notes

- The installer applies BBR/fq kernel tuning system-wide
  (`/etc/sysctl.d/99-hs2.conf`); hs2 also sets BBR on its own sockets.
- Building from source, the architecture, and the test lab: `hs2-src/BUILD.md`.
