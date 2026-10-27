#!/usr/bin/env python3
"""ana.py: the big rig's analysis (R1 task 2), and the helpers run.sh uses, so
the mixes, their windows and the verdicts are defined in one place.

  ana.py OUT/res.jsonl [--json]     table: median [min..max] of the repeats of
                                    each cell and binary, P10 verdicts (a)-(e),
                                    the probe and TUN ping p99 bounds, and new/old
  ana.py schedule PLAN REPS         run.sh: the runs, ABBABA per cell (TSV)
  ana.py spec "CELL" SPEC.json      run.sh: rigload's spec; prints "NUSERS TOTAL_S NTUN"
                                    (IPv4 port users, run length, TUN users)
  ana.py compose RAWDIR OK WHY      run.sh: one res.jsonl line from a run's raw files

Definitions (REDESIGN.fa.md section 5.1 and 6):
  capacity  the bottleneck's TCP payload rate: rate x 1448/1514 (MSS 1448 with
            timestamps; htb counts the 1514 B Ethernet frame). rate = RATE, or
            min(RATE, AR) with agg:AR in MODE; with police:PR in MODE the
            capacity is also at most N x PR (MODE=police:PR+agg:AR: both).
            RATE_UP (optional) is the upload direction's rate (default RATE),
            so each direction has its own capacity.
            A TUN user's goodput is counted as the inner IP packets it took
            (x 1320/1268 for IPv4, x 1320/1248 for IPv6: TUN MTU 1320, inner
            MSS with timestamps), so it compares with the same capacity.
  cap_P10   decision T17 (wip/r1/decisions.fa.md): the P10 gates (a), (b),
            (c) and the bulk part of (e) are taken at the socket, after hs2's
            record framing: cap_P10 = capacity x F, F = 0.9743 = (991.4 - 4) /
            (991.4 + 22), the shaped record sizes of obfs.NewHTTPSLengthSampler
            (mean 991.4 B, 4 B shaper header, 22 B TLS 1.3 overhead). F is a
            constant of the record shape, not measured per run; changing it
            needs a DPI review. Both ratios are reported: to cap_P10 (the gate)
            and to the raw capacity ("raw").
  RTT_floor p1 of the probe over the whole measured span of the same run (for
            the TUN ping: p1 of the TUN ping itself).
  (a) the lone user (16 connections) >= 0.95 x cap_P10
  (b) every bulk user >= 0.9 x its floor; floor = max-min fair share of
      cap_P10 (water-filling): min(demand, cap_P10 / active users), the light
      users' (measured) demand first. In one_vs_16 this is the 1-connection user
      next to the 16-connection user (floor = (cap_P10 - light) / 2).
  (c) total bulk >= 0.97 x cap_P10, in each direction that carries bulk
  (d) light (probe) p99 <= RTT_floor + max(20 ms, 3 x 1500 B / rate), where a
      probe still unanswered 2 s after the run counts as infinitely late: more
      than 1% lost fails (d) whatever the p99 of the answered ones (judge
      round 1, open item 9); rate = the slower direction
  (e) newheavy: after the heavy user joins, the probe p99 (from the join to the
      end) stays within (d), and the existing bulk user (from SETTLE s after the
      join) >= 0.9 x its floor (now shared with the heavy user)
  tun TUN ping p99 <= RTT_floor(ping) + max(10 ms, 3 x 1500 B / rate), lost
      pings counted as for (d)
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
F_REC = 0.9743  # T17: hs2's record framing, (991.4 - 4) / (991.4 + 22)
LOST_MAX = 0.01  # more lost probes or pings than this: p99 over all sent is infinite
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
        if not re.match(r"^[a-z0-9]+$", d["BIN"]):
            die("%s:%d: BIN %s (old, new, or a label of BINS)" % (plan, ln, d["BIN"]))
        U, v6 = int(d["USERS"]), int(d.get("V6", "0"))
        k = int(d.get("TUN", "1" if d["MIX"] == "tunping" else "0"))
        if U < 1 or U - k - (v6 > 0) > 500 or k > 500 or k > U:
            die("%s:%d: USERS %d, TUN %d, V6 %d (at most 500 IPv4 port users, 500 TUN users and the IPv6 user)" % (plan, ln, U, k, v6))
        if v6 and v6 <= k:
            die("%s:%d: V6=%d is a TUN user (TUN=%d takes the first users); the IPv6 port user must be a port user" % (plan, ln, v6, k))
        if d["MIX"] == "tunping" and k < 1:
            die("%s:%d: tunping needs TUN >= 1" % (plan, ln))
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
                    # k > 2 (the judge): forward, then backward for every
                    # further repeat, as ABBABA (old, a, b, b, a, old, b, a, old)
                    seq += bins if r == 0 else bins[::-1]
        count = {}
        for b in seq:
            count[b] = count.get(b, 0) + 1
            o = "A" if b == bins[0] else ("B" if len(bins) > 1 and b == bins[1] else "C")
            print("%s-%s-%d\t%s\t%s\t%d\t%s" % (h, b, count[b], b, o, count[b], c))


# ------------------------------------------------------------------ spec

def user_src(i):
    return "10.100.%d.%d" % (i // 250, i % 250 + 1)


def tun_src(i, v6):
    """the i-th TUN user's inner address (from 0): IPv4, or IPv6 in a /64 of its own"""
    return "fd00:110:%x::1" % (i + 1) if v6 else "10.110.%d.%d" % (i // 250, i % 250 + 1)


TUN_MTU = 1320
TUN_MSS = {4: TUN_MTU - 52, 6: TUN_MTU - 72}  # inner TCP payload per full packet (timestamps on)


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
    # TUN=K: the first K users go through the TUN, each from its own inner
    # address; TUNV6=1: those inner addresses (and hs0's) are IPv6
    K = num(d, "TUN", 1 if mix == "tunping" else 0)
    tunv6 = d.get("TUNV6", "0") == "1"
    if K > U:
        die("TUN %d > USERS %d" % (K, U))
    users = [{"id": "u%d" % (i + 1), "role": "light", "path": "tun" if i < K else "port", "flows": []}
             for i in range(U)]
    probe = lambda: {"kind": "probe", "n": 1, "start_s": 0}
    ping = lambda: {"kind": "ping", "n": 1, "start_s": 0}
    light_flow = lambda u: ping() if u["path"] == "tun" else probe()
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
    elif mix == "tunping":
        # the same inner host: bulk through hs0 and the ping next to it
        if K < 1:
            die("tunping needs TUN >= 1")
        users[0]["role"] = "tunbulk"
        users[0]["flows"].append({"kind": d.get("TUNDIR", "dn"), "n": num(d, "TDN", 4), "start_s": 0})
        users[0]["flows"].append(ping())
        light = list(range(1, U))
    # light users: one probe each (a ping on the TUN); if the mix leaves none,
    # user 1 carries it (tunping's user 1 already pings)
    for i in light:
        users[i]["flows"].append(light_flow(users[i]))
    if not light and mix != "tunping":
        users[0]["flows"].append(light_flow(users[0]))
    if mix == "s5":
        # S5's TUN ping, from hs0's own address (not one of the USERS)
        users.append({"id": "hs0", "role": "pinger", "path": "tun", "src": "", "v6": tunv6, "flows": [ping()]})
    windows.append({"name": "all", "from": warm, "to": total})
    # V6=K: port user K is one IPv6 /64 (a fresh address in it for every
    # connection); the IPv4 addresses go to the other port users in order
    v6 = int(d.get("V6", "0"))
    n4 = nt = 0
    for i, u in enumerate(users):
        if u["role"] == "pinger":
            continue
        if u["path"] == "tun":
            u["v6"] = tunv6
            u["src"] = tun_src(nt, tunv6)
            nt += 1
        elif i + 1 == v6:
            u["v6"] = True
            u["src"] = ""
        else:
            u["src"] = user_src(n4)
            n4 += 1
    s = {"total_s": total, "bulk": "10.201.0.1:5311", "echo": "10.201.0.1:5312",
         "bulk6": "[fd00:201::1]:5311", "echo6": "[fd00:201::1]:5312", "v6_prefix": "fd77:1::",
         "tun_bulk": "10.77.0.2:5213", "tun_bulk6": "[fd00:77::2]:5213",
         "tun_ping": "10.77.0.2", "tun_ping6": "fd00:77::2",
         "probe_ms": 200, "probe_bytes": 32, "ping_ms": 200,
         "users": users, "windows": windows}
    if n4 > 500 or nt > 500:
        die("%d IPv4 port users, %d TUN users (at most 500 each)" % (n4, nt))
    with open(path, "w") as f:
        json.dump(s, f, indent=1)
    print(n4, total, nt)


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
           "bin_env": meta.get("bin_env", ""),
           "order": meta.get("order"), "rep": meta.get("rep"), "cell": meta.get("cell")}
    for k in ORDER:
        if k in cell:
            rec[k] = num(cell, k) if k not in ("MODE", "MIX") else cell[k]
    rec["extra"] = {k: v for k, v in cell.items() if k not in ORDER}
    rec.update({"start": meta.get("start"), "load0": meta.get("load0"), "waited_s": meta.get("waited_s"),
                "host_busy_pct": host_busy(r)})
    for k in ("edge_cpus", "exit_cpus", "uptime", "top"):
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
        su = sp.get("users", [])
        users = []
        for i, u in enumerate(cl.get("users", [])):
            s_u = su[i] if i < len(su) else {}
            x = {"id": u["id"], "src": u["src"], "role": s_u.get("role"), "path": u.get("path", "port"),
                 "fam": 6 if s_u.get("v6") else 4,
                 "flows": ",".join("%s%d@%g" % (f["kind"], f.get("n", 1), f.get("start_s", 0)) for f in u["flows"]),
                 "dn_mbit": u["dn_mbit"], "up_mbit": u["up_mbit"], "conns": u["conns"]}
            for k in ("dial_errs", "short_ends", "probe_errs"):
                if u.get(k):
                    x[k] = u[k]
            for k in ("dn_1s", "up_1s"):
                if u.get(k):
                    x[k] = u[k]
            for k in ("probe", "ping"):
                if u.get(k):
                    x[k + "_p99"] = {w: l["p99"] for w, l in u[k].items() if l}
            users.append(x)
        rec.update({"total_s": cl.get("total_s"), "windows": cl.get("windows"), "users": users,
                    "dn_sum_mbit": cl.get("dn_sum_mbit"), "up_sum_mbit": cl.get("up_sum_mbit"),
                    "tun_dn_sum_mbit": cl.get("tun_dn_sum_mbit"), "tun_up_sum_mbit": cl.get("tun_up_sum_mbit"),
                    "probe": cl.get("probe_all"), "probe_tun": cl.get("probe_tun"), "ping_tun": cl.get("ping_tun"),
                    "dn_1s": cl.get("dn_1s"), "up_1s": cl.get("up_1s")})
        if cl.get("errs"):
            rec["client_errs"] = cl["errs"]
    rec["cpu"] = pidstat(r + "/pidstat.txt", {"edge": links.get("pid_edge"), "exit": links.get("pid_exit")})
    rec["retrans"] = rt
    mem = {}
    for s_ in ("edge", "exit"):
        try:
            for l in open("%s/mem-%s.txt" % (r, s_)):
                k, v = l.split(":", 1)
                mem.setdefault(s_, {})[k.strip()[2:].lower() + "_kb"] = int(v.split()[0])
        except (OSError, ValueError, IndexError):
            pass
    rec["mem"] = mem
    rec["bottleneck"] = {"dn": tc_parse(r + "/tc-dn.txt"), "up": tc_parse(r + "/tc-up.txt")}
    keep = ("links", "target", "users", "mbit", "phase", "flowing", "peak_mbit", "pressed", "cap_mbit")
    rec["status"] = {s: {k: st[s].get(k) for k in keep if k in st[s]} for s in st}
    rec["status_lines"] = {s: status_lines("%s/status-%s.txt" % (r, s)) for s in ("edge", "exit")}
    if errs:
        rec["errs"] = errs
    print(json.dumps(rec))


