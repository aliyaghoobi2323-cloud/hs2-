#!/usr/bin/env python3
"""ana.py: the big rig's analysis (R1 task 2), and the helpers run.sh uses, so
the mixes, their windows and the verdicts are defined in one place.

  ana.py OUT/res.jsonl [--json]     table: median [min..max] of the repeats of
                                    each cell and binary, P10 verdicts (a)-(e),
                                    the probe and TUN ping p99 bounds, and new/old
  ana.py schedule PLAN REPS         run.sh: the runs, ABBABA per cell (TSV)
  ana.py spec "CELL" SPEC.json      run.sh: rigload's spec; prints "NUSERS TOTAL_S"
  ana.py compose RAWDIR OK WHY      run.sh: one res.jsonl line from a run's raw files

Definitions (REDESIGN.fa.md section 5.1 and 6):
  capacity  the bottleneck's TCP payload rate: rate x 1448/1514 (MSS 1448 with
            timestamps; htb counts the 1514 B Ethernet frame). rate = RATE, or
            min(RATE, AR) with agg:AR in MODE; with police:PR in MODE the
            capacity is also at most N x PR (MODE=police:PR+agg:AR: both).
  RTT_floor p1 of the probe over the whole measured span of the same run (for
            the TUN ping: p1 of the TUN ping itself).
  (a) the lone user (16 connections) >= 0.95 x capacity
  (b) every bulk user >= 0.9 x its floor; floor = max-min fair share of the
      capacity (water-filling): min(demand, capacity / active users), the light
      users' (measured) demand first. In one_vs_16 this is the 1-connection user
      next to the 16-connection user (floor = (capacity - light) / 2).
  (c) total bulk >= 0.97 x capacity, in each direction that carries bulk
  (d) light (probe) p99 <= RTT_floor + max(20 ms, 3 x 1500 B / rate)
  (e) newheavy: after the heavy user joins, the probe p99 (from the join to the
      end) stays within (d), and the existing bulk user (from SETTLE s after the
      join) >= 0.9 x its floor (now shared with the heavy user)
  tun TUN ping p99 <= RTT_floor(ping) + max(10 ms, 3 x 1500 B / rate)
  thr new/old total >= 0.97 (when a cell has both binaries)
Verdicts are taken on the median of the repeats; "k/n" says in how many runs
the criterion held on its own.
"""
import hashlib
import json
import math
import os
import re
import statistics
import sys

EFF = 1448 / 1514
REQ = ["N", "RATE", "RTT", "LOSS", "MODE", "USERS", "MIX", "BIN"]
MIXES = ["s5", "lone16", "one_vs_16", "newheavy", "tunping"]
ORDER = ["N", "RATE", "RTT", "LOSS", "MODE", "USERS", "MIX"]


def die(msg):
    sys.stderr.write("ana.py: %s\n" % msg)
    sys.exit(1)


def parse_cell(s):
    d = {}
    for tok in s.split():
        if "=" not in tok:
            die("token without '=': %r" % tok)
        k, v = tok.split("=", 1)
        if k in d:
            die("key %s twice in %r" % (k, s))
        d[k] = v
    return d


def cell_str(d):
    keys = [k for k in ORDER if k in d] + sorted(k for k in d if k not in ORDER and k != "BIN")
    return " ".join("%s=%s" % (k, d[k]) for k in keys)


def num(d, k, default=None):
    v = d.get(k)
    if v is None or v == "":
        return default
    return float(v) if ("." in v or "e" in v) else int(v)


def parse_mode(m):
    """MODE = [police:PR+]LEAF or LEAF[+police:PR]; LEAF = fqcodel | pfifo:L | agg:AR.
    Returns (PR or None, LEAF), or None when malformed."""
    pol, leaf = None, None
    for p in m.split("+"):
        if re.match(r"^police:\d+(\.\d+)?$", p) and pol is None:
            pol = float(p[7:])
        elif re.match(r"^(fqcodel|pfifo:\d+|agg:\d+(\.\d+)?)$", p) and leaf is None:
            leaf = p
        else:
            return None
    return pol, leaf or "fqcodel"


# ------------------------------------------------------------------ schedule

