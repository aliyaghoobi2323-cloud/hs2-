#!/usr/bin/env python3
"""gen_extra.py OUTDIR: the second part of the same rig block (round 2, step
1), added after the first runs showed (a) TCP_WINDOW_CLAMP set on an open
link socket does not hold (the peer still saw a window of 321 KB where the
clamp was 52 KB: autotuning raises the clamp with the buffer), and (b)
with the queue gone (pacing 0.95 x), the slowest probes wait out BBR's
PROBE_RTT (cwnd 4). So:

  acw*    a with the sender's cwnd clamped to design B's budget
          BDP x (1 + 15 ms / RTT) in packets (route "cwnd N lock" in both
          hosts' namespaces, before the link opens: snd_cwnd_clamp)
  apinp*  the probe on a link of its own (N=2) AND every link paced at
          0.95 x bottleneck (beta with alpha's cap on the bulk link)
  acub    a with CUBIC on the link sockets (HS2_TUNE_CC=cubic): no PROBE_RTT
"""
import hashlib, os, sys

out = sys.argv[1]
A = "/root/r1-bins/hs2-diag-a"
PACE = {5: 4.706, 25: 23.53, 100: 94.12}
CWND = {5: 36, 25: 180, 100: 718}      # ceil(BDP payload x (1 + 15/72) / 1448)
PFIFO = {5: 120, 25: 600, 100: 2400}
bins = [("acub", A, "HS2_TUNE_CC=cubic")]
for r in (5, 25, 100):
    bins.append(("apinp%d" % r, A, "HS2_LAB_PIN_PORT=5312 HS2_LAB_PACE_MBIT=%s" % PACE[r]))
    bins.append(("acw%d" % r, A, ""))
with open(os.path.join(out, "bins-x.txt"), "w") as f:
    for l, p, e in bins:
        f.write(("%s %s %s" % (l, p, e)).rstrip() + "\n")

def row(cell, b, k):
    h = hashlib.sha1(cell.encode()).hexdigest()[:8]
    return "%s-%s-%d\t%s\tA\t%d\t%s" % (h, b, k, b, k, cell)

def s5(n, rate, mode, extra=""):
    return ("N=%d RATE=%d RTT=72 LOSS=0 MODE=%s USERS=7 MIX=s5 DUR=20 %s" % (n, rate, mode, extra)).strip()

for name, mode_of in (("x-fq", lambda r: "fqcodel"), ("x-pf", lambda r: "pfifo:%d" % PFIFO[r])):
    rows = []
    for rate in (5, 25, 100):
        m = mode_of(rate)
        cw = s5(1, rate, m, "CWNDLOCK=%d" % CWND[rate])
        cu = s5(1, rate, m)
        pn = s5(2, rate, m)
        for k in (1, 2):
            seq = [(cw, "acw%d" % rate), (cu, "acub"), (pn, "apinp%d" % rate)]
            for cell, b in (seq if k == 1 else seq[::-1]):
                rows.append(row(cell, b, k))
    open(os.path.join(out, name + ".tsv"), "w").write("\n".join(rows) + "\n")
    print(name, len(rows))
