# deaths.py ARMDIR: each in-run call DIED (el < TT-5) with its window, error, lastrx_ago (stalled = rx idle > 3 s
# before the death: a link-stuck close, not a rescue decision) and the rescue-rst trace lines (HS2_MCONN_RESCUE_TRACE)
# on both sides within [-1.5 s, +0.3 s] of the death (wall clock utc), counted by kind.
import json, re, sys, gzip, collections, datetime
A = sys.argv[1]
TT = int(re.search(r"TT=(\d+)", open(A + "/env.txt").read()).group(1))
ST = [(0, "pre"), (20, "ip1"), (90, "rst"), (160, "ip2x"), (230, "flap"), (340, "loss_all"), (495, "post")]
def win(el):
    x = el % 500; n = "pre"
    for a, k in ST:
        if x >= a: n = k
    return n
def sec(hms):
    h, m, s = hms.split(":"); return int(h) * 3600 + int(m) * 60 + float(s)
tr = []
for side in ("ir", "kh"):
    for f in (A + "/full/%s.log" % side,):
        try: fh = open(f, errors="replace")
        except OSError: fh = gzip.open(A + "/logs/%s.nostripe.log.gz" % side, "rt", errors="replace")
        for l in fh:
            if " rst " in l and re.match(r"\d\d:\d\d:\d\d\.\d+ ", l):
                t = sec(l.split()[0]); k = re.sub(r"^\S+ \S+ ", "", l.strip())
                k = re.sub(r"[0-9a-f]{6,} ", "", k); k = re.sub(r"\d+", "N", k)[:70]
                tr.append((t, side, k, l.strip()[:200]))
print("trace rst lines:", len(tr), collections.Counter((s, k) for _, s, k, _ in tr).most_common(12))
n = 0
for l in open(A + "/calls.jsonl"):
    d = json.loads(l)
    if d["ev"] != "DIED" or d["el"] >= TT - 5: continue
    n += 1; t = sec(d["utc"])
    near = [(round(x - t, 2), s, k) for x, s, k, _ in tr if -1.5 <= x - t <= 0.3]
    print("%5.1f %-8s i=%-2d lastrx_ago=%-5s %s %-40s near=%s" % (d["el"], win(d["el"]), d["i"], d.get("lastrx_ago"),
          "STALLED" if (d.get("lastrx_ago") or 0) > 3 else "flowing", d.get("err", "")[:40],
          collections.Counter((s, k) for _, s, k in near).most_common(4)))
print("in-run deaths", n)
