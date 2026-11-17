#!/bin/bash
# soakview99.sh [O]: soak99 at a glance. Per hour and side: RSS min/max (MB), goroutines, fds,
# tcp sockets max, raw-ring KB max / last and records-ring KB max / last; rescue counters (total,
# and added per hour); crowd fails + crowd p99 per hour; fails per named event window (events.txt,
# first-hour detail + average per event type); heavy; links; panic-like alarms; disk.
set -u
O=${1:-${O:-/opt/hs2lab/soak99/run}}
cat $O/env.txt; [ -e $O/DONE ] && cat $O/DONE
echo "unit: $(systemctl is-active soak99 2>/dev/null)  disk: $(tail -1 $O/df.txt 2>/dev/null)  out: $(du -sh $O | cut -f1)"
python3 - "$O" <<'PY'
import os, re, sys
O = sys.argv[1]
def rd(f):
    try: return open(os.path.join(O, f)).read().splitlines()
    except Exception: return []
# proc.txt: t side pid= rss_kb= gor= fds= tcp= estab= tcp2096= udp=
P = {}
for ln in rd("proc.txt"):
    p = ln.split()
    if len(p) < 4 or "MISSING" in ln: continue
    kv = dict(x.split("=", 1) for x in p[2:] if "=" in x)
    P.setdefault(int(p[0]) // 3600, {}).setdefault(p[1], []).append(kv)
miss = sum(1 for l in rd("proc.txt") if "MISSING" in l)
pids = {}
for ln in rd("proc.txt"):
    m = re.match(r"\d+ (\w+) pid=(\d+)", ln)
    if m: pids.setdefault(m.group(1), set()).add(m.group(2))
# resc.txt: t side mconn rescue: rings N (K KB) ... | mconn rescue-rec: conns N (K KB) ...
RX = re.compile(r"rings (\d+) \((\d+) KB\) · triggers (\d+) · started (\d+) acked (\d+) served (\d+) · nocover (\d+) failed (\d+) limited (\d+) overcap (\d+).*?nak (\d+) nakd (\d+)")
RR = re.compile(r"rescue-rec: conns (\d+) \((\d+) KB\) · started (\d+) acked (\d+) nakd (\d+) · served (\d+) nak (\d+) refused (\d+) · nocover (\d+) failed (\d+) lostjoin (\d+) endwait (\d+)")
R = {}
for ln in rd("resc.txt"):
    p = ln.split(" ", 2)
    if len(p) < 3: continue
    a, b = RX.search(ln), RR.search(ln)
    if not a or not b: continue
    R.setdefault(int(p[0]) // 3600, {}).setdefault(p[1], []).append((int(p[0]), [int(x) for x in a.groups()], [int(x) for x in b.groups()]))
def mx(v, k):
    v = [int(x[k]) for x in v if x.get(k, "").isdigit()]; return (min(v), max(v)) if v else (0, 0)
print("== per hour (RSS MB min/max · gor min/max · fds max · tcp max · rawKB max/last · recKB max/last · rings max)")
for h in sorted(P):
    for s in ("ir", "kh"):
        v = P[h].get(s, [])
        if not v: continue
        r, g, f, t = mx(v, "rss_kb"), mx(v, "gor"), mx(v, "fds"), mx(v, "tcp")
        rv = R.get(h, {}).get(s, [])
        raw = [x[1][1] for x in rv]; rec = [x[2][1] for x in rv]; rn = [x[1][0] for x in rv]
        print("h%d %s  rss %4d/%4d  gor %5d/%5d  fds %5d  tcp %5d  raw %6s/%-6s rec %6s/%-6s rings %s  (n=%d)" % (h, s, r[0] // 1024, r[1] // 1024, g[0], g[1], f[1], t[1],
              max(raw) if raw else "-", raw[-1] if raw else "-", max(rec) if rec else "-", rec[-1] if rec else "-", max(rn) if rn else "-", len(v)))
print("== missing samples: %d   hs2 pids seen: %s" % (miss, {k: len(v) for k, v in pids.items()}))
names = ["rings", "ringKB", "triggers", "started", "acked", "served", "nocover", "failed", "limited", "overcap", "nak", "nakd"]
rn = ["conns", "KB", "started", "acked", "nakd", "served", "nak", "refused", "nocover", "failed", "lostjoin", "endwait"]
print("== rescue counters: total (last sample) and per hour added")
for s in ("ir", "kh"):
    allv = [x for h in sorted(R) for x in R[h].get(s, [])]
    if not allv: print(s, "no rescue line yet"); continue
    l = allv[-1]
    print("%s RAW  %s" % (s, " ".join("%s=%d" % (n, v) for n, v in zip(names, l[1]) if n not in ("rings", "ringKB"))))
    print("%s REC  %s" % (s, " ".join("%s=%d" % (n, v) for n, v in zip(rn, l[2]) if n not in ("conns", "KB"))))
    prev = None
    for h in sorted(R):
        v = R[h].get(s, [])
        if not v: continue
        e = v[-1]; b = prev or (0, [0] * 12, [0] * 12)
        print("   h%d +raw started %d acked %d served %d nocover %d | +rec started %d acked %d served %d nak %d nocover %d failed %d" % (h,
              e[1][3] - b[1][3], e[1][4] - b[1][4], e[1][5] - b[1][5], e[1][6] - b[1][6], e[2][2] - b[2][2], e[2][3] - b[2][3], e[2][5] - b[2][5], e[2][6] - b[2][6], e[2][8] - b[2][8], e[2][9] - b[2][9]))
        prev = e
print("== pool trend every 5 min (t: ir rawKB/recKB/recConns  kh rawKB/recKB/recConns)")
T = {}
for h in R:
    for s in R[h]:
        for t, a, b in R[h][s]: T.setdefault(t, {})[s] = "%d/%d/%d" % (a[1], b[1], b[0])
for t in sorted(T)[::5]: print("   %5d  ir %-18s kh %s" % (t, T[t].get("ir", "-"), T[t].get("kh", "-")))
# light: min.txt = minute crowd_n p50 p99 fails call99 game99 (probe seconds ~ soak t)
M = []
for ln in rd("min.txt"):
    p = ln.split()
    if len(p) == 7: M.append(p)
print("== light per hour: crowd fails, crowd p99 (median of per-minute p99 / max), call p99 max, game p99 max")
H = {}
for p in M: H.setdefault(int(p[0]) // 60, []).append(p)
fl = lambda x: float(x) if x != "-" else None
for h in sorted(H):
    v = H[h]; c99 = sorted(x for x in (fl(p[3]) for p in v) if x is not None)
    k = [x for x in (fl(p[5]) for p in v) if x is not None]; g = [x for x in (fl(p[6]) for p in v) if x is not None]
    print("h%d fails %5d  crowd p99 med %6s max %6s  call p99 max %6s  game p99 max %6s  (min=%d)" % (h, sum(int(p[4]) for p in v),
          c99[len(c99) // 2] if c99 else "-", c99[-1] if c99 else "-", max(k) if k else "-", max(g) if g else "-", len(v)))
F = [float(x) for x in rd("fails.txt") if x.strip()]
E = [l.split() for l in rd("events.txt") if len(l.split()) == 3]
tmax = max(F + [float(p[0]) * 60 + 60 for p in M] + [0])
print("== fails per event window (window = event + 60 s tail for blackholes); detail for t < 3600")
by = {}
for a, b, n in E:
    a, b = int(a), int(b)
    if a > tmax: continue
    k = sum(1 for s in F if a <= s < b)
    if b <= tmax: by.setdefault(n, []).append(k)
    if a < 3600: print("   %5d-%5d %-9s fails %5d%s" % (a, b, n, k, "" if b <= tmax else " (running)"))
for n, v in sorted(by.items()):
    print("   %-9s n=%2d  avg %7.1f  min %5d  max %5d" % (n, len(v), sum(v) / len(v), min(v), max(v)))
inw = sum(1 for s in F if any(int(a) <= s < int(b) for a, b, _ in E))
print("   fails total %d, outside any window %d" % (len(F), len(F) - inw))
PY
echo "== heavy (last 4 of $(cat $O/heavy.txt 2>/dev/null | wc -l))"; tail -4 $O/heavy.txt 2>/dev/null
echo "== links (t ir kh, last 3)"; tail -3 $O/st.jsonl 2>/dev/null | python3 -c 'import sys,json
for l in sys.stdin:
  d=json.loads(l); print(d["t"], d["ir"].get("links"), d["kh"].get("links"), d["ir"].get("users"))'
echo "== alarms: $(cat $O/alarm.txt 2>/dev/null | wc -l)"; tail -3 $O/alarm.txt 2>/dev/null
