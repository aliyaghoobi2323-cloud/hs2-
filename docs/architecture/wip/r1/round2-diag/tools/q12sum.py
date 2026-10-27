#!/usr/bin/env python3
"""q12sum.py OUT PHASE...: Q1/Q2 tables of round 2, step 1 (lab only).

For each cell (rate, mode) and configuration: the probe's p99, floor and
p99 - floor (ana.py's, the main window), and from decomp.json (probes sent
in the main window, s 5..25) the mean of each part of a probe's round trip
over all probes and over the slowest 5% and 1%, grouped as

  host   user + sched + write + sock, both directions (hs2 and the sender's
         socket: C2, the shaper, C3, unsent bytes, pacing)
  prop   2 x RTT/2 + serialization (the floor's own part)
  queue  the bottleneck's queue, both directions (path - RTT/2 - ser, for
         segments sent once)
  loss   head-of-line behind a hole, plus the retransmission's delay when
         the probe's own segment was lost
  recv   the receiving hs2 (wakeup, TLS, smux)

and the TCP side: retransmissions (% of data segments, each end), CoDel or
tail drops at the bottleneck, srtt - min_rtt of the link socket at the
probe's write (median and p90, each end), the bottleneck's backlog as delay
(tcs.txt, p50/p99, each direction), throughput (down, up)."""
import glob, gzip, json, os, re, statistics, sys

sys.path.insert(0, "/root/hs2-wt/r1-diag/docs/architecture/wip/rig-scale")
import ana  # noqa

def med(v):
    v = [x for x in v if x is not None]
    return statistics.median(v) if v else None

def rng(v):
    v = [x for x in v if x is not None]
    return (min(v), max(v)) if v else (None, None)

def pct(v, q):
    v = sorted(x for x in v if x is not None)
    if not v:
        return None
    return v[min(len(v) - 1, int(round(q * (len(v) - 1))))]

def tcs(path, rate):
    """leaf backlog as delay (ms) per sample, each direction; drops"""
    out = {"D": [], "U": []}
    try:
        f = open(path, errors="replace")
    except OSError:
        return None
    cur, leaf = None, False
    for line in f:
        if line.startswith("T "):
            continue
        d, rest = line[:1], line[2:]
        if rest.startswith("qdisc"):
            leaf = " 3: " in rest
            continue
        if leaf and "backlog" in rest:
            m = re.search(r"backlog (\d+)([KMG]?)b", rest)
            if m:
                b = int(m.group(1)) * {"": 1, "K": 1000, "M": 1e6, "G": 1e9}[m.group(2)]
                out[d].append(b * 8 / (rate * 1e6) * 1e3)
    return {k: (pct(v, .5), pct(v, .99), max(v) if v else None) for k, v in out.items()}

def runs(out, phase):
    for line in open(os.path.join(out, phase, "res.jsonl")):
        r = json.loads(line)
        if not r.get("ok"):
            continue
        rd = os.path.join(out, phase, "raw", r["id"])
        yield r, rd

def decomp_groups(rd, rtt, rate):
    try:
        d = json.load(open(os.path.join(rd, "decomp.json")))
    except (OSError, ValueError):
        return None
    pr = [p for p in d["probes"] if p.get("ok") and 25 <= p["seq"] < 125]
    if not pr:
        return None
    ser = 1514 * 8 / (rate * 1e6) * 1e3
    rows = []
    for p in pr:
        up, dn = p["up"], p["dn"]
        host = up["user"] + dn["user"] + sum(x[k] for x in (up, dn) for k in ("sched", "write", "sock"))
        queue = sum(x["queue"] for x in (up, dn) if x["ntx"] <= 1)
        loss = sum(x["hol"] + (x["path"] - rtt / 2 - ser if x["ntx"] > 1 else 0) for x in (up, dn))
        recv = up["recv"] + dn["recv"] + dn["last"]
        prop = rtt + 2 * ser
        rows.append({"rtt": p["rtt"], "host": host, "queue": queue, "loss": loss, "recv": recv, "prop": prop,
                     "c2": up["sched"] + dn["sched"], "sock": up["sock"] + dn["sock"], "write": up["write"] + dn["write"],
                     "hol_any": (up["hol"] > 5 or dn["hol"] > 5), "own": (up["ntx"] > 1 or dn["ntx"] > 1),
                     "prt": ((up["cwnd"] or 99) <= 4 or (dn["cwnd"] or 99) <= 4),
                     "srtt_up": up["srtt"] - up["minrtt"], "srtt_dn": dn["srtt"] - dn["minrtt"],
                     "notsent_up": up["notsent"], "notsent_dn": dn["notsent"]})
    rows.sort(key=lambda x: x["rtt"])
    res = {"n": len(rows)}
    for name, frac in (("all", 1.0), ("slow5", .05), ("slow1", .01)):
        k = max(1, int(round(frac * len(rows))))
        sl = rows[-k:]
        res[name] = {f: sum(x[f] for x in sl) / k for f in ("rtt", "host", "queue", "loss", "recv", "prop", "c2", "sock", "write")}
        res[name]["hol_any"] = sum(1 for x in sl if x["hol_any"]) / k
        res[name]["own"] = sum(1 for x in sl if x["own"]) / k
        res[name]["prt"] = sum(1 for x in sl if x["prt"]) / k
    res["srtt_q"] = {"up": (pct([x["srtt_up"] for x in rows], .5), pct([x["srtt_up"] for x in rows], .9)),
                     "dn": (pct([x["srtt_dn"] for x in rows], .5), pct([x["srtt_dn"] for x in rows], .9))}
    res["notsent"] = {"up": pct([x["notsent_up"] for x in rows], .5), "dn": pct([x["notsent_dn"] for x in rows], .5)}
    return res

