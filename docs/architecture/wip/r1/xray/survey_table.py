#!/usr/bin/env python3
"""survey_table.py DIR [VARIANT...]: one markdown row per step-1 summary
(DIR/VARIANT.json from survey.py): connections, which flows share one, and
the TCP game's RTT during the first download (xray alone, no hs2)."""
import json, os, sys

NAMES = {"xa": "VLESS+XHTTP+REALITY, بی xmux (پیش‌فرض 26.6.27: maxConnections 6)",
         "xc16": "XHTTP، maxConcurrency 16-32 (پیش‌فرض کلاینت‌های ≤ v25.9.11)",
         "xb1": "XHTTP، maxConcurrency 1 (پیش‌فرض v25.10.15 تا v26.6.22)",
         "xm3": "XHTTP، maxConnections 3 (پیش‌فرض از v26.7.28)",
         "xb2": "XHTTP، maxConcurrency 1 + cMaxReuseTimes 1",
         "xt": "VLESS+XHTTP+TLS، بی xmux (auto = packet-up)",
         "vv": "VLESS+RAW+REALITY+Vision",
         "vm": "VMess+RAW",
         "tj": "Trojan+RAW+TLS",
         "ws": "VLESS+WS+TLS",
         "hu": "VLESS+HTTPUpgrade+TLS",
         "gr": "VLESS+gRPC+TLS (multiMode خاموش)",
         "grm": "VLESS+gRPC+TLS (multiMode روشن)",
         "ss": "Shadowsocks-2022 (TCP و UDP)",
         "kc": "VLESS+mKCP (UDP)"}


def yn(b):
    return "بله" if b else "نه"


def main():
    d = sys.argv[1]
    vs = sys.argv[2:] or [f[:-5] for f in sorted(os.listdir(d)) if f.endswith(".json")]
    print("| حالت | TCP | UDP | بیشترین جریان روی یک اتصال | بازی+دانلود ۱ | بازی ۲+دانلود ۲ | بازی UDP+دانلود | عمر اتصال‌ها (ثانیه) | RTT بازی TCP در دانلود p50/p99 (ms) |")
    print("|---|---|---|---|---|---|---|---|---|")
    for v in vs:
        p = os.path.join(d, v + ".json")
        if not os.path.exists(p):
            continue
        s = json.load(open(p))
        lifes = sorted(c["life_s"] for c in s["conns"])
        life = "%s–%s" % (lifes[0], lifes[-1]) if lifes else "-"
        g = s["lat"].get("G1", {})
        print("| %s (`%s`) | %d | %d | %d از %d | %s | %s | %s | %s | %s / %s |" % (
            NAMES.get(v, v), v, s["tcp_conns"], s["udp_tuples"], s["max_flows_on_one_conn"], s["proxied_flows"],
            yn(s["pairs"]["G1+D1"]), yn(s["pairs"]["G2+D2"]), yn(s["pairs"]["U1+D1"]), life,
            g.get("dl_p50"), g.get("dl_p99")))


if __name__ == "__main__":
    main()
