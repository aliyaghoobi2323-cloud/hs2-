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

User classes (R1 round 3, wip/r1/classes; MIX home, gamer_vs_dl,
stream_vs_dl): per class, what its users feel, in the main window. One-way
delays are per direction (up = user to server, dn = back) over the class's
floor in that direction (the least one-way delay of the run, window "all");
"path" is the same over the path's own RTT/2 (a floor more than FLOOR_WARN
over the path, a standing queue the floor hides, is flagged on the verdict
line). Proposed verdicts, REPORT ONLY (not gates; the lead decides):
  game  p99 <= floor + 10 ms and late (> floor + 100 ms) + lost <= 0.1 %,
        each direction (game over TCP and gameudp each)
  call  p99 <= floor + 30 ms, each direction
  stream  no rebuffer, every viewer started, startup <= 2 s; only when the
        bitrate (SBR) <= the viewers' fair share (max-min of cap_P10 in the
        download direction, the other users' measured download first);
        otherwise n/a
  web   p95 page load <= 1.05 x the base's (old) in the same cell
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
CLASS_MIXES = ["home", "gamer_vs_dl", "stream_vs_dl"]
MIXES = ["s5", "lone16", "one_vs_16", "newheavy", "tunping", "frozen", "idle", "exhaust"] + CLASS_MIXES
# a kill cell (KILLQ=T, run.sh: one link of the edge reset T s after the
# client starts) counts a connection as cut by it when it ended within this
# many s after the kill
KILL_WIN = 5.0
ORDER = ["N", "RATE", "RTT", "LOSS", "MODE", "USERS", "MIX"]
# the class mixes: users in this order, repeated (USERS need not be a multiple)
CLASS_TEMPLATE = {
    "home": ["game", "call", "stream", "web", "heavy", "stream", "web", "uploader"],
    "gamer_vs_dl": ["game", "heavy"],
    "stream_vs_dl": ["stream", "heavy"],
}
# the classes' proposed verdicts (report only)
GAME_P99 = 10.0     # ms over the floor, each direction
GAME_LATE = 0.001   # late (> floor + 100 ms) and lost, of the sent
CALL_P99 = 30.0     # ms over the floor, each direction
STREAM_STARTUP = 2.0  # s
WEB_TOL = 1.05      # p95 page load against the base's
FLOOR_WARN = 5.0    # ms: a class's floor this far over RTT/2 is flagged (a standing queue)
INF = 1e9           # "never": a percentile that falls on a lost message or a failed page


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
        if "KILLQ" in d and not re.match(r"^\d+(\.\d+)?$", d["KILLQ"]):
            die("%s:%d: KILLQ %s (seconds after the client starts)" % (plan, ln, d["KILLQ"]))
        if d.get("KILLWHAT", "quiet") not in ("quiet", "bulk", "any"):
            die("%s:%d: KILLWHAT %s (quiet, bulk, any)" % (plan, ln, d["KILLWHAT"]))
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
    elif mix == "frozen":
        # the red team's C5 (R1 round-4 judge; rigload frozen.go): FROZENU (1)
        # users whose app stops reading, FROZEN (64) downloads between them,
        # FWAVE (32) a second from WARM on, each read for FREAD s (1) with a
        # FRCVBUF B (16384) receive buffer, then held unread; the other users
        # probe
        fu = num(d, "FROZENU", 1)
        need(fu + 1)
        nf, wave = num(d, "FROZEN", 64), num(d, "FWAVE", 32)
        fread, frcv = float(d.get("FREAD", "1")), num(d, "FRCVBUF", 16384)
        if nf < 1 or wave < 1 or fread <= 0 or frcv < 1024:
            die("FROZEN, FWAVE >= 1, FREAD > 0, FRCVBUF >= 1024")
        for j in range(fu):
            n = nf // fu + (1 if j < nf % fu else 0)
            users[j]["role"] = "frozen"
            users[j]["flows"].append({"kind": "frozen", "n": n, "start_s": warm, "wave": max(1, wave // fu),
                                      "read_s": fread, "rcvbuf": frcv})
        light = list(range(fu, U))
    elif mix == "exhaust":
        # the R1 red team's second pass (item 5; rigload red2.go): user 1
        # holds HOLD (2000) connections that never send (slow loris), HOLDW
        # (200) opened a second from WARM on; user 2 runs CHURN (64) workers
        # that open, echo 32 bytes and close connections without pause;
        # user 3 downloads with CONNS (16) connections; the rest probe
        need(4)
        nh, hw, nc = num(d, "HOLD", 2000), num(d, "HOLDW", 200), num(d, "CHURN", 64)
        users[0]["role"] = "hold"
        if nh > 0:
            users[0]["flows"].append({"kind": "hold", "n": nh, "start_s": warm, "wave": max(1, hw)})
        users[1]["role"] = "churn"
        if nc > 0:
            users[1]["flows"].append({"kind": "churn", "n": nc, "start_s": warm})
        users[2]["role"] = "bulk"
        users[2]["flows"].append({"kind": dirn, "n": conns, "start_s": 0})
        # UDPF (0): user 4 sends this many datagrams a second to the user
        # port's UDP side from WARM on, each from a new source port; GAMEU
        # (0) players (UDP) among the light users
        udpf, gu = float(d.get("UDPF", "0")), num(d, "GAMEU", 0)
        light = list(range(3, U))
        if udpf > 0:
            need(5)
            users[3]["role"] = "udpflood"
            users[3]["flows"].append({"kind": "udpflood", "n": 1, "start_s": warm, "rate": udpf})
            light = list(range(4, U))
        for j in light[:gu]:
            users[j]["role"] = "gameudp"
            users[j]["flows"].append({"kind": "gameudp", "n": 1, "start_s": 0})
        light = light[gu:]
    elif mix == "idle":
        # an idle tunnel (the red team's Z10, the R1 round-4 judge's DPI
        # capture): no user sends anything until one probe in the last 2 s,
        # which keeps the client running the whole span
        for u in users:
            u["role"] = "idle"
        users[0]["flows"].append({"kind": "probe", "n": 1, "start_s": max(total - 2, 0)})
        # IDLEDL=S (the R1 red team's second pass): user 1 downloads on 4
        # connections for the first S s, then the tunnel is idle (an idle
        # tunnel after use, not from its start)
        if num(d, "IDLEDL", 0) > 0:
            users[0]["flows"].append({"kind": "dn", "n": 4, "start_s": 0, "stop_s": num(d, "IDLEDL", 0)})
        light = []
    elif mix in CLASS_MIXES:
        # the user classes (rigload classes.go), CLASS_TEMPLATE repeated over
        # USERS: GAME=tcp|udp|both (both: the players alternate, TCP first),
        # SBR stream bitrate (5 Mbit/s), CALLR call rate each way (1.5),
        # PAGE s between a browser's pages (5), CONNS the heavy user's
        # connections (16) in DIR, UPN the uploader's (1), TAIL s after the
        # main window that every flow still runs (5), so a message, page or
        # segment begun at the window's end still meets the same load
        tmpl = CLASS_TEMPLATE[mix]
        need(len(tmpl))
        game = d.get("GAME", "both")
        if game not in ("tcp", "udp", "both"):
            die("GAME %s (tcp, udp, both)" % game)
        sbr, callr, page = float(d.get("SBR", "5")), float(d.get("CALLR", "1.5")), float(d.get("PAGE", "5"))
        if sbr <= 0 or callr <= 0 or page <= 0:
            die("SBR, CALLR and PAGE must be > 0")
        upn, tail = num(d, "UPN", 1), num(d, "TAIL", 5)
        ackf = float(d.get("ACKF", "0"))
        total = warm + dur + tail
        roles = [tmpl[i % len(tmpl)] for i in range(U)]
        nst = roles.count("stream")
        ng = js = 0
        for i, role in enumerate(roles):
            if role == "game":
                role = "game" if game == "tcp" or (game == "both" and ng % 2 == 0) else "gameudp"
                ng += 1
                f = {"kind": role, "n": 1, "start_s": 0}
            elif role == "call":
                f = {"kind": "call", "n": 1, "start_s": 0, "rate": callr}
            elif role == "stream":
                # a viewer starts after the warm-up, the viewers spread over one
                # segment (4 s), so the startup meets the bulk users' steady load
                f = {"kind": "stream", "n": 1, "start_s": round(warm + 4.0 * js / nst, 3), "rate": sbr}
                js += 1
            elif role == "web":
                f = {"kind": "web", "n": 1, "start_s": 0, "every_s": page}
            elif role == "heavy":
                f = {"kind": dirn, "n": conns, "start_s": 0}
                if ackf > 0 and dirn == "dn":
                    # ACKF (the R1 red team's second pass, R2-1): the heavy
                    # user's downloads acknowledge in their own data (QUIC
                    # inside a proxy connection), this fraction of what
                    # they get
                    f = {"kind": "ackdn", "n": conns, "start_s": 0, "rate": ackf}
            else:
                f = {"kind": "up", "n": upn, "start_s": 0}
            users[i]["role"] = role
            users[i]["flows"].append(f)
        light = []
    # light users: one probe each (a ping on the TUN); if the mix leaves none,
    # user 1 carries it (tunping's user 1 already pings; the class mixes have
    # their own delays and no probe)
    for i in light:
        users[i]["flows"].append(light_flow(users[i]))
    if not light and mix not in ("tunping", "idle") and mix not in CLASS_MIXES:
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
    if "KILLQ" in d:
        # a kill cell: run.sh resets one link at KILLQ s; the client logs the
        # connections and opens cut ones again (rigload cuts.go)
        s["kill_s"] = float(d["KILLQ"])
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


def pctl(v, p):
    """the p-quantile of v (nearest rank); None when v is empty"""
    if not v:
        return None
    v = sorted(v)
    return v[max(0, int(math.ceil(p * len(v))) - 1)]


def log_lines(path, pat):
    """(seconds of the day, text) of the hs2 log lines (HH:MM:SS.ffffff first)
    whose text matches pat"""
    out = []
    try:
        for l in open(path, errors="replace"):
            mt = re.match(r"^(\d\d):(\d\d):(\d\d(?:\.\d+)?) (.*)$", l.rstrip("\n"))
            if mt and re.search(pat, mt.group(4)):
                out.append((int(mt.group(1)) * 3600 + int(mt.group(2)) * 60 + float(mt.group(3)), mt.group(4)))
    except OSError:
        pass
    return out


def day_s(clock):
    """seconds of the day of HH:MM:SS.fff (None: not one)"""
    mt = re.match(r"^(\d\d):(\d\d):(\d\d(?:\.\d+)?)$", clock or "")
    return int(mt.group(1)) * 3600 + int(mt.group(2)) * 60 + float(mt.group(3)) if mt else None


def kill_summary(r, cl, su):
    """a kill cell (run.sh KILLQ; rigload cuts.go): what one link's reset at
    the edge cut. A connection is live at the kill when it opened before and
    ended after it; cut by it when it ended cut (before its flow's stop)
    within KILL_WIN s. Users: those with a live connection, and those that
    lost one. Recovery: each cut flow's time to carry data again (-1 never).
    Outage: each game, call and probe flow's longest gap between two
    messages from 2 s before the kill (per direction). The edge's log gives
    when it saw the link end and when a link of the same class came up
    again (QUIET: "QUIET link qN up"; else "link N up")."""
    kj = readj(r + "/kill.json", {})
    t = kj.get("t_rel", cl["kill_s"])
    out = {k: kj.get(k) for k in ("what", "class", "peer", "n_quiet", "n_bulk", "killed", "clock")}
    out.update({"planned_s": cl["kill_s"], "at_s": t})
    roles = {i + 1: (u.get("role") or "?") for i, u in enumerate(su)}
    conns = cl.get("conns") or []
    live = [c for c in conns if c["o"] <= t < c["c"]]
    cut = [c for c in live if c["x"] >= 0 and t - 0.05 <= c["x"] <= t + KILL_WIN]
    cut_ids = {id(c) for c in cut}
    by_kind, by_role = {}, {}
    for c in live:
        b = by_kind.setdefault(c["k"], [0, 0])
        b[0] += 1
        b[1] += id(c) in cut_ids
    ul, uh = {c["u"] for c in live}, {c["u"] for c in cut}
    for u in ul:
        b = by_role.setdefault(roles.get(u, "?"), [0, 0])
        b[0] += 1
        b[1] += u in uh
    rc = [x for x in cl.get("recov") or [] if t - 0.05 <= x["cut"] <= t + KILL_WIN]
    back = [x["back"] for x in rc if x["back"] >= 0]
    out.update({"live": len(live), "cut": len(cut), "share": round(len(cut) / len(live), 4) if live else None,
                "users_live": len(ul), "users_hit": len(uh),
                "users_share": round(len(uh) / len(ul), 4) if ul else None,
                "by_kind": by_kind, "by_role": by_role,
                "recov_n": len(rc), "never": len(rc) - len(back),
                "recov_p50": pctl(back, 0.5), "recov_p95": pctl(back, 0.95), "recov_max": max(back) if back else None})
    og = {}
    for o in cl.get("outage") or []:
        og.setdefault("%s_%s" % (o["k"], o["dir"]), []).append(o["gap_ms"])
    out["outage_ms"] = {k: {"p50": pctl(v, 0.5), "max": max(v), "n": len(v),
                            "over_1s": sum(1 for x in v if x > 1000)} for k, v in og.items()}
    k0 = day_s(kj.get("clock"))
    if k0 is not None:
        after = lambda pat: next(((s - k0) % 86400 for s, _ in log_lines(r + "/edge.log", pat)
                                  if 0 <= (s - k0) % 86400 < 600), None)
        quiet = kj.get("class") == "quiet"
        out["edge_down_s"] = after(r"QUIET link q\d+ down" if quiet else r"(reverse )?link \d+ (from \S+ )?down")
        out["replace_s"] = after(r"QUIET link q\d+ up" if quiet else r"(reverse )?link \d+ up")
    return out


def frozen_log(path):
    """the edge's wedge guard and QUIET hold lines in a frozen cell: the
    connections it reset, the links that stopped reading, the users held"""
    reset = sum(int(m.group(1)) for _, x in log_lines(path, r"^mtcp: reset \d+ connection")
                for m in [re.match(r"^mtcp: reset (\d+)", x)] if m)
    stopped = len(log_lines(path, r"stopped reading for several seconds"))
    held = sum(int(m.group(1)) for _, x in log_lines(path, r"user\(s\) held more than")
               for m in [re.match(r"^mtcp: (\d+) user", x)] if m)
    return {"wedge_reset": reset, "stopped_lines": stopped, "held_users": held}


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
    for k in ("edge_cpus", "exit_cpus", "uptime", "top", "panel_sndbuf"):
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
            for k in ("rt", "stream", "web"):  # the classes, main window
                if u.get(k):
                    x[k] = u[k]
            users.append(x)
        rec.update({"total_s": cl.get("total_s"), "windows": cl.get("windows"), "users": users,
                    "dn_sum_mbit": cl.get("dn_sum_mbit"), "up_sum_mbit": cl.get("up_sum_mbit"),
                    "tun_dn_sum_mbit": cl.get("tun_dn_sum_mbit"), "tun_up_sum_mbit": cl.get("tun_up_sum_mbit"),
                    "probe": cl.get("probe_all"), "probe_tun": cl.get("probe_tun"), "ping_tun": cl.get("ping_tun"),
                    "dn_1s": cl.get("dn_1s"), "up_1s": cl.get("up_1s")})
        if cl.get("classes"):
            rec["classes"] = cl["classes"]
        if cl.get("notes"):
            rec["client_notes"] = cl["notes"]
        if cl.get("errs"):
            rec["client_errs"] = cl["errs"]
        if cl.get("kill_s"):
            rec["kill"] = kill_summary(r, cl, su)
        if cl.get("frozen"):
            rec["frozen"] = dict(cl["frozen"], edge_log=frozen_log(r + "/edge.log"))
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
    # the host's TCP memory through the run (run.sh, R1 round 4): a run that
    # saw pressure (the counter grew, or the memory passed tcp_mem[1]) is
    # marked host_pressure
    hm = readj(r + "/hostmem.json")
    if hm is not None:
        rec["hostmem"] = hm
        rec["host_pressure"] = bool(hm.get("pressures") or hm.get("samples_past_tcp_mem1"))
    # the opt-in memory cap (rigguard.sh, RIG_GUARD=cap): max_events > 0
    # means the cap held this run's memory back, so its numbers are the
    # capped rig's
    mg = readj(r + "/memcg.json")
    if mg is not None:
        rec["memcg"] = mg
        if mg.get("max_events") or mg.get("oom_kills"):
            errs.append("memory cap reached (max events %s, OOM kills %s): a capped run" % (mg.get("max_events"), mg.get("oom_kills")))
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
    if mix in CLASS_MIXES:
        # every user's bytes (the classes' too) against cap_P10, per direction
        m["util_dn"] = m["dn"] / caps10["dn"]
        m["util_up"] = m["up"] / caps10["up"]
        derive_classes(rec, m, caps10["dn"])
        return host_metrics(rec, m)
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
    return host_metrics(rec, m)


def host_metrics(rec, m):
    c = rec.get("cpu", {})
    m["cpu_edge"] = c.get("edge", {}).get("mean")
    m["cpu_exit"] = c.get("exit", {}).get("mean")
    m["rt_exit"] = rec.get("retrans", {}).get("exit", {}).get("pct")
    m["rt_edge"] = rec.get("retrans", {}).get("edge", {}).get("pct")
    m["links"] = rec.get("links", {}).get("end")
    m["busy"] = rec.get("host_busy_pct")
    kill_metrics(rec, m)
    return m


def kill_metrics(rec, m):
    """a kill cell's and a frozen cell's numbers, for the median of the repeats"""
    k = rec.get("kill")
    if k and k.get("live"):
        m["kill_live"] = k["live"]
        m["kill_share"] = k["share"]
        m["kill_users_share"] = k["users_share"]
        m["kill_users_hit"] = k["users_hit"]
        m["kill_users_live"] = k["users_live"]
        m["kill_never"] = k["never"]
        for q in ("recov_p50", "recov_p95", "recov_max", "replace_s", "edge_down_s"):
            if k.get(q) is not None:
                m["kill_" + q] = k[q]
        for kind, (lv, ct) in k.get("by_kind", {}).items():
            m["kill_share_" + kind] = ct / lv if lv else 0
        for role, (lv, ht) in k.get("by_role", {}).items():
            m["kill_users_" + role] = ht / lv if lv else 0
        for key, o in k.get("outage_ms", {}).items():
            m["kill_out_%s_p50" % key] = o["p50"]
            m["kill_out_%s_max" % key] = o["max"]
    f = rec.get("frozen")
    if f:
        g = f.get("probe_gap_ms", {})
        for a, b in (("max", "frz_gap_max"), ("p50", "frz_gap_p50"), ("probe_p99", "frz_probe_p99"),
                     ("probe_max", "frz_probe_max"), ("probe_lost", "frz_probe_lost")):
            if a in g:
                m[b] = g[a]
        m["frz_conns"] = f.get("conns")
        m["frz_frozen"] = f.get("frozen")
        m["frz_mbytes"] = f.get("mbytes")
        el = f.get("edge_log", {})
        m["frz_wedge_reset"] = el.get("wedge_reset", 0)
        m["frz_held_users"] = el.get("held_users", 0)


def stream_fair(rec, cap10):
    """the viewers' fair share of the download direction: max-min of cap_P10,
    the viewers and the users with bulk downloads unlimited, every other
    user's measured download (main window) first"""
    users = rec["users"]
    dem = []
    for u in users:
        greedy = u["role"] == "stream" or any(f.startswith("dn") or f.startswith("ackdn") for f in u["flows"].split(","))
        dem.append(None if greedy else u["dn_mbit"].get("main", 0.0))
    sh = fair_floor(cap10, dem)
    st = [s for u, s in zip(users, sh) if u["role"] == "stream"]
    return min(st) if st else None


def derive_classes(rec, m, cap10):
    """the classes' numbers (main window) and their proposed verdicts v_*
    (report only)"""
    cl = rec.get("classes") or {}
    half = float(rec["RTT"]) / 2
    m["path_owd"] = half
    for k in ("game", "gameudp", "call"):
        c = cl.get(k)
        if not c:
            continue
        ok = True
        for d in ("up", "dn"):
            o = (c.get(d) or {}).get("main")
            fl = (c.get("floor_ms") or {}).get(d, -1)
            if not o or fl is None or fl < 0 or not o.get("sent"):
                ok = False
                continue
            p = "%s_%s_" % (k, d)
            m[p + "floor"] = fl
            m[p + "floor_path"] = fl - half
            for q in ("p50", "p99", "p999"):
                m[p + q] = o[q] - fl if o.get(q, -1) >= 0 else INF
            m[p + "p99_path"] = o["p99"] - half if o.get("p99", -1) >= 0 else INF
            m[p + "jit99"] = o.get("jit_p99") if o.get("jit_p99", -1) >= 0 else None
            m[p + "late"] = (o.get("late", 0) + o.get("lost", 0)) / o["sent"]
            m[p + "lost"] = o.get("lost", 0)
            if o.get("offered_mbit"):
                m[p + "deliv"] = o.get("delivered_mbit", 0) / o["offered_mbit"]
            if k == "call":
                ok = ok and m[p + "p99"] <= CALL_P99
            else:
                ok = ok and m[p + "p99"] <= GAME_P99 and m[p + "late"] <= GAME_LATE
        m["v_" + k] = ok
        if c.get("no_record"):
            m[k + "_norec"] = c["no_record"]
    st = cl.get("stream")
    if st:
        m["stream_users"] = st["users"]
        m["stream_sbr"] = st["bitrate_mbit"]
        m["stream_notstarted"] = st["not_started"]
        m["stream_startup_p50"] = st["startup_p50_s"] if st["startup_p50_s"] >= 0 else INF
        m["stream_startup_max"] = st["startup_max_s"] if st["startup_max_s"] >= 0 and not st["not_started"] else INF
        m["stream_rebuf"] = st["rebuffers"]
        m["stream_rebuf_s"] = st["rebuffer_s"]
        m["stream_rebuf_users"] = st["rebuffer_users"]
        m["stream_seg_mbit"] = st["seg_mbit_mean"]
        fair = stream_fair(rec, cap10)
        if fair is not None:
            m["stream_fair"] = fair
            if st["bitrate_mbit"] <= fair:
                m["v_stream"] = (st["not_started"] == 0 and st["rebuffers"] == 0
                                 and m["stream_startup_max"] <= STREAM_STARTUP)
    w = (cl.get("web") or {}).get("main")
    if w:
        m["web_pages"] = w["pages"]
        m["web_failed"] = w["failed"]
        for k, f in (("web_p50", "load_p50_ms"), ("web_p95", "load_p95_ms"),
                     ("web_fb50", "fb_p50_ms"), ("web_fb95", "fb_p95_ms")):
            m[k] = w[f] if w[f] >= 0 else INF
    heavy = [u for u in rec["users"] if u["role"] == "heavy"]
    if heavy:
        hv = sorted(sum(u[x].get("main", 0.0) for x in ("dn_mbit", "up_mbit")) for u in heavy)
        m["heavy_mean"] = sum(hv) / len(hv)
        m["heavy_min"] = hv[0]
    return m


def agg(vals):
    v = sorted(x for x in vals if x is not None)
    if not v:
        return None
    med = statistics.median(v)
    if len(v) % 2 == 0 and v[len(v) // 2] >= INF:
        med = INF  # an even count with "never" in the middle: never, not a mean of the two
    return (med, v[0], v[-1])


def fmt(t, p=1, pct=False):
    if t is None:
        return "-"
    f0 = (lambda x: "%.0f%%" % (100 * x)) if pct else (lambda x: ("%%.%df" % p) % x)
    f = lambda x: "inf" if x >= INF else f0(x)
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
        if parse_cell(k[0] or "").get("MIX") in CLASS_MIXES:
            class_verdicts(s, ms)
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
            # web (report only): p95 page load against the base's
            o, n = b["old"]["med"].get("web_p95"), b[nb]["med"].get("web_p95")
            if o and n:
                if o[0] >= INF:
                    r = 1.0 if n[0] >= INF else 0.0
                else:
                    r = INF if n[0] >= INF else n[0] / o[0]
                b[nb]["web_vs_old"] = round(r, 3)
                b[nb].setdefault("class_verdict", {})["web"] = {
                    "pass": r <= WEB_TOL, "runs": "p95 %s / old %s ms" % (fmt(n, 0), fmt(o, 0))}
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
        if "kill_share" in md:
            f_ = lambda k, p=2, pct=False: fmt(md.get(k), p, pct)
            print("   kill: connections cut %s of %s live (%s), users hit %s of %s (%s); recovery p50 %s p95 %s max %s s, never %s; "
                  "edge saw the end after %s s, a replacement up after %s s" % (
                      f_("kill_share", 3), f_("kill_live", 0), f_("kill_share", pct=True), f_("kill_users_hit", 0),
                      f_("kill_users_live", 0), f_("kill_users_share", pct=True), f_("kill_recov_p50"), f_("kill_recov_p95"),
                      f_("kill_recov_max"), f_("kill_never", 0), f_("kill_edge_down_s"), f_("kill_replace_s")))
            ks = sorted(k[11:] for k in md if k.startswith("kill_share_"))
            if ks:
                print("      cut by kind: " + "  ".join("%s %s" % (k, f_("kill_share_" + k, pct=True)) for k in ks))
            kr = sorted(k[11:] for k in md if k.startswith("kill_users_") and k[11:] not in ("hit", "live", "share"))
            if kr:
                print("      users hit by role: " + "  ".join("%s %s" % (k, f_("kill_users_" + k, pct=True)) for k in kr))
            ko = sorted(k[9:-4] for k in md if k.startswith("kill_out_") and k.endswith("_max"))
            if ko:
                print("      longest gap ms (p50 / max over flows): " + "  ".join(
                    "%s %s / %s" % (k, f_("kill_out_%s_p50" % k, 0), f_("kill_out_%s_max" % k, 0)) for k in ko))
        if "frz_conns" in md:
            f_ = lambda k, p=1: fmt(md.get(k), p)
            print("   frozen: %s downloads (%s frozen, %s MB read before), the probes' longest gap max %s p50 %s ms, "
                  "probe p99 %s max %s ms lost %s; the edge's wedge guard reset %s, users held %s" % (
                      f_("frz_conns", 0), f_("frz_frozen", 0), f_("frz_mbytes"), f_("frz_gap_max", 0), f_("frz_gap_p50", 0),
                      f_("frz_probe_p99"), f_("frz_probe_max"), f_("frz_probe_lost", 0), f_("frz_wedge_reset", 0),
                      f_("frz_held_users", 0)))
        if "util_dn" in md:
            print_classes(s, md)
        if s.get("verdict"):
            print("   verdict: " + "  ".join("%s %s (%s)" % (f, "PASS" if v["pass"] else "FAIL", v["runs"])
                                         for f, v in s["verdict"].items()))
        if s.get("class_verdict"):
            print("   class verdict (report only): " + "  ".join(
                "%s %s (%s)" % (f, {True: "PASS", False: "FAIL", None: "n/a"}[v["pass"]], v["runs"])
                for f, v in s["class_verdict"].items()))


def class_verdicts(s, ms):
    """the classes' proposed verdicts (report only) on the median of the repeats"""
    med = s["med"]
    cv = {}
    g = lambda k: med.get(k, (INF,))[0]
    for k in ("game", "gameudp", "call"):
        got = [m["v_" + k] for m in ms if "v_" + k in m]
        if not got:
            continue
        if k == "call":
            v = all(g("call_%s_p99" % d) <= CALL_P99 for d in ("up", "dn"))
        else:
            v = all(g("%s_%s_p99" % (k, d)) <= GAME_P99 and g("%s_%s_late" % (k, d)) <= GAME_LATE
                    for d in ("up", "dn"))
        runs = "%d/%d" % (sum(got), len(got))
        # a floor well above the path's RTT/2 is a standing queue the verdict does not see
        sq = ["%s +%.0f ms" % (d, med["%s_%s_floor_path" % (k, d)][0]) for d in ("up", "dn")
              if med.get("%s_%s_floor_path" % (k, d)) and med["%s_%s_floor_path" % (k, d)][0] > FLOOR_WARN]
        if sq:
            runs += "; floor over the path: " + ", ".join(sq)
        cv[k] = {"pass": v, "runs": runs}
    if "stream_users" in med:
        got = [m["v_stream"] for m in ms if "v_stream" in m]
        if got:
            v = g("stream_notstarted") == 0 and g("stream_rebuf") == 0 and g("stream_startup_max") <= STREAM_STARTUP
            cv["stream"] = {"pass": v, "runs": "%d/%d" % (sum(got), len(got))}
        else:
            cv["stream"] = {"pass": None, "runs": "SBR %s > fair share %s Mbit/s" % (
                fmt(med.get("stream_sbr"), 2), fmt(med.get("stream_fair"), 2))}
    if cv:
        s["class_verdict"] = cv


def print_classes(s, md):
    """the class lines of one cell and binary"""
    f = lambda k, p=1, pct=False: fmt(md.get(k), p, pct)
    print("   all users against cap_P10: dn %s  up %s   heavy users mean %s min %s Mbit/s" % (
        f("util_dn", pct=True), f("util_up", pct=True), f("heavy_mean", 2), f("heavy_min", 2)))
    names = {"game": "game (TCP)", "gameudp": "game (UDP)", "call": "call"}
    for k in ("game", "gameudp", "call"):
        if "%s_up_floor" % k not in md and "%s_dn_floor" % k not in md:
            continue
        parts = []
        for d in ("up", "dn"):
            p = "%s_%s_" % (k, d)
            if p + "floor" not in md:
                parts.append("%s: no data" % d)
                continue
            x = "%s: p50 %s p99 %s p99.9 %s  jitter %s  late+lost %%: %s (lost %s)" % (
                d, f(p + "p50"), f(p + "p99"), f(p + "p999"), f(p + "jit99"),
                fmt(tuple(100 * v for v in md[p + "late"]), 2), f(p + "lost", 0))
            if p + "deliv" in md:
                x += "  delivered %s" % f(p + "deliv", pct=True)
            parts.append(x)
        print("   %s, one-way ms over the floor:" % names[k])
        for x in parts:
            print("      " + x)
        print("      floor up %s dn %s ms (path RTT/2 %s; p99 over the path: up %s dn %s)%s" % (
            f(k + "_up_floor"), f(k + "_dn_floor"), f("path_owd"), f(k + "_up_p99_path"), f(k + "_dn_p99_path"),
            ("   flows without a server record %s" % f(k + "_norec", 0)) if k + "_norec" in md else ""))
    if "stream_users" in md:
        print("   stream: %s viewers at %s Mbit/s (fair share %s): startup p50 %s max %s s, not started %s; "
              "rebuffers %s (viewers %s, %s s); segment rate %s Mbit/s" % (
                  f("stream_users", 0), f("stream_sbr", 2), f("stream_fair", 2), f("stream_startup_p50", 2),
                  f("stream_startup_max", 2), f("stream_notstarted", 0), f("stream_rebuf", 0),
                  f("stream_rebuf_users", 0), f("stream_rebuf_s"), f("stream_seg_mbit", 2)))
    if "web_pages" in md:
        print("   web: page load p50 %s p95 %s ms, first byte p50 %s p95 %s ms; pages %s, failed %s%s" % (
            f("web_p50", 0), f("web_p95", 0), f("web_fb50", 0), f("web_fb95", 0), f("web_pages", 0),
            f("web_failed", 0), ("   p95 new/old %s" % ("inf" if s["web_vs_old"] >= INF else "%.3f" % s["web_vs_old"]))
            if "web_vs_old" in s else ""))


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
