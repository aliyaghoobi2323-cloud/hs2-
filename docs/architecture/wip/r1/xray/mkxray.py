#!/usr/bin/env python3
"""mkxray.py: the lab's xray configs (R1 round 3, xray). Keys come from a
KEY=VALUE file kept outside the repo (xray x25519, xray uuid, xray vlessenc,
a random shortId, path and Shadowsocks-2022 key); nothing secret is printed.

  mkxray.py server KEYS OUT.json --listen IP --target HOST:PORT --cert C --key K
                   --access LOG --inbounds xr:4443,vv:4444,...
  mkxray.py client KEYS OUT.json --variant xa --addr IP --port P [--log LOG]

Inbound kinds (server) and the client variants that use them:
  xr  VLESS + XHTTP (mode auto) + REALITY, VLESS Encryption (ML-KEM-768,
      native, 600 s tickets), sniffing as the owner's panel inbound
        xa   no xmux in the client (the panel's XMUX toggle off: xray's
             built-in XMUX defaults apply; in 26.6.27, maxConnections 6)
        xc16 maxConcurrency 16-32 (the default of clients up to v25.9.11)
        xb1  maxConcurrency 1 (the default of v25.10.15 to v26.6.22)
        xm3  maxConnections 3 (the default from v26.7.28)
             (all three with hMaxRequestTimes 600-900 and hMaxReusableSecs
             1800-3000, as those defaults)
        xb2  xmux maxConcurrency 1 and cMaxReuseTimes 1 (one proxied
             connection per HTTP/2 connection, never reused)
  xt  VLESS + XHTTP (mode auto) + TLS          xt   no xmux
  vv  VLESS + RAW + REALITY + xtls-rprx-vision vv
  vm  VMess + RAW (no TLS)                      vm
  tj  Trojan + RAW + TLS                        tj
  ws  VLESS + WebSocket + TLS                   ws
  hu  VLESS + HTTPUpgrade + TLS                 hu
  gr  VLESS + gRPC + TLS                        gr (multiMode off), grm (on)
  ss  Shadowsocks-2022 (2022-blake3-aes-128-gcm), TCP and UDP   ss
  kc  VLESS + mKCP (UDP)                        kc
"""
import argparse, json, sys

SNI = "www.example.test"


def keys(path):
    k = {}
    for line in open(path):
        line = line.strip()
        if "=" in line:
            a, b = line.split("=", 1)
            k[a] = b
    return k


SNIFF = {"enabled": True, "destOverride": ["http", "tls", "quic", "fakedns"]}


def xhttp_settings(k, xmux=None):
    s = {"mode": "auto", "path": k["XPATH"], "host": SNI, "xPaddingBytes": "100-1000",
         "scMaxBufferedPosts": 30, "scStreamUpServerSecs": "20-80"}
    if xmux is not None:
        s["xmux"] = xmux
    return s


def reality_server(k, target):
    return {"show": False, "xver": 0, "target": target, "serverNames": [SNI],
            "privateKey": k["PRIV"], "shortIds": [k["SID"]]}


def tls_server(cert, key):
    return {"certificates": [{"certificateFile": cert, "keyFile": key}], "alpn": ["h2", "http/1.1"]}


def inbound(kind, port, a, k):
    vl = lambda flow="", dec="none": {"clients": [{"id": k["UUID"], "flow": flow, "email": "u@lab"}],
                                     "decryption": dec}
    base = {"tag": "in-" + kind, "listen": a.listen, "port": port, "sniffing": SNIFF}
    rs = {"security": "reality", "realitySettings": reality_server(k, a.target)}
    ts = {"security": "tls", "tlsSettings": tls_server(a.cert, a.key)}
    if kind == "xr":
        base.update(protocol="vless", settings=vl(dec=k["DEC"]),
                    streamSettings=dict(network="xhttp", xhttpSettings=xhttp_settings(k), **rs))
    elif kind == "xt":
        base.update(protocol="vless", settings=vl(),
                    streamSettings=dict(network="xhttp", xhttpSettings=xhttp_settings(k), **ts))
    elif kind == "vv":
        base.update(protocol="vless", settings=vl("xtls-rprx-vision", k["DEC"]),
                    streamSettings=dict(network="raw", **rs))
    elif kind == "vm":
        base.update(protocol="vmess", settings={"clients": [{"id": k["UUID"], "email": "u@lab"}]},
                    streamSettings={"network": "raw", "security": "none"})
    elif kind == "tj":
        base.update(protocol="trojan", settings={"clients": [{"password": k["UUID"], "email": "u@lab"}]},
                    streamSettings=dict(network="raw", **ts))
    elif kind == "ws":
        base.update(protocol="vless", settings=vl(),
                    streamSettings=dict(network="ws", wsSettings={"path": k["XPATH"], "host": SNI}, **ts))
    elif kind == "hu":
        base.update(protocol="vless", settings=vl(),
                    streamSettings=dict(network="httpupgrade",
                                        httpupgradeSettings={"path": k["XPATH"], "host": SNI}, **ts))
    elif kind == "gr":
        base.update(protocol="vless", settings=vl(),
                    streamSettings=dict(network="grpc", grpcSettings={"serviceName": k["XPATH"].strip("/")}, **ts))
    elif kind == "ss":
        base.update(protocol="shadowsocks",
                    settings={"method": "2022-blake3-aes-128-gcm", "password": k["SSK"], "network": "tcp,udp"})
    elif kind == "kc":
        base.update(protocol="vless", settings=vl(), streamSettings={"network": "kcp", "security": "none"})
    else:
        sys.exit("unknown inbound kind " + kind)
    return base


