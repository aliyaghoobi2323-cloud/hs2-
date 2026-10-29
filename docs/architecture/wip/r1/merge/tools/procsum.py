#!/usr/bin/env python3
"""procsum.py RAWDIR [RES.jsonl]: the R1 merge's smoke checks, one row per run
of a rig block (RAWDIR = run.sh's OUT): links (wanted N, at start, the edge's
serving and the exit's links at the end and after the idle tail, sockets
whose peer changed = links dialed again, sockets that never carried a byte),
throughput, host TCP memory pressure, and the hs2 processes over the run from
PROC_TRACE's samples: fds, goroutines, RSS (MB) and threads before the users
start / their peak / at the end of the users' window / after POST_IDLE, plus
"growth" = the users' window's last third against its first third. Then the
logs: panics, fatal lines, data races."""
import glob, json, os, re, statistics, sys

raw = sys.argv[1]
res = {}
rp = sys.argv[2] if len(sys.argv) > 2 else os.path.join(raw, "res.jsonl")
if os.path.exists(rp):
    for l in open(rp):
        if l.strip():
            r = json.loads(l)
            res[r["id"]] = r


def jl(p):
    try:
        return json.load(open(p))
    except Exception:
        return {}


def ss_socks(p):
    """peer -> (bytes_acked, bytes_received) of each established link socket"""
    out = {}
    if not os.path.exists(p):
        return None
    peer = None
    for line in open(p):
        if re.match(r"^\d", line):
            f = line.split()
            peer = f[3] if len(f) > 3 else None
            continue
        if peer:
            a = re.search(r"bytes_acked:(\d+)", line)
            b = re.search(r"bytes_received:(\d+)", line)
            out[peer] = (int(a.group(1)) if a else 0, int(b.group(1)) if b else 0)
            peer = None
    return out


def trace(p):
    rows = []
    if not os.path.exists(p):
        return rows
    for line in open(p):
        f = line.split()
        if len(f) < 5:
            continue
        try:
            rows.append((float(f[0]), int(f[1]), int(f[2]), int(f[3]), None if f[4] == "NA" else int(f[4])))
        except ValueError:
            pass
    return rows


def phase(rows, c0, c1):
    # pre: up to 1 s after the client starts (the sampler starts with the
    # tunnel, the client right after it: no user connection yet); load: the
    # users' window; post: after it (the POST_IDLE tail)
    pre = [r for r in rows if r[0] < c0 + 1]
    load = [r for r in rows if c0 + 1 <= r[0] <= c1]
    post = [r for r in rows if r[0] > c1 + 1]
    return pre, load, post


def steady(rows, c0, c1):
    # the users' steady part's last 20 s (to 3 s before their end): a
    # leak keeps growing there, a heap that grew to its working size is flat
    return [r for r in rows if c1 - 23 <= r[0] <= c1 - 3]


def growth(rows, i):
    v = col(rows, i)
    if len(v) < 2 or not v[0]:
        return None
    return (v[-1] - v[0]) / v[0] * 100


def col(rows, i):
    return [r[i] for r in rows if r[i] is not None]




def fmt(x, p=0):
    return "-" if x is None else ("%%.%df" % p) % x


