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
  8–16 links.
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
bash <(curl -fsSL https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/claude/amazing-meitner-vl4b5d/install.sh)
```

Choose **1**, answer the prompts (domain, tunnel port, panel inbound, mode,
UDP). At the end it prints a **`hs2://…` setup link** — copy it.

### 2. Iran server

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/claude/amazing-meitner-vl4b5d/install.sh)
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
curl -fsSL https://raw.githubusercontent.com/aliyaghoobi2323-cloud/hs2-/claude/amazing-meitner-vl4b5d/install.sh | bash -s upgrade
```

If the Iran server cannot reach GitHub, copy the new binary from kharej first
(`scp /usr/local/bin/hs2 root@IRAN_IP:/root/hs2/hs2-linux-amd64`), then on Iran
run `cd /root/hs2 && bash install.sh upgrade`.

Existing configs keep their mode. To switch, edit `"carrier"` in
`/etc/hs2/config.json` on both servers and `systemctl restart hs2`.

## Modes

| mode     | links | hs0 tunnel IPs | use it when                                  |
|----------|-------|----------------|----------------------------------------------|
| `mtcp`   | 8–16  | no             | default — fastest, beats per-connection caps |
| `l3mtcp` | 8–16  | yes            | you also need 10.77.0.x (ping, non-TCP)      |
| `tls`    | 1     | yes            | you want a single connection on the wire     |

On a slow path that is **not** throttled per connection, fewer links give
lower latency under full load: on the 8 Mbit lab path a single link (`tls`)
measured 333 ms against 628 ms for 8 links, because several parallel flows
keep a standing queue in the path. Lower `min_links`/`max_links` in the Iran
config for such a path; on throttled paths keep 8.

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
