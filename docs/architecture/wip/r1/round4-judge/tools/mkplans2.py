#!/usr/bin/env python3
"""mkplans2.py: the judge's second regression for the final candidate fin2
(it differs from fin only in pools of up to five links): the cells of up to
five links again with fin2, next to c4 where production / j3g / c4 / fin
ran today already (G1, G1b, G2, G5, G9, G10), and with all four labels
where nothing ran yet (loss at N=1, N=2, deep FIFO); the cells of eight
links and more ran with fin (= fin2 there) and continue as planned."""
ALL = ["prod", "j3g", "c4", "fin2"]
TWO = ["c4", "fin2"]
P = {
    "H1": ("P10 at N=1 (a) lone16, (b)/(c) one_vs_16; fin2 next to c4 (prod/j3g/c4/fin: G1, G1b)", TWO, [
        "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=lone16 DUR=30",
        "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=30"]),
    "H2": ("P10 (c)/(e) newheavy, home and 25 x 16 at N=1; fin2 next to c4 (G1b, G2, G5, G9)", TWO, [
        "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=newheavy DUR=40",
        "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=64 MIX=home SBR=1.2",
        "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=50 MIX=gamer_vs_dl DUR=20"]),
    "H3": ("loss 0.5 / 1 / 2 % at N=1", ALL, [
        "N=1 RATE=100 RTT=72 LOSS=%s MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=20" % l for l in ("0.5", "1", "2")]),
    "H4": ("latency at N=2; deep FIFO (report)", ALL, [
        "N=2 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=30",
        "N=1 RATE=25 RTT=72 LOSS=0 MODE=pfifo:600 USERS=7 MIX=s5 DUR=20"]),
    "H5": ("the TUN ping at N=8 (fin = fin2 there); the busiest QUIET killed at N=1 (fin2 next to c4; G10)", None, [
        ("N=8 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=tunping DUR=30", ["prod", "j3g", "c4", "fin"]),
        ("N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=64 MIX=home SBR=1.2 KILLQ=15", TWO)]),
}
for name, (what, bins, cells) in P.items():
    with open("/root/r4j/plans/%s.txt" % name, "w") as f:
        f.write("# %s (regression, 3 reps): %s\n" % (name, what))
        for c in cells:
            if isinstance(c, tuple):
                c, bs = c
            else:
                bs = bins
            for b in bs:
                f.write("%s BIN=%s\n" % (c, b))
print(" ".join(P))