rows = []
for d in sorted(glob.glob(os.path.join(raw, "raw", "*"))):
    rid = os.path.basename(d)
    meta = jl(os.path.join(d, "meta.json"))
    lk = jl(os.path.join(d, "links.json"))
    cell = meta.get("cell", "")
    N = int(re.search(r"\bN=(\d+)", cell).group(1)) if re.search(r"\bN=(\d+)", cell) else None
    mix = (re.search(r"\bMIX=(\S+)", cell) or [None, None])[1]
    se, sx = jl(os.path.join(d, "status-edge.json")), jl(os.path.join(d, "status-exit.json"))
    sei, sxi = jl(os.path.join(d, "status-edge-idle.json")), jl(os.path.join(d, "status-exit-idle.json"))
    s0, s1, s2 = (ss_socks(os.path.join(d, "ss-edge-%s.txt" % t)) for t in ("start", "end", "idle"))
    redial = None
    if s0 is not None and s1 is not None:
        redial = len(set(s1) - set(s0))
        if s2 is not None:
            redial += len(set(s2) - set(s1))
    dead = None
    last = s2 if s2 else s1
    if last is not None:
        dead = sum(1 for a, b in last.values() if a == 0 or b == 0)
    try:
        c0 = float(open(os.path.join(d, "client-t0.txt")).read().split()[0])
        c1 = float(open(os.path.join(d, "client-end.txt")).read().split()[0])
    except Exception:
        c0 = c1 = None
    pr = {}
    for side in ("edge", "exit"):
        t = trace(os.path.join(d, "proc-%s.tsv" % side))
        if not t or c0 is None:
            pr[side] = None
            continue
        pre, load, post = phase(t, c0, c1)
        st = steady(t, c0, c1)
        pr[side] = {}
        for name, i, sc in (("fd", 1, 1), ("gor", 4, 1), ("rss", 2, 1 / 1024), ("thr", 3, 1)):
            def pick(rs, how):
                v = col(rs, i)
                return None if not v else how(v) * sc
            pr[side][name] = (pick(pre, lambda v: v[0]), pick(load, max), pick(load, lambda v: v[-1]), pick(post, lambda v: v[-1]), growth(st, i))
    r = res.get(rid, {})
    hm = jl(os.path.join(d, "hostmem.json"))
    logs = ""
    for side in ("edge", "exit"):
        p = os.path.join(d, side + ".log")
        if os.path.exists(p):
            logs += open(p, errors="replace").read()
    bad = {"panic": len(re.findall(r"^panic:|goroutine \d+ \[", logs, re.M)), "fatal": len(re.findall(r"fatal error|FATAL", logs)), "race": logs.count("DATA RACE")}
    rows.append(dict(id=rid, N=N, mix=mix, ok=r.get("ok"), why=r.get("errs") or "", links_at_start=lk.get("links_at_start"), wait=lk.get("links_wait_s"),
                     e_links=se.get("links"), e_serving=se.get("serving"), x_links=sx.get("links"), e_idle=sei.get("serving"), x_idle=sxi.get("links"),
                     socks=len(last) if last is not None else None, redial=redial, dead=dead,
                     dn=(r.get("dn_sum_mbit") or {}).get("main"), up=(r.get("up_sum_mbit") or {}).get("main"),
                     press=hm.get("pressures"), maxpages=hm.get("max_pages"), past1=hm.get("samples_past_tcp_mem1"),
                     cpu=(r.get("cpu") or {}), pr=pr, bad=bad))

print("| run | N | MIX | ok | links up at start (wait s) | edge serving / exit links (end) | same after idle | link sockets; re-dialed; silent | down / up Mbit/s | host TCP pressures; max pages; samples past tcp_mem[1] | CPU edge / exit % (mean) | panic / fatal / race in logs |")
print("|---|---|---|---|---|---|---|---|---|---|---|---|")
for x in rows:
    print("| %s | %s | %s | %s | %s (%s) | %s / %s | %s / %s | %s; %s; %s | %s / %s | %s; %s; %s | %s / %s | %d / %d / %d |" % (
        x["id"], x["N"], x["mix"], x["ok"], x["links_at_start"], x["wait"], x["e_serving"], x["x_links"], x["e_idle"], x["x_idle"],
        x["socks"], x["redial"], x["dead"], fmt(x["dn"], 1), fmt(x["up"], 1), x["press"], x["maxpages"], x["past1"],
        fmt((x["cpu"].get("edge") or {}).get("mean")), fmt((x["cpu"].get("exit") or {}).get("mean")),
        x["bad"]["panic"], x["bad"]["fatal"], x["bad"]["race"]))
print()
print("Process samples (PROC_TRACE, every 5 s): before the users (first sample, the tunnel up) / peak / end of the users' window / after POST_IDLE (20 s after the client ended); growth = the change over the users' last 20 s (to 3 s before their end), % (a leak grows there; a heap that reached its working size is flat).")
print()
print("| run | side | fds | goroutines | RSS MB | threads | growth fds / gor / RSS % |")
print("|---|---|---|---|---|---|---|")
for x in rows:
    for side in ("edge", "exit"):
        p = x["pr"].get(side)
        if not p:
            print("| %s | %s | - | - | - | - | - |" % (x["id"], side))
            continue
        q = lambda k, pr=0: "%s / %s / %s / %s" % tuple(fmt(v, pr) for v in p[k][:4])
        print("| %s | %s | %s | %s | %s | %s | %s / %s / %s |" % (x["id"], side, q("fd"), q("gor"), q("rss", 1), q("thr"),
              fmt(p["fd"][4], 1), fmt(p["gor"][4], 1), fmt(p["rss"][4], 1)))
