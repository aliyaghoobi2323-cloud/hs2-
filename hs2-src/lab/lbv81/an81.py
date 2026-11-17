#!/usr/bin/env python3
# an81.py RESDIR: phase table of the lbv81 soak (light latency per class,
# crowd failures, heavy Mbit/s, per-path LB state + kernel ss ground truth,
# lb auto2 log events, rss/goroutines, panics).
import json, sys, re, os, glob, datetime, collections
R = sys.argv[1]
t0 = int(open(f"{R}/t0").read())
PH = [("warm", 20, 60), ("base", 60, 150), ("a_loss3", 150, 240), ("a_loss10", 240, 330), ("a_rec", 330, 390),
      ("b_queue", 390, 540), ("b_rec", 540, 600), ("c_black", 600, 720), ("c_rec", 720, 810),
      ("d_flap", 810, 1050), ("d_rec", 1050, 1110), ("e_rst", 1110, 1230), ("e_rec", 1230, 1290),
      ("f_mild", 1290, 1440), ("f_rec", 1440, 1500), ("g_edge", 1500, 1620), ("g_exit", 1620, 1680), ("h_clean", 1680, 1860)]
PATH = {"192.168.50.1": "a", "fd00:51::1": "b"}
for k in range(2, 10):
    PATH[f"192.168.5{k}.1"] = "abcdefghij"[k]


def ph(t):
    for n, a, b in PH:
        if a <= t < b:
            return n
    return None


def pct(v, q):
    if not v:
        return float("nan")
    v = sorted(v)
    return v[min(len(v) - 1, int(q * (len(v) - 1) + 0.5))]


out = []
P = lambda *a: out.append(" ".join(str(x) for x in a))
# 1. light latency per class per phase (samples are probe-relative; the probe starts ~1-3 s after t0)
lat = collections.defaultdict(lambda: collections.defaultdict(list))
fails = collections.Counter()
for line in open(f"{R}/samples.txt"):
    try:
        c, t, v = line.split()
    except ValueError:
        continue
    p = ph(float(t))
    if p is None:
        continue
    if c == "F":
        fails[p] += 1
    else:
        lat[p][c].append(float(v))
seqs = set()
for line in open(f"{R}/ping.txt"):
    m = re.search(r"icmp_seq=(\d+).*time=([0-9.]+)", line)
    if m:
        seqs.add(int(m.group(1)))
        p = ph(int(m.group(1)) * 0.2)
        if p:
            lat[p]["p"].append(float(m.group(2)))
pings_sent = collections.Counter()
pings_got = collections.Counter()
if seqs:
    for s in range(1, max(seqs) + 1):
        p = ph(s * 0.2)
        if p:
            pings_sent[p] += 1
            pings_got[p] += s in seqs
hv = collections.defaultdict(list)
for f in glob.glob(f"{R}/hv-*.json"):
    m = re.search(r"hv-([du])-(\d+)", f)
    d, ts = m.group(1), int(m.group(2))
    try:
        j = json.load(open(f))
        for iv in j.get("intervals", []):
            s = iv["sum"]
            t = ts + (s["start"] + s["end"]) / 2 + (0 if d == "d" else 41)
            p = ph(t)
            if p:
                hv[(p, d)].append(s["bits_per_second"] / 1e6)
    except Exception:
        hv[("err", d)].append(0)