def main():
    out = sys.argv[1]
    phases = sys.argv[2:]
    cells = {}
    for ph in phases:
        for r, rd in runs(out, ph):
            m = ana.derive(r)
            rate, rtt = float(r["RATE"]), float(r["RTT"])
            key = (int(rate), r["MODE"])
            dg = decomp_groups(rd, rtt, rate)
            t = tcs(os.path.join(rd, "tcs.txt"), rate)
            bn = r.get("bottleneck", {})
            drops = {d: sum(q.get("dropped", 0) for q in bn.get(d, []) if q.get("handle") == "3:") for d in ("dn", "up")}
            cells.setdefault(key, {}).setdefault(r["bin"], []).append({"m": m, "dg": dg, "tcs": t, "drops": drops, "id": r["id"], "N": r["N"]})
    order = ["base", "a", "ap5", "ap25", "ap100", "ac5", "ac25", "ac100", "acl5", "acl25", "acl100", "acw5", "acw25", "acw100", "acub", "apin", "apinp5", "apinp25", "apinp100"]
    summary = {}
    for key in sorted(cells):
        print("\n## %s Mbit/s, %s" % key)
        print("| config | n | down | up | p99 | floor | p99-floor | bound | rt exit/edge % | drops dn/up | host | queue | loss+HOL | recv | prop (slowest 1%%: rtt) | slow5: host/queue/loss | HOL>5ms % (all/slow5) | cwnd<=4 % (all/slow5) | srtt-min dn/up p50 | backlog dn/up p50/p99 ms |")
        print("|" + "---|" * 20)
        for b in sorted(cells[key], key=lambda x: order.index(x) if x in order else 99):
            rs = cells[key][b]
            M = lambda k: med([x["m"].get(k) for x in rs])
            dgs = [x["dg"] for x in rs if x["dg"]]
            def DG(w, f):
                return med([d[w][f] for d in dgs])
            row = {"n": len(rs), "dn": M("dn"), "up": M("up"), "p99": M("p99"), "floor": M("rtt_floor"),
                   "p99_floor": med([x["m"].get("p99", 0) - x["m"].get("rtt_floor", 0) for x in rs if "p99" in x["m"]]),
                   "p99_floor_rng": rng([x["m"].get("p99", 0) - x["m"].get("rtt_floor", 0) for x in rs if "p99" in x["m"]]),
                   "bound": M("d_bound"), "rt_exit": M("rt_exit"), "rt_edge": M("rt_edge"),
                   "drops_dn": med([x["drops"]["dn"] for x in rs]), "drops_up": med([x["drops"]["up"] for x in rs])}
            for w in ("all", "slow5", "slow1"):
                for f in ("rtt", "host", "queue", "loss", "recv", "prop", "c2", "sock", "write", "hol_any", "own", "prt"):
                    row[w + "." + f] = DG(w, f) if dgs else None
            row["srtt_dn"] = med([d["srtt_q"]["dn"][0] for d in dgs])
            row["srtt_up"] = med([d["srtt_q"]["up"][0] for d in dgs])
            ts = [x["tcs"] for x in rs if x["tcs"]]
            row["bl_dn"] = (med([t["D"][0] for t in ts]), med([t["D"][1] for t in ts])) if ts else (None, None)
            row["bl_up"] = (med([t["U"][0] for t in ts]), med([t["U"][1] for t in ts])) if ts else (None, None)
            summary["%d/%s/%s" % (key[0], key[1], b)] = row
            f = lambda v, p=0: "-" if v is None else ("%.*f" % (p, v))
            print("| %s | %d | %s | %s | %s | %s | +%s [%s..%s] | %s | %s/%s | %s/%s | %s | %s | %s | %s | %s (%s) | %s/%s/%s | %s/%s | %s/%s | %s/%s | %s/%s, %s/%s |" % (
                b, row["n"], f(row["dn"], 2), f(row["up"], 2), f(row["p99"]), f(row["floor"]), f(row["p99_floor"]),
                f(row["p99_floor_rng"][0]), f(row["p99_floor_rng"][1]), f(row["bound"]),
                f(row["rt_exit"], 2), f(row["rt_edge"], 2), f(row["drops_dn"]), f(row["drops_up"]),
                f(row["slow1.host"]), f(row["slow1.queue"]), f(row["slow1.loss"]), f(row["slow1.recv"]), f(row["slow1.prop"]), f(row["slow1.rtt"]),
                f(row["slow5.host"]), f(row["slow5.queue"]), f(row["slow5.loss"]),
                f(row["all.hol_any"] and 100 * row["all.hol_any"]), f(row["slow5.hol_any"] and 100 * row["slow5.hol_any"]),
                f(row["all.prt"] and 100 * row["all.prt"]), f(row["slow5.prt"] and 100 * row["slow5.prt"]),
                f(row["srtt_dn"]), f(row["srtt_up"]),
                f(row["bl_dn"][0]), f(row["bl_dn"][1]), f(row["bl_up"][0]), f(row["bl_up"][1])))
    json.dump(summary, open(os.path.join(out, "q12sum.json"), "w"), indent=1)

if __name__ == "__main__":
    main()
