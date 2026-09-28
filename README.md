# hs2-tunnel

A DPI-resistant tunnel between an Iran server and a foreign (kharej) server.
Runs **alongside Backhaul** without touching it — its own subnet, its own ports.

Current layer: **L3-GRE over multi-link TLS** — IP packets are carried across
several parallel real-TLS connections (Chrome fingerprint, real Let's Encrypt
certificate, active-probe resistance), auto-scaled by load.

## What it does

- Opens user ports on the Iran server and forwards them, over the tunnel, to a
  panel inbound on the kharej server — the same role Backhaul plays.
- Carries the traffic inside ordinary-looking TLS 1.3 to a real domain, so a
  passive observer sees an HTTPS connection, and an active probe that connects
  gets a real certificate and a plain web page.
- Survives carrier drops: the tunnel interface and user ports stay up while
  links reconnect underneath.

## Requirements

- Two Linux servers (tested on Ubuntu 22.04+), root access.
- A domain whose A record points to the **kharej** server.
- Port 80 free on the kharej server during first install (for the certificate),
  or an existing Let's Encrypt certificate for the domain.
- A free tunnel port on the kharej server (default 2096).

## Install

### 1. Kharej (foreign server)

```bash
mkdir -p /root/hs2 && cd /root/hs2
curl -fL -o install.sh https://raw.githubusercontent.com/hosseintaghipoursori-alt/hs2-tunnel/main/install.sh
bash install.sh
```

Choose **1**, answer the prompts (domain, tunnel port, panel inbound address).
At the end it prints a **`hs2://…` setup link** — copy it.

### 2. Iran server

```bash
mkdir -p /root/hs2 && cd /root/hs2
curl -fL -o install.sh https://raw.githubusercontent.com/hosseintaghipoursori-alt/hs2-tunnel/main/install.sh
bash install.sh
```

Choose **2**, paste the `hs2://` link, pick the user port(s).

> If GitHub is unreachable from the Iran server, copy `hs2-linux-amd64` from the
> kharej server into `/root/hs2/` first; the installer uses a local binary when
> present.

### 3. Point clients at Iran

In your panel, take a client config and change only its **address** to the Iran
server's IP (and port, if you chose a different one). Everything else — UUID,
SNI, security — stays the same.

## Managing

```bash
bash install.sh    # 3 = uninstall, 4 = status/logs
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

- The tunnel uses subnet `10.77.0.0/30` and interface `hs0`. Backhaul is
  untouched.
- Multi-link auto-scales between 4 and 16 parallel links based on load.
- This is early software under active development; test alongside your existing
  tunnel before relying on it.
