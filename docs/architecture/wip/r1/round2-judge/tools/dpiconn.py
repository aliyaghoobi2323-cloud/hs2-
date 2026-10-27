#!/usr/bin/env python3
"""dpiconn.py: the round-2 judge's connection profile of a capture (what a
censor sees per connection without reading a byte): for each TCP connection
on port 2096, when its SYN came (relative to the first), how long it was
seen, packets and payload bytes each way, the payload rate, the share of
the capture's bytes, and the gaps between its payload packets (median,
p90) in each direction. With the TLS records of the same capture
(tlsrec.py extract), also each connection's application records: count,
median and p10/p90 length, share >= 1400 B.

  dpiconn.py PCAP [RECORDS.json] [--md LABEL]
"""
import json
import os
import statistics as st
import struct
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tlsrec  # noqa: E402

PORT = 2096


def conns(pcap):
    cs = {}
    t0 = None
    for lt, t, data in tlsrec.pcap_packets(pcap):
        if lt == 1:
            off = 14
        elif lt == 113:
            off = 16
        elif lt == 276:
            off = 20
        else:
            continue
        ip = data[off:]
        if len(ip) < 20 or ip[0] >> 4 != 4 or ip[9] != 6:
            continue
        ihl = (ip[0] & 15) * 4
        tot = struct.unpack(">H", ip[2:4])[0]
        tcp = ip[ihl:]
        if len(tcp) < 14:
            continue
        sp, dp = struct.unpack(">HH", tcp[:4])
        doff = (tcp[12] >> 4) * 4
        flags = tcp[13]
        pl = tot - ihl - doff
        if sp == PORT:
            key, d = (dp, "dn"), "edge>exit"
        elif dp == PORT:
            key, d = (sp, "up"), "exit>edge"
        else:
            continue
        k = key[0]
        c = cs.setdefault(k, {"syn": None, "first": t, "last": t, "pk": {"edge>exit": 0, "exit>edge": 0},
                              "by": {"edge>exit": 0, "exit>edge": 0}, "ts": {"edge>exit": [], "exit>edge": []}})
        if t0 is None:
            t0 = t
        if flags & 0x02 and not flags & 0x10 and c["syn"] is None:
            c["syn"] = t
        c["last"] = t
        if pl > 0:
            c["pk"][d] += 1
            c["by"][d] += pl
            c["ts"][d].append(t)
    return cs, t0


def pct(v, p):
    if not v:
        return None
    v = sorted(v)
    return v[min(len(v) - 1, int(p * len(v)))]


def main():
    a = [x for x in sys.argv[1:] if not x.startswith("--")]
    label = sys.argv[sys.argv.index("--md") + 1] if "--md" in sys.argv else os.path.basename(a[0])
    cs, t0 = conns(a[0])
    recs = {}
    if len(a) > 1 and a[1] != label:
        for r in json.load(open(a[1])):
            if r["type"] != 23:
                continue
            port = int(r["c"].split(">")[0].split(":")[1])
            if port == PORT:
                port = int(r["c"].split(">")[1].split(":")[1])
            recs.setdefault(port, []).append(r["len"])
    total = sum(sum(c["by"].values()) for c in cs.values()) or 1
    syns = sorted(c["syn"] for c in cs.values() if c["syn"] is not None)
    print("#### %s: %d connections, SYNs spread %.2f s" % (label, len(cs), (syns[-1] - syns[0]) if len(syns) > 1 else 0))
    print("| connection | SYN at s | seen s | packets exit>edge / edge>exit | MB exit>edge / edge>exit | share of bytes | Mbit/s both | packet gap median / p90 ms (exit>edge) | records | record median [p10..p90] | >=1400 B |")
    print("|---|---|---|---|---|---|---|---|---|---|---|")
    for i, (k, c) in enumerate(sorted(cs.items(), key=lambda kv: kv[1]["syn"] or kv[1]["first"])):
        dur = max(c["last"] - c["first"], 1e-3)
        by = sum(c["by"].values())
        g = [1000 * (b - a_) for a_, b in zip(c["ts"]["exit>edge"], c["ts"]["exit>edge"][1:])]
        r = recs.get(k, [])
        print("| c%d | %s | %.1f | %d / %d | %.2f / %.2f | %.1f%% | %.2f | %s / %s | %s | %s | %s |" % (
            i, "-" if c["syn"] is None else "%.2f" % (c["syn"] - (syns[0] if syns else t0)), dur,
            c["pk"]["exit>edge"], c["pk"]["edge>exit"], c["by"]["exit>edge"] / 1e6, c["by"]["edge>exit"] / 1e6,
            100 * by / total, by * 8 / dur / 1e6,
            "-" if not g else "%.2f" % st.median(g), "-" if not g else "%.1f" % pct(g, 0.9),
            len(r) or "-", "-" if not r else "%d [%d..%d]" % (st.median(r), pct(r, 0.1), pct(r, 0.9)),
            "-" if not r else "%.0f%%" % (100 * sum(1 for x in r if x >= 1400) / len(r))))


if __name__ == "__main__":
    main()
