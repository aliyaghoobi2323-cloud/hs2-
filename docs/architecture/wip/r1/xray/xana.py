#!/usr/bin/env python3
"""xana.py: step 2 of R1 round 3 (xray), the rig runs of xrun.sh.

  xana.py compose RAWDIR ID CELL BIN_SHA BIN_ENV   one JSON line for one run
  xana.py table RES.jsonl [--md]                    median over the repeats

Game samples (xtraffic.py): [t_send, rtt, up one-way, down one-way] in ms,
one clock. Windows by send time: idle = [1, WARM), dl = [WARM + 1, WARM +
DUR) (the download runs from WARM to WARM + DUR). A frame with no answer
within 2 s of the end is lost; lost frames count in the dl window when sent
in it.
"""
import json, re, statistics, sys


def pct(xs, p):
    if not xs:
        return None
    xs = sorted(xs)
    return round(xs[min(len(xs) - 1, int(p / 100.0 * len(xs)))], 1)


def game_stats(f, warm, dur):
    s = f.get("samples") or []
    idle = [x for x in s if 1 <= x[0] < warm]
    dl = [x for x in s if warm + 1 <= x[0] < warm + dur]
    lost_t = f.get("lost_t") or []
    lost_dl = sum(1 for t in lost_t if warm + 1 <= t < warm + dur)
    return {"idle_p50": pct([x[1] for x in idle], 50), "idle_p99": pct([x[1] for x in idle], 99),
            "dl_p50": pct([x[1] for x in dl], 50), "dl_p90": pct([x[1] for x in dl], 90),
            "dl_p99": pct([x[1] for x in dl], 99), "dl_max": pct([x[1] for x in dl], 100),
            "dl_up_p99": pct([x[2] for x in dl], 99), "dl_down_p99": pct([x[3] for x in dl], 99),
            "dl_n": len(dl), "lost_dl": lost_dl, "sent": f.get("sent"), "lost": f.get("lost"),
            "err": f.get("err")}


def status_lines(path):
    try:
        txt = open(path, errors="replace").read()
    except OSError:
        return {}
    out = {}
    m = re.search(r"place: .*?opened on quiet (\d+), on bulk \(no quiet up\) (\d+); moved to bulk (\d+) \(not yet (\d+)", txt)
    if m:
        out["opened_quiet"], out["opened_bulk"], out["moved"], out["not_yet"] = map(int, m.groups())
    m = re.search(r"classes: light (\d+) / bulk (\d+) / unknown (\d+)", txt)
    if m:
        out["cls_light"], out["cls_bulk"], out["cls_unknown"] = map(int, m.groups())
    out["lines"] = [l.strip() for l in txt.splitlines() if re.search(r"place:|classes:|groups:", l)][:6]
    return out


def compose(raw, rid, cell, sha, benv):
    fl = json.load(open(raw + "/flows.json"))
    warm, dur = fl["warm"], fl["dur"]
    row = {"id": rid, "bin_sha": sha, "bin_env": benv, "warm": warm, "dur": dur}
    for tok in cell.split():
        k, v = tok.split("=", 1)
        row[k] = v
    for f in fl["flows"]:
        if f["kind"] == "game":
            row["game"] = game_stats(f, warm, dur)
        elif f["kind"] == "udpgame":
            row["udpgame"] = game_stats(f, warm, dur)
        elif f["kind"] == "dl":
            bins = dict((int(k), v) for k, v in f.get("bins") or [])
            w = [bins.get(i, 0) for i in range(int((warm + 1) * 10), int((warm + dur) * 10))]
            row["dl_mbit"] = round(sum(w) * 8 / 1e6 / (len(w) / 10.0), 2) if w else None
            row["dl_ttfb_ms"] = f.get("ttfb_ms")
            row["dl_err"] = f.get("err")
    try:
        # ss -i puts each socket's info on an indented second line
        row["user_conns_mid"] = sum(1 for l in open(raw + "/ss-user-mid.txt") if l.strip() and not l[0].isspace())
    except OSError:
        row["user_conns_mid"] = None
    # which user connection (the exit's "from" port at the xray server)
    # carried the TCP game, the UDP game and the download
    port = {}
    try:
        for l in open(raw + "/xaccess.log", errors="replace"):
            m = re.search(r" from (?:tcp:|udp:)?[\d.]+:(\d+) accepted (?:tcp:|udp:)?[\d.]+:(700[1-4])", l)
            if m:
                port.setdefault({"7001": "game", "7002": "dl", "7003": "web", "7004": "udpgame"}[m.group(2)], m.group(1))
    except OSError:
        pass
    row["conn_of"] = port
    row["game_dl_shared"] = bool(port.get("game")) and port.get("game") == port.get("dl")
    row["udp_dl_shared"] = bool(port.get("udpgame")) and port.get("udpgame") == port.get("dl")
    row["status_mid"] = status_lines(raw + "/status-mid.txt")
    row["status_end"] = status_lines(raw + "/status-end.txt")
    # the edge's counters run over the hs2 pair's life (several variants):
    # this run's share is mid (or end) minus pre, taken before its client
    row["status_pre"] = pre = status_lines(raw + "/status-pre.txt")
    for w in ("mid", "end"):
        sm = row["status_" + w]
        if "moved" in sm:
            row["place_" + w] = {k: sm[k] - pre.get(k, 0) for k in ("opened_quiet", "opened_bulk", "moved")}
    if "place_mid" in row:
        row["place"] = "quiet %d, moved %d" % (row["place_mid"]["opened_quiet"], row["place_mid"]["moved"])
    print(json.dumps(row))


