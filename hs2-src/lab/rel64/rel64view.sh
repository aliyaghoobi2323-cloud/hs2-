#!/bin/bash
# rel64view.sh [O]: the rel64 soak at a glance. Per hour and side: RSS min/max MB, goroutines, fds, tcp, links;
# rescue counters (raw started/acked/nak, records started/acked/nak/lostjoin, rescue145 retry/retryfail/retrysilent)
# total and per hour; keepstall counters (last); crowd fails per event window (each cycle, and mean per type);
# crowd p99 per hour; heavy (bg + test downloads); idle groups; alarms; disk.
set -u
O=${1:-${O:-/opt/hs2lab/rel64/soak}}
cat $O/env.txt; [ -e $O/DONE ] && cat $O/DONE
echo "unit: $(systemctl is-active rel64soak 2>/dev/null)  elapsed: $(( $(date +%s) - $(cat $O/t0 2>/dev/null || date +%s) )) s  disk: $(tail -1 $O/df.txt 2>/dev/null)  log: $(tail -2 $O/logsize.txt 2>/dev/null | tr '\n' ' ')"
python3 - "$O" <<'PY'
import os, re, sys, json, glob
O = sys.argv[1]
def rd(f):
    try: return open(os.path.join(O, f), errors="replace").read().splitlines()
    except Exception: return []
P = {}
for ln in rd("proc.txt"):
    p = ln.split()
    if len(p) < 4 or "MISSING" in ln: continue
    kv = dict(x.split("=", 1) for x in p[2:] if "=" in x)
    P.setdefault(int(p[0]) // 3600, {}).setdefault(p[1], []).append(kv)
def mm(v, k):
    v = [int(x[k]) for x in v if x.get(k, "").isdigit()]; return (min(v), max(v)) if v else (0, 0)
print("== per hour: RSS MB min/max · gor min/max · fds max · tcp max · tcp2096 max · links last")
for h in sorted(P):
    for s in ("ir", "kh"):
        v = P[h].get(s, [])
        if not v: continue
        r, g, f, t, l = mm(v, "rss_kb"), mm(v, "gor"), mm(v, "fds"), mm(v, "tcp"), mm(v, "tcp2096")
        print("h%d %s rss %4d/%4d gor %6d/%6d fds %5d tcp %5d 2096 %4d links %s (n=%d)" % (h, s, r[0] >> 10, r[1] >> 10, g[0], g[1], f[1], t[1], l[1], v[-1].get("links", "-"), len(v)))
print("== missing samples: %d" % sum(1 for l in rd("proc.txt") if "MISSING" in l))
def num(rx, s):
    m = re.search(rx, s); return int(m.group(1)) if m else -1
F = [("raw_started", r"mconn rescue: .*?started (\d+)"), ("raw_acked", r"mconn rescue: .*?acked (\d+)"),
     ("raw_lostjoin", r"mconn rescue: .*?lostjoin (\d+)"), ("raw_nak", r"mconn rescue: .*? nak (\d+) nakd"),
     ("rec_started", r"rescue-rec: .*?started (\d+)"), ("rec_acked", r"rescue-rec: .*?acked (\d+)"),
     ("rec_nak", r"rescue-rec: .*? nak (\d+)"), ("rec_lostjoin", r"rescue-rec: .*?lostjoin (\d+)"),
     ("rec_endwait", r"rescue-rec: .*?endwait (\d+)"), ("pick_fresh", r"rescue145: pick fresh (\d+)"),
     ("retry", r"· retry (\d+)"), ("retryfail", r"retryfail (\d+)"), ("retrysilent", r"retrysilent (\d+)"),
     ("rawKB", r"rings \d+ \((\d+) KB\)"), ("recKB", r"rescue-rec: conns \d+ \((\d+) KB\)")]
R = {"ir": [], "kh": []}
for ln in rd("resc.txt"):
    p = ln.split(" ", 2)
    if len(p) < 3 or p[1] not in R: continue
    R[p[1]].append((int(p[0]), {k: num(rx, p[2]) for k, rx in F}))
print("== rescue counters (cumulative at the last sample, then per hour added)")
for s in ("ir", "kh"):
    v = R[s]
    if not v: print(s, "no rescue line yet"); continue
    print("%s t=%d %s" % (s, v[-1][0], " ".join("%s=%d" % kv for kv in v[-1][1].items())))
    prev = {k: 0 for k, _ in F}; byh = {}
    for t, d in v: byh[t // 3600] = d
    for h in sorted(byh):
        d = byh[h]
        print("   h%d +" % h + " ".join("%s %d" % (k, d[k] - prev[k]) for k, _ in F if k not in ("rawKB", "recKB")) + "  rawKB %d recKB %d" % (d["rawKB"], d["recKB"]))
        prev = d
for s in ("ir", "kh"):
    k = rd("keep.txt"); k = [l for l in k if l.split(" ", 2)[1:2] == [s]]
    if k: print("== keepstall %s (t=%s): %s" % (s, k[-1].split()[0], re.sub(r".*keepstall: ", "", k[-1])[:400]))
# crowd fails per event window (fails.txt in probe seconds ~ soak seconds)
fails = []
for l in rd("fails.txt"):
    try: fails.append(float(l))
    except ValueError: pass
EV = [l.split() for l in rd("events.txt") if len(l.split()) == 3]
print("== crowd fails total %d; per event window (cycle: fails)" % len(fails))
agg = {}
for a, b, n in EV:
    a, b = int(a), int(b)
    if fails and a > max(fails) + 120: continue
    c = sum(1 for f in fails if a <= f < b); agg.setdefault(n, []).append((a // 1800, c))
for n, v in agg.items(): print("  %-9s %s  mean %.1f" % (n, " ".join("c%d:%d" % x for x in v), sum(x[1] for x in v) / len(v)))
inev = sum(1 for f in fails if any(int(a) <= f < int(b) for a, b, _ in EV))
print("  outside windows: %d" % (len(fails) - inev))
M = [l.split() for l in rd("min.txt")]
byh = {}
for m in M:
    if len(m) < 7: continue
    byh.setdefault(int(m[0]) // 60, []).append(m)
print("== per hour crowd: samples, p99 max of minute p99s, fails, call p99 max, game p99 max")
fl = lambda x: float(x) if x != "-" else 0
for h in sorted(byh):
    v = byh[h]
    print("  h%d n=%d crowd_p99max=%.0f med_p99=%.0f fails=%d call_p99max=%.0f game_p99max=%.0f" % (h, sum(int(x[1]) for x in v), max(fl(x[3]) for x in v), sorted(fl(x[3]) for x in v)[len(v) // 2], sum(int(x[4]) for x in v), max(fl(x[5]) for x in v), max(fl(x[6]) for x in v)))
bg = rd("bg.txt"); err = [l for l in bg if "mbit=ERR" in l]
print("== bg heavy runs %d, ERR %d%s" % (len(bg), len(err), (" last ERR: " + err[-1]) if err else ""))
ts = rd("tests.txt"); vals = [int(re.search(r"test_mbit=(\d+)", l).group(1)) for l in ts if re.search(r"test_mbit=\d+", l)]
if vals:
    sv = sorted(vals); print("== test downloads n=%d ERR %d mean %.0f p10 %d min %d last5 %s" % (len(ts), len(ts) - len(vals), sum(vals) / len(vals), sv[len(sv) // 10], sv[0], vals[-5:]))
for f in sorted(glob.glob(os.path.join(O, "idle-*.jsonl"))):
    for l in open(f): print("== idle", os.path.basename(f), l.strip()[:200])
for l in rd("alarm.txt")[-10:]: print("ALARM", l[:300])
PY