P("== light latency (ms) and failures per phase; c=crowd k=call g=game p=hs0 ping; heavy Mbit/s mean")
P(f"{'phase':9} {'c_n':>7} {'c50':>6} {'c99':>6} {'k50':>6} {'k99':>6} {'g50':>6} {'g99':>6} {'p50':>6} {'p99':>6} {'ploss%':>6} {'cfail':>5} {'down':>6} {'up':>6}")
for n, a, b in PH:
    L = lat[n]
    f = lambda c, q: pct(L[c], q)
    pl = 100 * (1 - pings_got[n] / pings_sent[n]) if pings_sent[n] else float("nan")
    hd = sum(hv[(n, 'd')]) / len(hv[(n, 'd')]) if hv[(n, 'd')] else float("nan")
    hu = sum(hv[(n, 'u')]) / len(hv[(n, 'u')]) if hv[(n, 'u')] else float("nan")
    P(f"{n:9} {len(L['c']):7d} {f('c',.5):6.1f} {f('c',.99):6.1f} {f('k',.5):6.1f} {f('k',.99):6.1f} {f('g',.5):6.1f} {f('g',.99):6.1f} {f('p',.5):6.1f} {f('p',.99):6.1f} {pl:6.2f} {fails[n]:5d} {hd:6.0f} {hu:6.0f}")
P("heavy errors:", len(hv[("err", "d")]), len(hv[("err", "u")]))
# 2. LB state per path per phase (exit = dialing side = the judge: kh lb_paths)
st = [json.loads(l) for l in open(f"{R}/st.jsonl") if l.strip()]
P("\n== exit (judge) per path per phase: links/target avg, min factor, seconds not ok, users avg")
rows = collections.defaultdict(lambda: collections.defaultdict(list))
timeline = collections.defaultdict(list)
for s in st:
    t = s["t"]
    p = ph(t)
    for q in s.get("kh", {}).get("lb_paths") or []:
        nm = PATH.get(q["remote"], q["remote"])
        f = q.get("auto_factor", 1)
        stt = q.get("state", "ok")
        timeline[nm].append((t, f, stt, q.get("links", 0), q.get("target", 0), q.get("users", 0), q.get("loss_down_pct", 0), q.get("auto_reason", "")))
        if p:
            rows[p][nm].append((q.get("links", 0), q.get("target", 0), f, stt, q.get("users", 0)))
P(f"{'phase':9} " + " ".join(f"{x:>21}" for x in "abcdefghij"))
for n, a, b in PH:
    cells = []
    for x in "abcdefghij":
        v = rows[n][x]
        if not v:
            cells.append(f"{'-':>21}")
            continue
        l = sum(r[0] for r in v) / len(v)
        tg = sum(r[1] for r in v) / len(v)
        mf = min(r[2] for r in v)
        cut = 5 * sum(1 for r in v if r[3] != "ok")
        u = sum(r[4] for r in v) / len(v)
        cells.append(f"{l:3.0f}/{tg:3.0f} f{mf:.2f} x{cut:3d} u{u:4.0f}".rjust(21))
    P(f"{n:9} " + " ".join(cells))
# 3. detection / restore per event
P("\n== event response (exit judge, 5 s samples): first lowered / not-ok after start, restored after clear")


def first(nm, a, b, cond):
    for r in timeline[nm]:
        if a <= r[0] < b and cond(r):
            return r[0] - a
    return None


for ev, path, a, clr in [("a_loss", "c", 150, 330), ("b_queue", "d", 390, 540), ("c_black", "e", 600, 720),
                         ("d_flap", "f", 810, 1050), ("e_rst", "g", 1110, 1230), ("f_mild", "*", 1290, 1440)]:
    if path == "*":
        low = {x: first(x, a, clr, lambda r: r[1] < 0.99 or r[2] != "ok") for x in "abcdefghij"}
        P(f"{ev:8} all paths: first lowered per path {low}")
        continue
    low = first(path, a, clr, lambda r: r[1] < 0.99 or r[2] != "ok")
    cut = first(path, a, clr, lambda r: r[2] != "ok")
    rest = first(path, clr, 1860, lambda r: r[1] >= 0.99 and r[2] == "ok")
    others = sorted({x for x in "abcdefghij" if x != path for r in timeline[x] if a <= r[0] < clr + 30 and (r[1] < 0.99 or r[2] != "ok")})
    ul = [r[5] for r in timeline[path] if a + 30 <= r[0] < clr]
    ub = [r[5] for r in timeline[path] if 60 <= r[0] < 150]
    P(f"{ev:8} path {path}: lowered +{low}s not-ok +{cut}s restored +{rest}s after clear; users on it base {sum(ub)/max(1,len(ub)):.0f} -> during {sum(ul)/max(1,len(ul)):.0f}; other paths lowered/not-ok: {others or 'none'}")
