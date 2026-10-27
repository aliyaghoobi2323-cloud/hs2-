#!/usr/bin/env python3
"""gen_sched.py OUTDIR: the round-2 diagnosis schedules (one run.sh schedule
per rig and phase, as ana.py schedule prints them) and the BINS file.

  q12-fq   Q1/Q2, fq_codel bottleneck: s5 at 5, 25, 100 Mbit/s, RTT 72:
           base, a (r1-judge), a paced at 0.95 x bottleneck (ap*), a with
           the window clamped at design B's budget BDP x (1 + 15 ms / RTT)
           (ac*), a with the probe on a link of its own (apin, N=2)
  q12-pf   the same on a deep drop-tail (pfifo, 4 x BDP packets)
  q3-1     lone16 (a, raw TCP), one_vs_16 (a, 8 MiB window, window core off,
           paced) at 100 Mbit/s, N=1, 200 users
  q3-2     newheavy (a, new flows' first bytes as bulk, 8 MiB window,
           paced) and Q4's s5 100/N8 (a, b)
  q4-solo  Q4's s5 1000/N1 (a, b), alone on the host
Configurations rotate within each cell (forward, backward, forward)."""
import hashlib, os, sys

out = sys.argv[1]
os.makedirs(out, exist_ok=True)
A = "/root/r1-bins/hs2-diag-a"
PACE = {5: 4.706, 25: 23.53, 100: 94.12}          # 0.95 x RATE x 1500/1514
CLAMP = {5: 52000, 25: 260000, 100: 1040000}       # BDP payload x (1 + 15/72)
PFIFO = {5: 120, 25: 600, 100: 2400}               # 4 x BDP, full packets
bins = [
    ("base", "/root/r1-bins/hs2-diag-base", ""),
    ("a", A, ""),
    ("b", "/root/r1-bins/hs2-diag-b", "HS2_TUNE_HQG_SEGS=8"),
    ("apin", A, "HS2_LAB_PIN_PORT=5312"),
    ("anb", A, "HS2_TUNE_SMUX_NEEDBUF=8388608 HS2_TUNE_SMUX_SESSBUF=33554432"),
    ("anw", A, "HS2_TUNE_WCORE=0"),
    ("an0", A, "HS2_LAB_NEWBYTES=0"),
]
for r in (5, 25, 100):
    bins.append(("ap%d" % r, A, "HS2_LAB_PACE_MBIT=%s" % PACE[r]))
    bins.append(("ac%d" % r, A, "HS2_LAB_WCLAMP=%d" % CLAMP[r]))
with open(os.path.join(out, "bins.txt"), "w") as f:
    for l, p, e in bins:
        f.write(("%s %s %s" % (l, p, e)).rstrip() + "\n")

def rows(cells, reps):
    """cells: [(cell string, [bins])]; each cell's bins forward, backward, ..."""
    res = []
    for cell, bs in cells:
        h = hashlib.sha1(cell.encode()).hexdigest()[:8]
        cnt = {}
        for k in range(reps):
            for b in (bs if k % 2 == 0 else bs[::-1]):
                cnt[b] = cnt.get(b, 0) + 1
                res.append("%s-%s-%d\t%s\tA\t%d\t%s" % (h, b, cnt[b], b, cnt[b], cell))
    return res

def s5(n, rate, mode, extra=""):
    return ("N=%d RATE=%d RTT=72 LOSS=0 MODE=%s USERS=7 MIX=s5 DUR=20 %s" % (n, rate, mode, extra)).strip()

def q12(mode_of):
    res = []
    for rate in (5, 25, 100):
        mode = mode_of(rate)
        # N=1 configurations and the pinned probe (N=2) as one rotation:
        # the pinned runs carry N=2 in their own cell, interleaved by hand
        n1 = rows([(s5(1, rate, mode), ["base", "a", "ap%d" % rate, "ac%d" % rate])], 3)
        n2 = rows([(s5(2, rate, mode), ["apin"])], 3)
        # put one pinned run after each 4 N=1 runs
        for k in range(3):
            res += n1[4 * k:4 * k + 4] + n2[k:k + 1]
    return res

open(os.path.join(out, "q12-fq.tsv"), "w").write("\n".join(q12(lambda r: "fqcodel")) + "\n")
open(os.path.join(out, "q12-pf.tsv"), "w").write("\n".join(q12(lambda r: "pfifo:%d" % PFIFO[r])) + "\n")
p10 = "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=%s HS2_LAB_TRACE_NOFRAMES=1"
q31 = rows([(p10 % "lone16", ["a", "raw"]), (p10 % "one_vs_16", ["a", "anb", "anw", "ap100"])], 2)
q32 = rows([(p10 % "newheavy", ["a", "an0", "anb", "ap100"]), (s5(8, 100, "fqcodel"), ["a", "b"])], 2)
open(os.path.join(out, "q3-1.tsv"), "w").write("\n".join(q31) + "\n")
open(os.path.join(out, "q3-2.tsv"), "w").write("\n".join(q32) + "\n")
solo = rows([(s5(1, 1000, "fqcodel"), ["a", "b"])], 2)
open(os.path.join(out, "q4-solo.tsv"), "w").write("\n".join(solo) + "\n")
smoke = rows([(s5(1, 25, "fqcodel", "SMOKE=1"), ["a"])], 1) + rows([(s5(1, 25, "fqcodel", "SMOKE=2"), ["base"])], 1)
open(os.path.join(out, "smoke1.tsv"), "w").write(smoke[0] + "\n")
open(os.path.join(out, "smoke2.tsv"), "w").write(smoke[1] + "\n")
for n in ("q12-fq", "q12-pf", "q3-1", "q3-2", "q4-solo"):
    print(n, sum(1 for _ in open(os.path.join(out, n + ".tsv"))))
