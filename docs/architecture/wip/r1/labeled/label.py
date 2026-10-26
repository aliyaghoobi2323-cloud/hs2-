#!/usr/bin/env python3
# label.py OUT DEST: labels the flows of the labeled session (session.sh) and
# writes DEST (c1_labeled.jsonl.gz): flowcap's lines {"f","t","up","dn"} plus
# "want", the verdict the C1 rules give the pattern that produced the flow.
#
# A flow belongs to the pattern whose window (the earliest start to the latest
# end of its connections, from client.jsonl, in Unix ms) holds the flow's first
# line (flowcap_start_ms + t). Patterns run one after another with 5 s gaps, so
# a clock skew of a second between the two servers changes nothing.
#
# want (R1 global constraints, C1; decision t1: silence = < 8 KiB in 2 s):
#   bulkdn, bulkup  bulk  (continuous)
#   burst           bulk  20 KiB within 200 ms >= 16 KiB; its lines are its bursts
#   onoff           bulk  1 MiB/s for 1 s >= 16 KiB in 200 ms; the 2 s gap
#                         ends exactly when the next second on begins
#   http            light 600 B + 8 KiB per 1 s: < 16 KiB in 200 ms, < 64 KiB in 2 s
#   voip            light 160 B each way every 20 ms (8 kB/s)
# Lines exist only for ticks that moved bytes (flowcap), so a flow's silent
# ticks have no line; the replay fills them with zeros.
import gzip, io, json, sys

WANT = {"bulkdn": "bulk", "bulkup": "bulk", "burst": "bulk", "onoff": "bulk",
        "http": "light", "voip": "light"}

out, dest = sys.argv[1], sys.argv[2]
t0 = int(open(out + "/flowcap_start_ms").read().split()[-1])
win = {}
for l in open(out + "/client.jsonl"):
    r = json.loads(l)
    s, e = win.get(r["pat"], (r["start_ms"], r["end_ms"]))
    win[r["pat"]] = (min(s, r["start_ms"]), max(e, r["end_ms"]))
    if r.get("err"):
        print("client error: %s conn %d: %s" % (r["pat"], r["conn"], r["err"]))
lines = [json.loads(l) for l in gzip.open(out + "/flowcap.jsonl.gz", "rt")]
first = {}
for r in lines:
    first.setdefault(r["f"], r["t"])
pat = {}
for f, t in first.items():
    w = t0 + t
    hit = [p for p, (s, e) in win.items() if s - 2000 <= w <= e + 3000]
    pat[f] = hit[0] if len(hit) == 1 else None
res, stray = [], 0
for r in lines:
    p = pat[r["f"]]
    if p is None:
        stray += 1
        continue
    res.append({"f": r["f"], "t": r["t"], "up": r["up"], "dn": r["dn"], "want": WANT[p]})
res.sort(key=lambda r: r["t"])
buf = io.BytesIO()
with gzip.GzipFile(filename="", mode="wb", fileobj=buf, mtime=0, compresslevel=9) as g:
    for r in res:
        g.write((json.dumps(r, separators=(",", ":")) + "\n").encode())
open(dest, "wb").write(buf.getvalue())
print("lines %d (dropped %d outside every window), flows %d, %d bytes gz" % (len(res), stray, len(first), len(buf.getvalue())))
for p, (s, e) in sorted(win.items(), key=lambda x: x[1][0]):
    fl = [f for f in pat if pat[f] == p]
    ln = [r for r in res if pat[r["f"]] == p]
    print("  %-7s %-5s flows %d  lines %5d  up %10d B  dn %11d B  window %.1f s" % (
        p, WANT[p], len(fl), len(ln), sum(r["up"] for r in ln), sum(r["dn"] for r in ln), (e - s) / 1000))