def schedule(plan, reps):
    reps = int(reps)
    cells, order = {}, []
    for ln, line in enumerate(open(plan), 1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        d = parse_cell(line)
        miss = [k for k in REQ if k not in d]
        if miss:
            die("%s:%d: missing %s" % (plan, ln, " ".join(miss)))
        if d["MIX"] not in MIXES:
            die("%s:%d: MIX %s (one of %s)" % (plan, ln, d["MIX"], " ".join(MIXES)))
        if parse_mode(d["MODE"]) is None:
            die("%s:%d: MODE %s (fqcodel, pfifo:L, police:PR, agg:AR, police:PR+LEAF)" % (plan, ln, d["MODE"]))
        if d["BIN"] not in ("old", "new"):
            die("%s:%d: BIN %s (old or new)" % (plan, ln, d["BIN"]))
        v6 = int(d.get("V6", "0"))
        if int(d["USERS"]) > 500 + (v6 > 0) or int(d["USERS"]) < 1 or v6 > int(d["USERS"]):
            die("%s:%d: USERS %s, V6 %d (at most 500 IPv4 users, plus the IPv6 user V6=K)" % (plan, ln, d["USERS"], v6))
        c = cell_str(d)
        if c not in cells:
            cells[c] = []
            order.append(c)
        if d["BIN"] not in cells[c]:
            cells[c].append(d["BIN"])
    for c in order:
        bins = sorted(cells[c], key=lambda b: b != "old")  # A = old when the cell has it
        h = hashlib.sha1(c.encode()).hexdigest()[:8]
        if len(bins) == 1:
            seq = bins * reps
        else:
            # ABBABA: AB, then BA for every further repeat (k > 2 binaries: rotate)
            seq = []
            for r in range(reps):
                if len(bins) == 2:
                    seq += bins if r == 0 else bins[::-1]
                else:
                    seq += bins[r % len(bins):] + bins[:r % len(bins)]
        count = {}
        for b in seq:
            count[b] = count.get(b, 0) + 1
            o = "A" if b == bins[0] else ("B" if len(bins) > 1 and b == bins[1] else "C")
            print("%s-%s-%d\t%s\t%s\t%d\t%s" % (h, b, count[b], b, o, count[b], c))


# ------------------------------------------------------------------ spec

def user_src(i):
    return "10.100.%d.%d" % (i // 250, i % 250 + 1)


def spec(cellstr, path):
    d = parse_cell(cellstr)
    U = int(d["USERS"])
    mix = d["MIX"]
    warm = num(d, "WARM", 5)
    dur = num(d, "DUR", 30)
    settle = num(d, "SETTLE", 5)
    dirn = d.get("DIR", "dn")
    if dirn not in ("dn", "up"):
        die("DIR %s" % dirn)
    conns = num(d, "CONNS", 16)
    users = [{"id": "u%d" % (i + 1), "src": user_src(i), "role": "light", "flows": []} for i in range(U)]
    probe = lambda: {"kind": "probe", "n": 1, "start_s": 0}
    ping_tun = False
    tun_flows = []
    total = warm + dur
    windows = [{"name": "main", "from": warm, "to": warm + dur}]

    def need(k):
        if U < k:
            die("MIX %s needs USERS >= %d" % (mix, k))

    if mix == "lone16":
        users[0]["role"] = "lone"
        users[0]["flows"].append({"kind": dirn, "n": conns, "start_s": 0})
        light = list(range(1, U))
    elif mix == "one_vs_16":
        need(2)
        users[0]["role"] = "one"
        users[0]["flows"].append({"kind": dirn, "n": 1, "start_s": 0})
        users[1]["role"] = "sixteen"
        users[1]["flows"].append({"kind": dirn, "n": conns, "start_s": 0})
        light = list(range(2, U))
    elif mix == "newheavy":
        need(3)
        join = warm + dur / 2
        total = join + settle + dur / 2
        windows = [{"name": "before", "from": warm, "to": join},
                   {"name": "after", "from": join, "to": total},
                   {"name": "after_settled", "from": join + settle, "to": total}]
        users[1]["role"] = "exist"
        users[1]["flows"].append({"kind": dirn, "n": num(d, "NHBULK", 1), "start_s": 0})
        users[2]["role"] = "heavy"
        users[2]["flows"].append({"kind": dirn, "n": conns, "start_s": join})
        light = [0] + list(range(3, U))
    elif mix == "s5":
        dn, up = num(d, "DN", 4), num(d, "UP", 2)
        flows = ["dn"] * dn + ["up"] * up
        nb = len(flows) if U > len(flows) else max(1, U - 1)
        for j, k in enumerate(flows):
            u = users[j % nb]
            u["role"] = "bulk"
            u["flows"].append({"kind": k, "n": 1, "start_s": 0})
        light = list(range(nb, U))
        ping_tun = True
    elif mix == "tunping":
        tun_flows = [{"kind": d.get("TUNDIR", "dn"), "n": num(d, "TDN", 4), "start_s": 0}]
        ping_tun = True
        light = list(range(U))
    # light users: one probe each; if the mix leaves none, user 1 carries the probe
    for i in light:
        users[i]["flows"].append(probe())
    if not light:
        users[0]["flows"].append(probe())
    windows.append({"name": "all", "from": warm, "to": total})
    # V6=K: user K is one IPv6 /64 (a fresh address in it for every connection);
    # the IPv4 addresses go to the others in order
    v6 = int(d.get("V6", "0"))
    n4 = 0
    for i, u in enumerate(users):
        if i + 1 == v6:
            u["v6"] = True
            u["src"] = ""
        else:
            u["src"] = user_src(n4)
            n4 += 1
    s = {"total_s": total, "bulk": "10.201.0.1:5311", "echo": "10.201.0.1:5312",
         "bulk6": "[fd00:201::1]:5311", "echo6": "[fd00:201::1]:5312", "v6_prefix": "fd77:1::",
         "tun_bulk": "10.77.0.2:5213", "tun_ping": "10.77.0.2", "ping_tun": ping_tun,
         "probe_ms": 200, "probe_bytes": 32, "ping_ms": 200,
         "users": users, "tun_flows": tun_flows, "windows": windows}
    if n4 > 500:
        die("%d IPv4 users (at most 500)" % n4)
    with open(path, "w") as f:
        json.dump(s, f, indent=1)
    print(n4, total)


# ------------------------------------------------------------------ compose

def readj(path, default=None):
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return default


def pidstat(path, pids):
    """per-second %CPU of each pid (LC_ALL=C pidstat -u -p a,b 1 n)"""
    out = {}
    try:
        lines = open(path).read().splitlines()
    except OSError:
        return {}
    for l in lines:
        t = l.split()
        if len(t) < 9 or t[0].startswith("Average") or "%CPU" in t:
            continue
        try:
            pid, cpu = int(t[-8]), float(t[-3])
        except ValueError:
            continue
        out.setdefault(pid, []).append(cpu)
    res = {}
    for name, pid in pids.items():
        v = sorted(out.get(pid, []))
        if v:
            res[name] = {"mean": round(statistics.mean(v), 1),
                         "p95": v[min(len(v) - 1, int(math.ceil(0.95 * len(v))) - 1)], "max": v[-1], "n": len(v)}
    return res


def host_busy(r):
    try:
        a = [int(x) for x in open(r + "/stat-start.txt").read().split()[1:]]
        b = [int(x) for x in open(r + "/stat-end.txt").read().split()[1:]]
    except (OSError, ValueError):
        return None
    d = [y - x for x, y in zip(a, b)]
    tot = sum(d[:8])
    return round(100 * (1 - (d[3] + d[4]) / tot), 1) if tot > 0 else None


def ss_parse(path):
    socks = {}
    key = None
    try:
        lines = open(path).read().splitlines()
    except OSError:
        return socks
    for l in lines:
        if not l.strip():
            continue
        if not l[0].isspace():
            t = l.split()
            key = " ".join(t[2:4]) if len(t) >= 4 else l
            socks[key] = {}
            continue
        if key is None:
            continue
        s = socks[key]
        for k in ("data_segs_out", "bytes_sent", "bytes_retrans", "segs_out"):
            m = re.search(r"\b%s:(\d+)" % k, l)
            if m:
                s[k] = int(m.group(1))
        m = re.search(r"\bretrans:(\d+)/(\d+)", l)
        if m:
            s["retrans"] = int(m.group(2))
    return socks


def retrans(r, side):
    a, b = ss_parse("%s/ss-%s-start.txt" % (r, side)), ss_parse("%s/ss-%s-end.txt" % (r, side))
    tot = {"data_segs_out": 0, "retrans": 0, "bytes_sent": 0, "bytes_retrans": 0}
    for k, v in b.items():
        u = a.get(k, {})
        for f in tot:
            tot[f] += max(0, v.get(f, 0) - u.get(f, 0))
    tot["pct"] = round(100 * tot["retrans"] / tot["data_segs_out"], 3) if tot["data_segs_out"] else 0
    tot["sockets_end"] = len(b)
    return tot


def tc_parse(path):
    out = []
    try:
        txt = open(path).read()
    except OSError:
        return out
    for m in re.finditer(r"qdisc (\S+) (\S+) .*?\n\s*Sent (\d+) bytes (\d+) pkt \(dropped (\d+), overlimits (\d+)", txt):
        out.append({"qdisc": m.group(1), "handle": m.group(2), "pkts": int(m.group(4)),
                    "dropped": int(m.group(5)), "overlimits": int(m.group(6))})
    return out


ANSI = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")


def status_lines(path):
    try:
        return [ANSI.sub("", l).rstrip() for l in open(path).read().splitlines() if l.strip()]
    except OSError:
        return []


def compose(r, ok, why):
    meta = readj(r + "/meta.json", {})
    cell = parse_cell(meta.get("cell", ""))
    links = readj(r + "/links.json", {})
    sp = readj(r + "/spec.json", {})
    cl = readj(r + "/client.json")
    errs = []
    if why:
        errs.append(why)
    if ok == "1" and cl is None:
        ok = "0"
        errs.append("no client output: " + " ".join(open(r + "/client.err").read().split()[-30:]) if os.path.exists(r + "/client.err") else "no client output")
    rec = {"id": meta.get("id"), "ok": ok == "1", "bin": meta.get("bin"), "bin_sha": meta.get("bin_sha"),
           "order": meta.get("order"), "rep": meta.get("rep"), "cell": meta.get("cell")}
    for k in ORDER:
        if k in cell:
            rec[k] = num(cell, k) if k not in ("MODE", "MIX") else cell[k]
    rec["extra"] = {k: v for k, v in cell.items() if k not in ORDER}
    rec.update({"start": meta.get("start"), "load0": meta.get("load0"), "waited_s": meta.get("waited_s"),
                "host_busy_pct": host_busy(r)})
    for k in ("edge_cpus", "exit_cpus"):
        if meta.get(k):
            rec[k] = meta[k]
    st = {s: readj("%s/status-%s.json" % (r, s), {}) for s in ("edge", "exit")}
    policed = 0
    try:
        policed = sum(1 for l in open(r + "/police.log") if "policed" in l)
    except OSError:
        pass
    rt = {s: retrans(r, s) for s in ("exit", "edge")}
    rec["links"] = {"wait_s": round(links.get("links_wait_s", -1), 2), "at_start": links.get("links_at_start"),
                    "end": rt["exit"]["sockets_end"], "edge_status": st["edge"].get("links"),
                    "exit_status": st["exit"].get("links"), "policed": policed}
    if cl is not None:
        roles = [u.get("role") for u in sp.get("users", [])]
        users = []
        for i, u in enumerate(cl.get("users", [])):
            x = {"id": u["id"], "src": u["src"], "role": roles[i] if i < len(roles) else None,
                 "flows": ",".join("%s%d@%g" % (f["kind"], f.get("n", 1), f.get("start_s", 0)) for f in u["flows"]),
                 "dn_mbit": u["dn_mbit"], "up_mbit": u["up_mbit"], "conns": u["conns"]}
            for k in ("dial_errs", "short_ends", "probe_errs"):
                if u.get(k):
                    x[k] = u[k]
            if u.get("probe"):
                x["probe_p99"] = {w: l["p99"] for w, l in u["probe"].items() if l}
            users.append(x)
        rec.update({"total_s": cl.get("total_s"), "windows": cl.get("windows"), "users": users,
                    "dn_sum_mbit": cl.get("dn_sum_mbit"), "up_sum_mbit": cl.get("up_sum_mbit"),
                    "probe": cl.get("probe_all"), "ping_tun": cl.get("ping_tun"), "tun": cl.get("tun"),
                    "dn_1s": cl.get("dn_1s"), "up_1s": cl.get("up_1s")})
        if cl.get("errs"):
            rec["client_errs"] = cl["errs"]
    rec["cpu"] = pidstat(r + "/pidstat.txt", {"edge": links.get("pid_edge"), "exit": links.get("pid_exit")})
    rec["retrans"] = rt
    rec["bottleneck"] = {"dn": tc_parse(r + "/tc-dn.txt"), "up": tc_parse(r + "/tc-up.txt")}
    keep = ("links", "target", "users", "mbit", "phase", "flowing", "peak_mbit", "pressed", "cap_mbit")
    rec["status"] = {s: {k: st[s].get(k) for k in keep if k in st[s]} for s in st}
    rec["status_lines"] = {s: status_lines("%s/status-%s.txt" % (r, s)) for s in ("edge", "exit")}
    if errs:
        rec["errs"] = errs
    print(json.dumps(rec))


# ------------------------------------------------------------------ analysis

def rate_eff(rec):
    r = float(rec["RATE"])
    pol, leaf = parse_mode(rec["MODE"])
    if leaf.startswith("agg:"):
        r = min(r, float(leaf[4:]))
    return r


def capacity(rec):
    r = rate_eff(rec)
    pol, leaf = parse_mode(rec["MODE"])
    if pol is not None:
        r = min(r, float(rec["N"]) * pol)
    return r * EFF


def fair_floor(cap, demands):
    """max-min fair share (water-filling); demands: list, None = unlimited"""
    left, rest = cap, list(range(len(demands)))
    share = {}
    while rest:
        lvl = left / len(rest)
        lim = [i for i in rest if demands[i] is not None and demands[i] <= lvl]
        if not lim:
            for i in rest:
                share[i] = lvl
            break
        for i in lim:
            share[i] = demands[i]
            left -= demands[i]
            rest.remove(i)
    return [share[i] for i in range(len(demands))]


def lat(rec, key, win):
    x = (rec.get(key) or {}).get(win)
    return x if x else None


def derive(rec):
    """per-run metrics and verdict flags"""
    m = {}
    if not rec.get("ok") or "users" not in rec:
        return m
    cap = capacity(rec)
    rate = rate_eff(rec)
    mix = rec["MIX"]
    dirn = rec.get("extra", {}).get("DIR", "dn")
    users = rec["users"]
    m["cap"] = cap
    m["dn"] = rec["dn_sum_mbit"].get("main", rec["dn_sum_mbit"].get("after_settled"))
    m["up"] = rec["up_sum_mbit"].get("main", rec["up_sum_mbit"].get("after_settled"))
    pa = lat(rec, "probe", "all")
    if pa and pa["n"]:
        floor = pa["p1"]
        m["rtt_floor"] = floor
        m["d_bound"] = floor + max(20.0, 3 * 1500 * 8 / (rate * 1e6) * 1000)
        pm = lat(rec, "probe", "main") or lat(rec, "probe", "after")
        if pm and pm["n"]:
            m["p50"], m["p99"], m["pmax"] = pm["p50"], pm["p99"], pm["max"]
            m["plost"] = pm["lost"]
            m["d"] = pm["p99"] <= m["d_bound"]
    ta = lat(rec, "ping_tun", "all")
    if ta and ta["n"]:
        tm = lat(rec, "ping_tun", "main") or ta
        m["tfloor"] = ta["p1"]
        m["t_bound"] = ta["p1"] + max(10.0, 3 * 1500 * 8 / (rate * 1e6) * 1000)
        m["t50"], m["t99"], m["tlost"] = tm["p50"], tm["p99"], tm["lost"]
        m["tun"] = tm["p99"] <= m["t_bound"]
    win = "after_settled" if mix == "newheavy" else "main"

    def thr(u, d, w=win):
        return (u["dn_mbit"] if d == "dn" else u["up_mbit"]).get(w, 0.0)

    def bulk_dir(u, d):
        return any(f.startswith(d) for f in u["flows"].split(","))

    # (b) generic and (c), per direction carrying bulk
    worst = None
    for d in ("dn", "up"):
        bulk = [u for u in users if bulk_dir(u, d) and u["role"] != "light"]
        if not bulk:
            continue
        if mix == "newheavy" and win == "after_settled":
            bulk = [u for u in bulk if u["role"] in ("exist", "heavy")]
        light = sum(thr(u, d) for u in users if u not in bulk)
        tot = sum(thr(u, d) for u in bulk)
        m["c_" + d] = tot / cap
        if len(bulk) > 1:
            fl = fair_floor(cap - light, [None] * len(bulk))
            ratios = [thr(u, d) / f for u, f in zip(bulk, fl)]
            m["fair_" + d] = min(ratios)
            worst = min(ratios) if worst is None else min(worst, min(ratios))
    cs = [m[k] for k in ("c_dn", "c_up") if k in m]
    if cs:
        m["c_ratio"] = min(cs)
        m["c"] = min(cs) >= 0.97
    if mix == "lone16":
        u = next(u for u in users if u["role"] == "lone")
        m["a_ratio"] = thr(u, dirn) / cap
        m["a"] = m["a_ratio"] >= 0.95
    if mix in ("one_vs_16", "s5") and worst is not None:
        m["b_ratio"] = worst
        m["b"] = worst >= 0.9
    if mix == "one_vs_16":
        one = next(u for u in users if u["role"] == "one")
        m["one"] = thr(one, dirn)
    if mix == "newheavy":
        ex = next(u for u in users if u["role"] == "exist")
        hv = next(u for u in users if u["role"] == "heavy")
        light = sum(thr(u, dirn) for u in users if u["role"] == "light")
        fl = fair_floor(cap - light, [None, None])[0]
        m["exist_before"] = thr(ex, dirn, "before")
        m["exist_after"] = thr(ex, dirn, "after_settled")
        m["heavy_after"] = thr(hv, dirn, "after_settled")
        m["e_bulk_ratio"] = m["exist_after"] / fl
        pa2 = lat(rec, "probe", "after")
        pb = lat(rec, "probe", "before")
        if pb and pb["n"]:
            m["p99_before"] = pb["p99"]
        if pa2 and pa2["n"] and "d_bound" in m:
            m["p99_after"] = pa2["p99"]
            m["e"] = pa2["p99"] <= m["d_bound"] and m["e_bulk_ratio"] >= 0.9
        m["b_ratio"] = m["e_bulk_ratio"]
    if mix == "tunping" and rec.get("tun"):
        m["tun_dn"] = rec["tun"]["dn_mbit"].get("main")
        m["tun_up"] = rec["tun"]["up_mbit"].get("main")
    c = rec.get("cpu", {})
    m["cpu_edge"] = c.get("edge", {}).get("mean")
    m["cpu_exit"] = c.get("exit", {}).get("mean")
    m["rt_exit"] = rec.get("retrans", {}).get("exit", {}).get("pct")
    m["rt_edge"] = rec.get("retrans", {}).get("edge", {}).get("pct")
    m["links"] = rec.get("links", {}).get("end")
    m["busy"] = rec.get("host_busy_pct")
    return m


def agg(vals):
    v = [x for x in vals if x is not None]
    if not v:
        return None
    return (statistics.median(v), min(v), max(v))


def fmt(t, p=1, pct=False):
    if t is None:
        return "-"
    f = (lambda x: "%.0f%%" % (100 * x)) if pct else (lambda x: ("%%.%df" % p) % x)
    if t[1] == t[2]:
        return f(t[0])
    return "%s [%s..%s]" % (f(t[0]), f(t[1]), f(t[2]))


VERD = [("a", "a_ratio", 0.95), ("b", "b_ratio", 0.9), ("c", "c_ratio", 0.97)]


def analyze(path, as_json=False):
    recs = [json.loads(l) for l in open(path) if l.strip()]
    groups, order = {}, []
    for r in recs:
        k = (r.get("cell"), r.get("bin"))
        if k not in groups:
            groups[k] = []
            order.append(k)
        groups[k].append(r)
    summary = []
    for k in order:
        rs = groups[k]
        good = [r for r in rs if r.get("ok")]
        ms = [derive(r) for r in good]
        s = {"cell": k[0], "bin": k[1], "runs": len(rs), "ok": len(good),
             "failed": [r.get("errs") for r in rs if not r.get("ok")]}
        keys = sorted({x for m in ms for x in m if not isinstance(m[x], bool)})
        s["med"] = {x: agg([m.get(x) for m in ms]) for x in keys}
        for flag in ("a", "b", "c", "d", "e", "tun"):
            got = [m[flag] for m in ms if flag in m]
            if not got:
                continue
            med = s["med"]
            if flag == "a":
                v = med["a_ratio"][0] >= 0.95
            elif flag == "b":
                v = med["b_ratio"][0] >= 0.9
            elif flag == "c":
                v = med["c_ratio"][0] >= 0.97
            elif flag == "d":
                v = med["p99"][0] <= med["d_bound"][0]
            elif flag == "e":
                v = med["p99_after"][0] <= med["d_bound"][0] and med["e_bulk_ratio"][0] >= 0.9
            else:
                v = med["t99"][0] <= med["t_bound"][0]
            s.setdefault("verdict", {})[flag] = {"pass": v, "runs": "%d/%d" % (sum(got), len(got))}
        summary.append(s)
    # new against old, per cell
    by_cell = {}
    for s in summary:
        by_cell.setdefault(s["cell"], {})[s["bin"]] = s
    for c, b in by_cell.items():
        if "old" in b and "new" in b:
            for d in ("dn", "up"):
                o, n = b["old"]["med"].get(d), b["new"]["med"].get(d)
                if o and n and o[0] > 0.01:
                    b["new"].setdefault("vs_old", {})[d] = round(n[0] / o[0], 3)
            vo = b["new"].get("vs_old", {})
            if vo:
                b["new"].setdefault("verdict", {})["thr"] = {"pass": all(x >= 0.97 for x in vo.values()),
                                                             "runs": " ".join("%s %.3f" % i for i in vo.items())}
    if as_json:
        print(json.dumps(summary, indent=1))
        return
    for s in summary:
        md = s["med"]
        print("== %s  BIN=%s  runs %d (ok %d)" % (s["cell"], s["bin"], s["runs"], s["ok"]))
        for f in s["failed"]:
            print("   failed run: %s" % f)
        if not s["ok"]:
            continue
        print("   capacity %.2f Mbit/s   links %s   host busy %s%%" % (md["cap"][0], fmt(md.get("links"), 0), fmt(md.get("busy"), 0)))
        print("   total dn %s  up %s Mbit/s   (c) %s" % (fmt(md.get("dn"), 2), fmt(md.get("up"), 2), fmt(md.get("c_ratio"), pct=True)))
        if "a_ratio" in md:
            print("   (a) lone user %s of capacity" % fmt(md["a_ratio"], pct=True))
        if "one" in md:
            print("   1-connection user %s Mbit/s" % fmt(md["one"], 2))
        if "b_ratio" in md:
            print("   (b) worst bulk user %s of its floor" % fmt(md["b_ratio"], pct=True))
        if "exist_after" in md:
            print("   newheavy: existing bulk before %s, after %s, heavy after %s Mbit/s; probe p99 before %s, after %s ms" % (
                fmt(md.get("exist_before"), 2), fmt(md.get("exist_after"), 2), fmt(md.get("heavy_after"), 2),
                fmt(md.get("p99_before")), fmt(md.get("p99_after"))))
        if "p99" in md:
            print("   probe p50 %s  p99 %s  max %s ms  lost %s   RTT_floor %s  bound (d) %s" % (
                fmt(md["p50"]), fmt(md["p99"]), fmt(md.get("pmax")), fmt(md.get("plost"), 0),
                fmt(md.get("rtt_floor")), fmt(md.get("d_bound"))))
        if "t99" in md:
            print("   TUN ping p50 %s  p99 %s ms  lost %s   floor %s  bound %s" % (
                fmt(md["t50"]), fmt(md["t99"]), fmt(md.get("tlost"), 0), fmt(md.get("tfloor")), fmt(md.get("t_bound"))))
        if "tun_dn" in md:
            print("   TUN bulk dn %s  up %s Mbit/s" % (fmt(md["tun_dn"], 2), fmt(md.get("tun_up"), 2)))
        print("   CPU %% (one core = 100): edge %s  exit %s   retrans %%: exit %s  edge %s" % (
            fmt(md.get("cpu_edge")), fmt(md.get("cpu_exit")), fmt(md.get("rt_exit"), 3), fmt(md.get("rt_edge"), 3)))
        if s.get("verdict"):
            print("   verdict: " + "  ".join("%s %s (%s)" % (f, "PASS" if v["pass"] else "FAIL", v["runs"])
                                         for f, v in s["verdict"].items()))


def main():
    a = sys.argv[1:]
    if not a:
        die("usage: ana.py RES.jsonl [--json] | schedule PLAN REPS | spec CELL FILE | compose RAW OK WHY")
    if a[0] == "schedule":
        schedule(a[1], a[2])
    elif a[0] == "spec":
        spec(a[1], a[2])
    elif a[0] == "compose":
        compose(a[1], a[2], a[3] if len(a) > 3 else "")
    else:
        analyze(a[0], "--json" in a)


if __name__ == "__main__":
    main()
