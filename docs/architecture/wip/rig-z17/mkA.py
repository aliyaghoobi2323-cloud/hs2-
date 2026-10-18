# mkA.py REPS [REP0]: Part A plan (calibration with fixed windows): every fast stream pinned
# at a multiple of the path's BDP (HS2_TUNE_WCORE_FIXEDWIN via Z5_WC_FIXED), the window
# core's trace per run, the turnaround sampler (Z5_TURNS), the emulator trace (Z5_TRACE2).
# Interleaved: per repetition every cell runs all its windows, the windows' order rotated.
import sys
S = "/tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z11"
reps = int(sys.argv[1]); rep0 = int(sys.argv[2]) if len(sys.argv) > 2 else 1
cells = []
for path in [("300", "150", "50"), ("300", "150", "200"), ("1000", "100", "50")]:
    for loss in ["0", "0.1"]:
        for d in ["dl", "ul"]:
            cells.append((path, loss, d))
WIN = {"300": [0.8, 1, 1.2, 1.5, 2, 3], "1000": [0.4, 0.6, 0.8, 1, 1.2, 1.5, 2, 3]}
common = "Z5_POOL=128 Z5_MAXBUF=48 Z5_TURNS=1 Z5_TRACE=1 Z5_TRACE2=1 Z5_RELAY=1 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=20 Z5_MTU=1500"
for rep in range(rep0, rep0 + reps):
    for (mb, rtt, q), loss, d in cells:
        ws = WIN[mb]
        k = (rep - 1) % len(ws)
        for w in ws[k:] + ws[:k]:
            bdp = float(mb) * 1e6 / 8 * float(rtt) / 1000
            fixed = int(w * bdp)
            sid = f"{d}-{mb}-{rtt}-q{q}-l{loss}-w{w}"
            rid = f"A-{sid}-r{rep}"
            kind = "Z5_FAST=1" if d == "dl" else "Z5_UPL=1"
            print(f"{rid} core Z5_SCEN={sid} Z5_VER=w{w} Z5_REP={rep} {common} Z5_WC_FIXED={fixed} "
                  f"Z5_WC_TRACE={S}/res/tr/{rid}.jsonl Z5_DN_MBIT={mb} Z5_UP_MBIT={mb} Z5_RTT_MS={rtt} "
                  f"Z5_LOSS={float(loss)/100} Z5_QMS={q} {kind}")