def server(a, k):
    ins = []
    for item in a.inbounds.split(","):
        kind, port = item.split(":")
        ins.append(inbound(kind, int(port), a, k))
    return {"log": {"loglevel": a.loglevel, "access": a.access, "error": a.error or ""},
            "inbounds": ins,
            # the lab's targets are on 127.0.0.1, which freedom refuses by
            # default since Xray 26 ("blocked target"): allow loopback only
            "outbounds": [{"protocol": "freedom", "tag": "direct",
                           "settings": {"finalRules": [{"action": "allow", "ip": ["127.0.0.0/8"]}]}},
                          {"protocol": "blackhole", "tag": "blocked"}]}


VARIANT = {"xa": "xr", "xc16": "xr", "xb1": "xr", "xm3": "xr", "xb2": "xr", "xt": "xt", "vv": "vv", "vm": "vm", "tj": "tj",
           "ws": "ws", "hu": "hu", "gr": "gr", "grm": "gr", "ss": "ss", "kc": "kc"}


def client(a, k):
    v = a.variant
    kind = VARIANT[v]
    rc = {"security": "reality", "realitySettings": {"serverName": SNI, "fingerprint": "chrome",
                                                     "password": k["PUB"], "shortId": k["SID"], "spiderX": "/"}}
    tc = {"security": "tls", "tlsSettings": {"serverName": SNI, "fingerprint": "chrome", "pinnedPeerCertSha256": k["PCS"]}}
    user = lambda flow="", enc="none": {"id": k["UUID"], "encryption": enc, "flow": flow}
    vnext = lambda u: {"vnext": [{"address": a.addr, "port": a.port, "users": [u]}]}
    if kind == "xr":
        life = {"hMaxRequestTimes": "600-900", "hMaxReusableSecs": "1800-3000"}
        xmux = {"xa": None,
                "xc16": dict(maxConcurrency="16-32", **life),
                "xb1": dict(maxConcurrency="1", **life),
                "xm3": dict(maxConnections="3", **life),
                "xb2": {"maxConcurrency": "1", "cMaxReuseTimes": "1"}}[v]
        ob = dict(protocol="vless", settings=vnext(user(enc=k["ENC"])),
                  streamSettings=dict(network="xhttp", xhttpSettings=xhttp_settings(k, xmux), **rc))
    elif kind == "xt":
        ob = dict(protocol="vless", settings=vnext(user()),
                  streamSettings=dict(network="xhttp", xhttpSettings=xhttp_settings(k), **tc))
    elif kind == "vv":
        ob = dict(protocol="vless", settings=vnext(user("xtls-rprx-vision", k["ENC"])),
                  streamSettings=dict(network="raw", **rc))
    elif kind == "vm":
        ob = dict(protocol="vmess", settings=vnext({"id": k["UUID"], "security": "auto"}),
                  streamSettings={"network": "raw", "security": "none"})
    elif kind == "tj":
        ob = dict(protocol="trojan", settings={"servers": [{"address": a.addr, "port": a.port, "password": k["UUID"]}]},
                  streamSettings=dict(network="raw", **tc))
    elif kind == "ws":
        ob = dict(protocol="vless", settings=vnext(user()),
                  streamSettings=dict(network="ws", wsSettings={"path": k["XPATH"], "host": SNI}, **tc))
    elif kind == "hu":
        ob = dict(protocol="vless", settings=vnext(user()),
                  streamSettings=dict(network="httpupgrade", httpupgradeSettings={"path": k["XPATH"], "host": SNI}, **tc))
    elif kind == "gr":
        ob = dict(protocol="vless", settings=vnext(user()),
                  streamSettings=dict(network="grpc", grpcSettings={"serviceName": k["XPATH"].strip("/"),
                                                                     "multiMode": v == "grm"}, **tc))
    elif kind == "ss":
        ob = dict(protocol="shadowsocks", settings={"servers": [{"address": a.addr, "port": a.port,
                                                                 "method": "2022-blake3-aes-128-gcm",
                                                                 "password": k["SSK"]}]})
    elif kind == "kc":
        ob = dict(protocol="vless", settings=vnext(user()), streamSettings={"network": "kcp", "security": "none"})
    ob["tag"] = "proxy"
    if a.send_through:
        ob["sendThrough"] = a.send_through
    return {"log": {"loglevel": a.loglevel, "error": a.error or "", "access": a.access or ""},
            "inbounds": [{"tag": "socks", "listen": "127.0.0.1", "port": 1080, "protocol": "socks",
                          "settings": {"auth": "noauth", "udp": True, "ip": "127.0.0.1"}}],
            "outbounds": [ob, {"protocol": "freedom", "tag": "direct"}]}


def main():
    p = argparse.ArgumentParser()
    p.add_argument("role", choices=["server", "client"])
    p.add_argument("keys")
    p.add_argument("out")
    p.add_argument("--listen", default="0.0.0.0")
    p.add_argument("--target", default="127.0.0.1:4431")
    p.add_argument("--cert", default="")
    p.add_argument("--key", default="")
    p.add_argument("--access", default="")
    p.add_argument("--error", default="")
    p.add_argument("--loglevel", default="info")
    p.add_argument("--inbounds", default="xr:4443")
    p.add_argument("--variant", default="xa")
    p.add_argument("--addr", default="10.231.2.1")
    p.add_argument("--port", type=int, default=4443)
    p.add_argument("--send-through", default="")
    a = p.parse_args()
    k = keys(a.keys)
    cfg = server(a, k) if a.role == "server" else client(a, k)
    with open(a.out, "w") as fh:
        json.dump(cfg, fh, indent=1)


if __name__ == "__main__":
    main()
