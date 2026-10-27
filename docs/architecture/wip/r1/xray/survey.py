#!/usr/bin/env python3
"""survey.py RAWDIR VARIANT [--json OUT]: one step-1 run's connection map.

Inputs (written by survey.sh): RAWDIR/VARIANT.pcap (the mid namespace's
client-facing veth, headers only), RAWDIR/VARIANT-flows.json (xtraffic.py
client), RAWDIR/VARIANT-access.log (the xray server's access log lines of this
run). Output: every TCP connection and UDP 5-tuple between the client and the
server, with open/close times (s from the client's t0), who closed, bytes each
way, and the proxied flows (game, udpgame, dl, web) the server's access log
puts on it ("from <client ip:port>"), and which flows share one; plus each
game's RTT before and during the downloads (xray alone, no hs2).
"""
import argparse, json, re, struct, sys
from collections import defaultdict

CLIENT_NET = "10.231.1."
KIND = {7001: "game", 7002: "dl", 7003: "web", 7004: "udpgame"}


def ip4(b):
    return "%d.%d.%d.%d" % tuple(b)


def read_pcap(path):
    with open(path, "rb") as fh:
        gh = fh.read(24)
        if len(gh) < 24:
            return
        magic = struct.unpack("<I", gh[:4])[0]
        en = "<" if magic in (0xa1b2c3d4, 0xa1b23c4d) else ">"
        nano = magic in (0xa1b23c4d, 0x4d3cb2a1)
        while True:
            rh = fh.read(16)
            if len(rh) < 16:
                return
            ts, tf, incl, orig = struct.unpack(en + "IIII", rh)
            d = fh.read(incl)
            t = ts + tf / (1e9 if nano else 1e6)
            if len(d) < 34 or d[12:14] != b"\x08\x00":
                continue
            ihl = (d[14] & 15) * 4
            tot = struct.unpack("!H", d[16:18])[0]
            proto = d[23]
            src, dst = ip4(d[26:30]), ip4(d[30:34])
            l4 = 14 + ihl
            if len(d) < l4 + 4:
                continue
            sp, dp = struct.unpack("!HH", d[l4:l4 + 4])
            if proto == 6 and len(d) >= l4 + 14:
                doff = (d[l4 + 12] >> 4) * 4
                flags = d[l4 + 13]
                pay = tot - ihl - doff
            elif proto == 17:
                flags = 0
                pay = tot - ihl - 8
            else:
                continue
            yield t, proto, src, sp, dst, dp, flags, max(pay, 0)


def conns_from_pcap(path, t0):
    cs = {}
    for t, proto, src, sp, dst, dp, flags, pay in read_pcap(path):
        up = src.startswith(CLIENT_NET)
        cport, sport = (sp, dp) if up else (dp, sp)
        key = ("tcp" if proto == 6 else "udp", cport, sport)
        c = cs.get(key)
        if c is None:
            c = cs[key] = {"proto": key[0], "cport": cport, "sport": sport, "first": t - t0, "last": t - t0,
                           "syn": None, "fin_c": None, "fin_s": None, "rst": None, "up": 0, "down": 0,
                           "pk": 0, "down_s": defaultdict(int)}
        c["last"] = t - t0
        c["pk"] += 1
        if up:
            c["up"] += pay
        else:
            c["down"] += pay
            c["down_s"][int(t - t0)] += pay
        if proto == 6:
            if flags & 0x02 and not flags & 0x10 and c["syn"] is None:
                c["syn"] = round(t - t0, 3)
            if flags & 0x01:
                k = "fin_c" if up else "fin_s"
                if c[k] is None:
                    c[k] = round(t - t0, 3)
            if flags & 0x04 and c["rst"] is None:
                c["rst"] = round(t - t0, 3)
    return cs


ACC = re.compile(r"^(\S+ \S+) from (?:(?:tcp|udp):)?([\d.]+):(\d+) accepted (tcp|udp):([\d.]+):(\d+)")


def access(path, t0):
    import datetime
    out = []
    for line in open(path, errors="replace"):
        m = ACC.match(line)
        if not m:
            continue
        ts = datetime.datetime.strptime(m.group(1), "%Y/%m/%d %H:%M:%S.%f").timestamp()
        out.append({"t": round(ts - t0, 3), "cport": int(m.group(3)), "net": m.group(4),
                    "dport": int(m.group(6)), "kind": KIND.get(int(m.group(6)), "?")})
    return out