def med(xs):
    xs = [x for x in xs if x is not None]
    return round(statistics.median(xs), 1) if xs else None


def table(path, md=False):
    rows = [json.loads(l) for l in open(path) if l.strip()]
    cells = {}
    for r in rows:
        key = (r["VARIANT"], r["N"], r["LOSS"], r["BIN"])
        cells.setdefault(key, []).append(r)
    order = {"xa": 0, "xb1": 1, "xb2": 2, "vv": 3, "gr": 4, "grm": 5}
    hdr = ["variant", "N", "loss", "bin", "reps", "idle p50", "game p50", "game p99", "game max", "lost",
           "udp p99", "up p99", "down p99", "dl Mbit/s", "user conns", "game+dl one conn", "quiet/moved"]
    out = []
    for key in sorted(cells, key=lambda k: (order.get(k[0], 9), int(k[1]), float(k[2]), k[3] != "old")):
        rs = cells[key]
        g = [r.get("game", {}) for r in rs]
        u = [r.get("udpgame", {}) for r in rs]
        mv = ";".join(sorted(set(r.get("place", "-") for r in rs)))
        out.append([key[0], key[1], key[2], key[3], len(rs), med([x.get("idle_p50") for x in g]),
                    med([x.get("dl_p50") for x in g]), med([x.get("dl_p99") for x in g]),
                    med([x.get("dl_max") for x in g]), sum((x.get("lost_dl") or 0) for x in g),
                    med([x.get("dl_p99") for x in u]), med([x.get("dl_up_p99") for x in g]),
                    med([x.get("dl_down_p99") for x in g]), med([r.get("dl_mbit") for r in rs]),
                    med([r.get("user_conns_mid") for r in rs]),
                    "%d/%d" % (sum(1 for r in rs if r.get("game_dl_shared")), len(rs)), mv])
    if md:
        print("| " + " | ".join(hdr) + " |")
        print("|" + "---|" * len(hdr))
        for o in out:
            print("| " + " | ".join(str(x) for x in o) + " |")
    else:
        print("\t".join(hdr))
        for o in out:
            print("\t".join(str(x) for x in o))


def recount(path):
    """Recount user_conns_mid of every row of RES from its raw dir (rows
    made before the count read ss's info lines as sockets)."""
    import os
    base = os.path.join(os.path.dirname(os.path.abspath(path)), "raw")
    rows = [json.loads(l) for l in open(path) if l.strip()]
    for r in rows:
        try:
            r["user_conns_mid"] = sum(1 for l in open(os.path.join(base, r["id"], "ss-user-mid.txt"))
                                      if l.strip() and not l[0].isspace())
        except OSError:
            pass
    with open(path, "w") as fh:
        for r in rows:
            fh.write(json.dumps(r) + "\n")


if __name__ == "__main__":
    if sys.argv[1] == "compose":
        compose(*sys.argv[2:7])
    elif sys.argv[1] == "recount":
        recount(sys.argv[2])
    elif sys.argv[1] == "table":
        table(sys.argv[2], "--md" in sys.argv)
