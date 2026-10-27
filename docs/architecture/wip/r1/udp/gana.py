#!/usr/bin/env python3
# gana.py: the analysis of the UDP game runs (game.sh), r1-udp, R1 round 3.
#
#   gana.py run RAW ID LABEL N LOSS REP MIX T0 WARM DUR ALIVE   one JSON line for a run
#   gana.py table RES.jsonl                                      the table (median of reps)
#
# Per direction (up: client -> server, 64 Hz; dn: server -> client, 30 Hz),
# over the datagrams SENT in the window [first + WARM, first + WARM + DUR - 1]
# (first: the client's first send; both ends share the clock):
#   exp     datagrams sent in the window (paced on schedule: seq i at first_dir + i/Hz)
#   lost    exp - distinct received
#   d       one-way delay = arrival - sent (ms); floor = the smallest d of the run
#   p50 p99 p999 max   of d
#   late    received with d > floor + 100 ms (a game shows it as a stall)
#   jit     RFC 3550 interarrival jitter, final value (ms)
#   ipdv    mean |d_i - d_(i-1)| over arrival order (ms)
#   reord   arrivals with a lower seq than one before
import json
import sys

HZ = {"up": 64, "dn": 30}


def pct(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    k = min(len(xs) - 1, max(0, int(round(p / 100.0 * (len(xs) - 1)))))
    return xs[k]


def direction(recv, first_dir, hz, a, b):
    lo = max(0, int((a - first_dir) * hz / 1e9) + 1)
    hi = int((b - first_dir) * hz / 1e9)
    exp = max(0, hi - lo + 1)
    seen = {}
    arr = []
    for seq, sent, at in recv:
        if lo <= seq <= hi and seq not in seen:
            seen[seq] = (at - sent) / 1e6
            arr.append((at, seq, (at - sent) / 1e6))
    allD = [(at - sent) / 1e6 for seq, sent, at in recv]
    floor = min(allD) if allD else None
    ds = list(seen.values())
    arr.sort()
    jit, ipdv, prev, reord, maxseq = 0.0, [], None, 0, -1
    for at, seq, d in arr:
        if prev is not None:
            dd = abs(d - prev)
            jit += (dd - jit) / 16.0
            ipdv.append(dd)
        prev = d
        if seq < maxseq:
            reord += 1
        maxseq = max(maxseq, seq)
    r = lambda v: None if v is None else round(v, 1)
    return {
        "exp": exp, "rx": len(ds), "lost": exp - len(ds),
        "lost_pct": round(100.0 * (exp - len(ds)) / exp, 2) if exp else None,
        "floor": r(floor), "p50": r(pct(ds, 50)), "p99": r(pct(ds, 99)), "p999": r(pct(ds, 99.9)),
        "max": r(max(ds) if ds else None),
        "late": sum(1 for d in ds if floor is not None and d > floor + 100),
        "late_pct": round(100.0 * sum(1 for d in ds if floor is not None and d > floor + 100) / exp, 2) if exp else None,
        "jit": r(jit), "ipdv": r(sum(ipdv) / len(ipdv) if ipdv else None), "reord": reord,
    }


def run(argv):
    raw, rid, label, n, loss, rep, mix, t0, warm, dur, alive = argv
    warm, dur = float(warm), float(dur)
    errs = []
    try:
        cl = json.load(open(raw + "/game-client.json"))
        sv = json.load(open(raw + "/game-server.json"))
    except Exception as e:  # noqa: BLE001
        print(json.dumps({"id": rid, "label": label, "n": int(n), "loss": float(loss), "rep": int(rep), "mix": mix,
                          "ok": False, "errs": ["game files: %s" % e]}))
        return
    cf = cl["flows"].get("1", {})
    sf = sv["flows"].get("1", {})
    first = cf.get("first", 0)
    a, b = first + warm * 1e9, first + (warm + dur - 1) * 1e9
    up = direction(sf.get("recv") or [], first, HZ["up"], a, b)
    dn = direction(cf.get("recv") or [], sf.get("first", first), HZ["dn"], a, b)
    bulk = probe = None
    try:
        c = json.load(open(raw + "/client.json"))
        bulk = c.get("dn_sum_mbit", {}).get("main")
        pa = c.get("probe_all", {}).get("main", {})
        probe = {"p50": pa.get("p50"), "p99": pa.get("p99"), "lost": pa.get("lost")}
    except Exception as e:  # noqa: BLE001
        errs.append("rigload: %s" % e)
    if up["exp"] == 0 or dn["exp"] == 0:
        errs.append("no game datagrams in the window")
    print(json.dumps({"id": rid, "label": label, "n": int(n), "loss": float(loss), "rep": int(rep), "mix": mix,
                      "ok": not errs and alive == "1", "edge_alive": alive == "1", "errs": errs,
                      "up": up, "dn": dn, "bulk_mbit": bulk, "probe": probe}))


def med(xs):
    xs = sorted(x for x in xs if x is not None)
    if not xs:
        return None
    m = len(xs) // 2
    return xs[m] if len(xs) % 2 else round((xs[m - 1] + xs[m]) / 2, 2)


def table(argv):
    last = {}
    for l in open(argv[0]):
        if l.strip():
            r = json.loads(l)
            last[r["id"]] = r  # a run done again replaces the earlier line
    rows = [r for r in last.values() if r.get("ok")]
    cells = {}
    for r in rows:
        cells.setdefault((r["mix"], r["loss"], r["n"], r["label"]), []).append(r)
    print("| mix | loss % | N | binary | reps | dir | p50 | p99 | p99.9 | max | floor | late % | lost % | jitter | bulk Mbit/s | probe p99 |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for k in sorted(cells):
        rs = cells[k]
        for d in ("up", "dn"):
            g = lambda f: med([r[d][f] for r in rs])
            print("| %s | %s | %d | %s | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |" % (
                k[0], k[1], k[2], k[3], len(rs), d, g("p50"), g("p99"), g("p999"), g("max"), g("floor"),
                g("late_pct"), g("lost_pct"), g("jit"),
                med([r["bulk_mbit"] for r in rs]) if d == "up" else "",
                med([(r["probe"] or {}).get("p99") for r in rs]) if d == "up" else ""))


if __name__ == "__main__":
    {"run": run, "table": table}[sys.argv[1]](sys.argv[2:])