def pct(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    return round(xs[min(len(xs) - 1, int(p / 100 * len(xs)))], 1)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("raw")
    p.add_argument("variant")
    p.add_argument("--json")
    a = p.parse_args()
    fl = json.load(open("%s/%s-flows.json" % (a.raw, a.variant)))
    t0 = fl["t0"]
    cs = conns_from_pcap("%s/%s.pcap" % (a.raw, a.variant), t0)
    acc = access("%s/%s-access.log" % (a.raw, a.variant), t0)
    # proxied flows by kind, in start order: an access entry takes the
    # earliest flow of its kind not taken yet whose start is before it
    flows = []
    for f in fl["flows"]:
        if f["kind"] == "web":
            for i, _ in enumerate(f["conns"] or [None] * 6):
                flows.append({"name": "%s.%d" % (f["name"], i), "kind": "web", "start": f["start"]})
        else:
            flows.append({"name": f["name"], "kind": f["kind"], "start": f["start"]})
    flows.sort(key=lambda x: x["start"] or 0)
    taken = set()
    for e in sorted(acc, key=lambda x: x["t"]):
        for i, f in enumerate(flows):
            if i not in taken and f["kind"] == e["kind"] and (f["start"] or 0) <= e["t"] + 0.05:
                taken.add(i)
                f["acc_t"], f["cport"], f["net"] = e["t"], e["cport"], e["net"]
                break
    # the conns, with their flows
    rows = []
    for key, c in sorted(cs.items(), key=lambda kv: kv[1]["first"]):
        if c["pk"] < 2:
            continue
        on = [f["name"] for f in flows if f.get("cport") == c["cport"]]
        if c["proto"] == "tcp":
            end = min([x for x in (c["fin_c"], c["fin_s"], c["rst"]) if x is not None], default=None)
            closer = None
            if end is not None:
                closer = "rst" if end == c["rst"] else ("client" if end == c["fin_c"] else "server")
        else:
            end, closer = None, None
        rows.append({"proto": c["proto"], "cport": c["cport"], "sport": c["sport"],
                     "open": round(c["syn"] if c["syn"] is not None else c["first"], 2),
                     "close": end, "closer": closer, "last_pkt": round(c["last"], 2),
                     "life_s": round((end if end is not None else c["last"]) - (c["syn"] or c["first"]), 1),
                     "up_kB": round(c["up"] / 1e3, 1), "down_kB": round(c["down"] / 1e3, 1),
                     "flows": on})
    # sharing: which flow pairs ride one conn
    by = defaultdict(list)
    for f in flows:
        if "cport" in f:
            by[f["cport"]].append(f["name"])
    names = {f["name"]: f.get("cport") for f in flows}
    def same(x, y):
        return names.get(x) is not None and names.get(x) == names.get(y)
    pairs = {"G1+D1": same("G1", "D1"), "U1+D1": same("U1", "D1"), "G1+U1": same("G1", "U1"),
             "G2+D2": same("G2", "D2"), "D1+D2": same("D1", "D2"), "W2.0+D1": same("W2.0", "D1"),
             "W1.0+G1": same("W1.0", "G1")}
    # game RTTs: idle (before the first download), during D1
    d1 = next((f for f in fl["flows"] if f["name"] == "D1"), None)
    lat = {}
    for f in fl["flows"]:
        if f["kind"] in ("game", "udpgame") and f.get("samples"):
            s = f["samples"]
            idle = [x[1] for x in s if x[0] < 3]
            dur = [x[1] for x in s if d1 and d1["start"] + 1 <= x[0] < d1["end"] - 0.5]
            lat[f["name"]] = {"idle_p50": pct(idle, 50), "idle_p99": pct(idle, 99),
                              "dl_p50": pct(dur, 50), "dl_p99": pct(dur, 99), "dl_max": pct(dur, 100),
                              "sent": f.get("sent"), "lost": f.get("lost")}
    dls = {f["name"]: {"MB": round(f["bytes"] / 1e6, 2), "ttfb_ms": f["ttfb_ms"],
                       "mbit": round(f["bytes"] * 8 / 1e6 / max(0.1, (f["end"] or 0) - (f["start"] or 0)), 1)}
           for f in fl["flows"] if f["kind"] == "dl"}
    web = {}
    for f in fl["flows"]:
        if f["kind"] == "web":
            tt = [c["ttfb_ms"] for c in f["conns"] if c["ttfb_ms"] is not None]
            web[f["name"]] = {"ttfb_ms_med": pct(tt, 50), "ttfb_ms_max": pct(tt, 100),
                              "errs": sum(1 for c in f["conns"] if c["err"])}
    tcp = [r for r in rows if r["proto"] == "tcp"]
    udp = [r for r in rows if r["proto"] == "udp"]
    summ = {"variant": a.variant, "tcp_conns": len(tcp), "udp_tuples": len(udp),
            "proxied_flows": len(flows), "mapped": len(taken), "access_entries": len(acc),
            "max_flows_on_one_conn": max([len(v) for v in by.values()] or [0]),
            "pairs": pairs, "lat": lat, "dl": dls, "web": web, "conns": rows}
    if a.json:
        with open(a.json, "w") as fh:
            json.dump(summ, fh, indent=1)
    print("== %s: TCP %d, UDP %d, flows %d (mapped %d), most flows on one conn %d" % (
        a.variant, len(tcp), len(udp), len(flows), len(taken), summ["max_flows_on_one_conn"]))
    print("   shares: " + ", ".join("%s=%s" % (k, "yes" if v else "no") for k, v in pairs.items()))
    for r in rows:
        print("   %s c%-5d open %6.2f close %-7s by %-6s life %6.1fs up %8.1fk down %9.1fk  %s" % (
            r["proto"], r["cport"], r["open"], r["close"], r["closer"], r["life_s"], r["up_kB"], r["down_kB"],
            ",".join(r["flows"])))
    for k, v in lat.items():
        print("   %s rtt idle p50 %s p99 %s | during D1 p50 %s p99 %s max %s (sent %s lost %s)" % (
            k, v["idle_p50"], v["idle_p99"], v["dl_p50"], v["dl_p99"], v["dl_max"], v["sent"], v["lost"]))
    for k, v in dls.items():
        print("   %s %s MB %s Mbit/s ttfb %s ms" % (k, v["MB"], v["mbit"], v["ttfb_ms"]))
    for k, v in web.items():
        print("   %s ttfb med %s max %s errs %s" % (k, v["ttfb_ms_med"], v["ttfb_ms_max"], v["errs"]))


if __name__ == "__main__":
    main()