P("\n== per path judge transitions (t:factor/state/links-target/users/loss/reason), changes only")
for x in "abcdefghij":
    prev = None
    ch = []
    for r in timeline[x]:
        key = (round(r[1], 2), r[2])
        if key != prev:
            ch.append(f"{r[0]}:{r[1]:.2f}/{r[2]}/{r[3]}-{r[4]}/u{r[5]}/l{r[6]}/{r[7]}")
            prev = key
    P(f"{x}: " + "  ".join(ch))
# 4. kernel ground truth
P("\n== kernel ss (exit link sockets by Iran IP) per phase: sockets avg, retrans/s, rtt avg, bytes_sent share")
lines = open(f"{R}/ss.txt").read().split("\n")
samples = []
t = None
cur = {}
for i, l in enumerate(lines):
    if l.startswith("@T"):
        if t is not None:
            samples.append((t, cur))
        t = int(l.split()[1])
        cur = {}
        continue
    m = re.match(r"\s*\S+\s+\S+\s+(\S+)\s+(\S+)", l)
    if m and m.group(2).endswith(":2096"):
        peer = m.group(2).rsplit(":", 1)[0].strip("[]")
        info = lines[i + 1] if i + 1 < len(lines) else ""
        rt = re.search(r"\brtt:([0-9.]+)", info)
        rr = re.search(r"retrans:\d+/(\d+)", info)
        ba = re.search(r"bytes_sent:(\d+)", info)
        cur[(m.group(1), peer)] = (PATH.get(peer, peer), float(rt.group(1)) if rt else 0, int(rr.group(1)) if rr else 0, int(ba.group(1)) if ba else 0)
if t is not None:
    samples.append((t, cur))
agg = collections.defaultdict(lambda: collections.defaultdict(lambda: [0, 0, 0.0, 0, 0]))
for (ta, A), (tb, B) in zip(samples, samples[1:]):
    p = ph(tb)
    if not p:
        continue
    for k, (nm, rt, rr, bs) in B.items():
        g = agg[p][nm]
        g[1] += 1
        g[2] += rt
        if k in A:
            g[3] += max(0, rr - A[k][2])
            g[4] += max(0, bs - A[k][3])
    for nm in set(v[0] for v in B.values()):
        agg[p][nm][0] += 1
P(f"{'phase':9} " + " ".join(f"{x:>19}" for x in "abcdefghij"))
for n, a, b in PH:
    tot = sum(agg[n][x][4] for x in "abcdefghij") or 1
    cells = []
    for x in "abcdefghij":
        g = agg[n][x]
        if not g[0]:
            cells.append(f"{'-':>19}")
            continue
        cells.append(f"{g[1]/g[0]:3.0f}s {g[3]/(b-a):5.0f}r/s {g[2]/max(1,g[1]):3.0f}ms {100*g[4]/tot:3.0f}%".rjust(19))
    P(f"{n:9} " + " ".join(cells))
# 5. logs
d0 = datetime.datetime.fromtimestamp(t0, datetime.timezone.utc)
s0 = d0.hour * 3600 + d0.minute * 60 + d0.second


def tlog(line):
    m = re.match(r"(\d\d):(\d\d):(\d\d\.\d+)", line)
    if not m:
        return None
    return round(int(m.group(1)) * 3600 + int(m.group(2)) * 60 + float(m.group(3)) - s0, 1)