# ------------------------------------------------------------------ analysis

def rate_eff(rec, d="dn"):
    """the bottleneck rate of direction d (dn, up), Mbit/s"""
    r = float(rec["RATE"])
    if d == "up":
        r = float((rec.get("extra") or {}).get("RATE_UP", rec.get("RATE_UP", r)))
    pol, leaf = parse_mode(rec["MODE"])
    if leaf.startswith("agg:"):
        r = min(r, float(leaf[4:]))
    return r


def capacity(rec, d="dn"):
    """the raw capacity of direction d: the bottleneck's TCP payload rate"""
    r = rate_eff(rec, d)
    pol, leaf = parse_mode(rec["MODE"])
    if pol is not None:
        r = min(r, float(rec["N"]) * pol)
    return r * EFF


def cap_p10(rec, d="dn"):
    """T17: the capacity at the socket, after the record framing (the P10 gates)"""
    return capacity(rec, d) * F_REC


def lost_frac(l):
    """the share of the sent probes or pings never answered"""
    sent = l.get("sent") or (l.get("n", 0) + l.get("lost", 0))
    return (l.get("lost", 0) / sent) if sent else 0.0


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
    caps = {d: capacity(rec, d) for d in ("dn", "up")}
    caps10 = {d: cap_p10(rec, d) for d in ("dn", "up")}
    rate = min(rate_eff(rec, "dn"), rate_eff(rec, "up"))
    mix = rec["MIX"]
    dirn = rec.get("extra", {}).get("DIR", "dn")
    cap, cap10 = caps[dirn], caps10[dirn]
    users = rec["users"]
    m["cap"] = cap
    m["cap_p10"] = cap10
    if caps["up"] != caps["dn"]:
        m["cap_up"] = caps["up"]
    wsum = "after_settled" if mix == "newheavy" else "main"
    # goodput through the tunnel: port users and TUN users together
    m["dn"] = rec["dn_sum_mbit"].get(wsum, 0) + (rec.get("tun_dn_sum_mbit") or {}).get(wsum, 0)
    m["up"] = rec["up_sum_mbit"].get(wsum, 0) + (rec.get("tun_up_sum_mbit") or {}).get(wsum, 0)
    if any(u.get("path") == "tun" and u.get("role") != "pinger" for u in users):
        m["tun_dn"] = (rec.get("tun_dn_sum_mbit") or {}).get(wsum)
        m["tun_up"] = (rec.get("tun_up_sum_mbit") or {}).get(wsum)
    pa = lat(rec, "probe", "all")
    if pa and pa["n"]:
        floor = pa["p1"]
        m["rtt_floor"] = floor
        m["d_bound"] = floor + max(20.0, 3 * 1500 * 8 / (rate * 1e6) * 1000)
        pm = lat(rec, "probe", "main") or lat(rec, "probe", "after")
        if pm and pm["n"]:
            m["p50"], m["p99"], m["pmax"] = pm["p50"], pm["p99"], pm["max"]
            m["plost"] = pm["lost"]
            m["plost_frac"] = lost_frac(pm)
            m["d"] = pm["p99"] <= m["d_bound"] and m["plost_frac"] <= LOST_MAX
    ta = lat(rec, "ping_tun", "all")
    if ta and ta["n"]:
        tm = lat(rec, "ping_tun", "main") or ta
        m["tfloor"] = ta["p1"]
        m["t_bound"] = ta["p1"] + max(10.0, 3 * 1500 * 8 / (rate * 1e6) * 1000)
        m["t50"], m["t99"], m["tlost"] = tm["p50"], tm["p99"], tm["lost"]
        m["tlost_frac"] = lost_frac(tm)
        m["tun"] = tm["p99"] <= m["t_bound"] and m["tlost_frac"] <= LOST_MAX
    win = "after_settled" if mix == "newheavy" else "main"

    def thr(u, d, w=win):
        """a user's goodput as bytes on the link: a TUN user's inner packets"""
        v = (u["dn_mbit"] if d == "dn" else u["up_mbit"]).get(w, 0.0)
        if u.get("path") == "tun":
            v *= TUN_MTU / TUN_MSS[u.get("fam", 4)]
        return v

    def bulk_dir(u, d):
        return any(f.startswith(d) for f in u["flows"].split(","))

    # (b) generic and (c), per direction carrying bulk; each ratio both to
    # cap_P10 (the gate, T17) and to the raw capacity (*_raw)
    worst = worst_raw = None
    for d in ("dn", "up"):
        bulk = [u for u in users if bulk_dir(u, d) and u["role"] not in ("light", "pinger")]
        if not bulk:
            continue
        if mix == "newheavy" and win == "after_settled":
            bulk = [u for u in bulk if u["role"] in ("exist", "heavy")]
        light = sum(thr(u, d) for u in users if u not in bulk)
        tot = sum(thr(u, d) for u in bulk)
        m["c_" + d] = tot / caps10[d]
        m["c_raw_" + d] = tot / caps[d]
        if len(bulk) > 1:
            for cp, key in ((caps10[d], "fair_"), (caps[d], "fair_raw_")):
                fl = fair_floor(cp - light, [None] * len(bulk))
                ratios = [thr(u, d) / f for u, f in zip(bulk, fl)]
                m[key + d] = min(ratios)
            worst = m["fair_" + d] if worst is None else min(worst, m["fair_" + d])
            worst_raw = m["fair_raw_" + d] if worst_raw is None else min(worst_raw, m["fair_raw_" + d])
    cs = [m[k] for k in ("c_dn", "c_up") if k in m]
    if cs:
        m["c_ratio"] = min(cs)
        m["c_raw"] = min(m[k] for k in ("c_raw_dn", "c_raw_up") if k in m)
        m["c"] = min(cs) >= 0.97
    if mix == "lone16":
        u = next(u for u in users if u["role"] == "lone")
        m["a_ratio"] = thr(u, dirn) / cap10
        m["a_raw"] = thr(u, dirn) / cap
        m["a"] = m["a_ratio"] >= 0.95
    if mix in ("one_vs_16", "s5") and worst is not None:
        m["b_ratio"] = worst
        m["b_raw"] = worst_raw
        m["b"] = worst >= 0.9
    if mix == "one_vs_16":
        one = next(u for u in users if u["role"] == "one")
        m["one"] = thr(one, dirn)
    if mix == "newheavy":
        ex = next(u for u in users if u["role"] == "exist")
        hv = next(u for u in users if u["role"] == "heavy")
        light = sum(thr(u, dirn) for u in users if u["role"] in ("light", "pinger"))
        fl = fair_floor(cap10 - light, [None, None])[0]
        fl_raw = fair_floor(cap - light, [None, None])[0]
        m["exist_before"] = thr(ex, dirn, "before")
        m["exist_after"] = thr(ex, dirn, "after_settled")
        m["heavy_after"] = thr(hv, dirn, "after_settled")
        m["e_bulk_ratio"] = m["exist_after"] / fl
        m["e_bulk_raw"] = m["exist_after"] / fl_raw
        pa2 = lat(rec, "probe", "after")
        pb = lat(rec, "probe", "before")
        if pb and pb["n"]:
            m["p99_before"] = pb["p99"]
        if pa2 and pa2["n"] and "d_bound" in m:
            m["p99_after"] = pa2["p99"]
            m["plost_after_frac"] = lost_frac(pa2)
            m["e"] = pa2["p99"] <= m["d_bound"] and m["plost_after_frac"] <= LOST_MAX and m["e_bulk_ratio"] >= 0.9
        m["b_ratio"] = m["e_bulk_ratio"]
        m["b_raw"] = m["e_bulk_raw"]
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
                v = med["p99"][0] <= med["d_bound"][0] and med["plost_frac"][0] <= LOST_MAX
            elif flag == "e":
                v = (med["p99_after"][0] <= med["d_bound"][0] and med["e_bulk_ratio"][0] >= 0.9
                     and med["plost_after_frac"][0] <= LOST_MAX)
            else:
                v = med["t99"][0] <= med["t_bound"][0] and med["tlost_frac"][0] <= LOST_MAX
            s.setdefault("verdict", {})[flag] = {"pass": v, "runs": "%d/%d" % (sum(got), len(got))}
        summary.append(s)
    # new against old, per cell
    by_cell = {}
    for s in summary:
        by_cell.setdefault(s["cell"], {})[s["bin"]] = s
    for c, b in by_cell.items():
        if "old" not in b:
            continue
        for nb in b:
            if nb == "old":
                continue
            for d in ("dn", "up"):
                o, n = b["old"]["med"].get(d), b[nb]["med"].get(d)
                if o and n and o[0] > 0.01:
                    b[nb].setdefault("vs_old", {})[d] = round(n[0] / o[0], 3)
            vo = b[nb].get("vs_old", {})
            if vo:
                b[nb].setdefault("verdict", {})["thr"] = {"pass": all(x >= 0.97 for x in vo.values()),
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
        print("   capacity %.2f Mbit/s (cap_P10 %.2f)%s   links %s   host busy %s%%" % (
            md["cap"][0], md["cap_p10"][0], ("   up capacity %.2f" % md["cap_up"][0]) if "cap_up" in md else "",
            fmt(md.get("links"), 0), fmt(md.get("busy"), 0)))
        print("   total dn %s  up %s Mbit/s   (c) %s of cap_P10 (raw %s)" % (
            fmt(md.get("dn"), 2), fmt(md.get("up"), 2), fmt(md.get("c_ratio"), pct=True), fmt(md.get("c_raw"), pct=True)))
        if "a_ratio" in md:
            print("   (a) lone user %s of cap_P10 (raw %s)" % (fmt(md["a_ratio"], pct=True), fmt(md.get("a_raw"), pct=True)))
        if "one" in md:
            print("   1-connection user %s Mbit/s" % fmt(md["one"], 2))
        if "b_ratio" in md:
            print("   (b) worst bulk user %s of its floor in cap_P10 (raw %s)" % (fmt(md["b_ratio"], pct=True), fmt(md.get("b_raw"), pct=True)))
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
            print("   of which TUN users (inner goodput) dn %s  up %s Mbit/s" % (fmt(md["tun_dn"], 2), fmt(md.get("tun_up"), 2)))
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