P("\n== lb auto2 / rescue / call / flap log events (t = s from t0)")
resc = {}
for side in ("kh", "ir"):
    lf = f"{R}/{side}.log"
    if not os.path.exists(lf):
        lf = f"{R}/{os.path.basename(R)}-{side}.log"
    if not os.path.exists(lf):
        continue
    cnt = collections.Counter()
    for l in open(lf, errors="replace"):
        if re.search(r"panic|fatal error|goroutine \d+ \[|data race", l):
            P(f"[{side}] PANIC? {l.strip()[:200]}")
        if re.search(r"lb auto2|rescue|call late|call stuck|flap|drain", l):
            k = re.sub(r"[0-9a-f:.>]{4,}|\d+", "#", l[16:80])
            cnt[k] += 1
            if re.search(r"lb rescue", l):
                if "cut-drain" in l or "census" in l:
                    pass
                ph_ = ph(tlog(l) or -1)
                if ph_:
                    resc[(side, ph_, l[16:45].strip())] = l[16:].strip()
            elif not re.search(r"spare dial|rebalance", l):
                P(f"[{side}] {tlog(l)} {l[16:].strip()[:240]}")
    P(f"[{side}] message kinds: " + " | ".join(f"{v}x {k.strip()}" for k, v in cnt.most_common(30)))
P("\n== last lb rescue / cut-drain line per phase (cumulative counters)")
for k in sorted(resc, key=lambda k: (k[0], [n for n, _, _ in PH].index(k[1]), k[2])):
    P(f"[{k[0]}] {k[1]:9} {resc[k][:330]}")
# 6. proc
P("\n== rss MB / goroutines / fds at each phase end")
pr = collections.defaultdict(dict)
for l in open(f"{R}/proc.txt"):
    m = re.match(r"(\d+) (\w+) pid=(\d+) rss_kb=(\d+) gor=(\d*) fds=(\d+)", l)
    if m:
        pr[m.group(2)][int(m.group(1))] = (m.group(3), int(m.group(4)) // 1024, m.group(5), int(m.group(6)))
for side in ("ir", "kh"):
    row = []
    for n, a, b in PH:
        ks = [k for k in pr[side] if a <= k < b]
        if ks:
            v = pr[side][max(ks)]
            row.append(f"{n}:{v[1]}MB/{v[2]}g/{v[3]}fd")
    P(f"{side}: " + "  ".join(row))
try:
    r = json.loads(open(f"{R}/result.json").read())
    pb = r.get("probe") or {}
    P("\n== run.sh result: link_churn", r.get("link_churn"), "links_max", r.get("links_max"), "lb_paths_end", r.get("lb_paths_end"),
      "crowd", {k: pb.get("crowd", {}).get(k) for k in ("conns", "open", "fail", "n", "p50", "p99")},
      "call", {k: (pb.get("call") or {}).get(k) for k in ("error", "p50", "p99", "lost")}, "game", {k: (pb.get("game") or {}).get(k) for k in ("error", "p50", "p99")})
except Exception as e:
    P("result.json:", e)
# 7. light latency while our own heavy download starts (first 15 s; the stripes
# have not spread yet and one path carries it all: lbv81 finding 2) vs the
# rest of the same heavy download vs no heavy at all, over the clean phases.
CLEAN = {"base", "a_rec", "c_rec", "e_rec", "f_rec", "h_clean", "d_rec", "b_rec"}
win = collections.defaultdict(lambda: collections.defaultdict(list))
for line in open(f"{R}/samples.txt"):
    try:
        c, t, v = line.split()
        t = float(t)
    except ValueError:
        continue
    if c == "F" or (ph(t) not in CLEAN and not os.environ.get("SHORT")):
        continue
    k = (t - 60) % 120 if t >= 60 else 999
    w = "start0-15" if k < 15 else ("down15-40" if k < 40 else ("up40-66" if k < 66 else "noheavy"))
    win[w][c].append(float(v))
P("\n== light latency (ms) by heavy window, clean phases only (finding 2)")
for w in ("noheavy", "start0-15", "down15-40", "up40-66"):
    P(f"{w:10} " + " ".join(f"{c}: n{len(win[w][c])} p50 {pct(win[w][c], .5):.1f} p99 {pct(win[w][c], .99):.1f}" for c in "ckg"))
print("\n".join(out))
