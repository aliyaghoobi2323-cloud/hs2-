#!/usr/bin/env python3
"""mkplans.py: the judge's regression plans (3 reps each, production / j3g /
c4 / the final candidate fin, interleaved by run.sh), as round 4's R1..R6
plus the QUIET-kill cells, split so that no block passes ~25 min."""
B = ["prod", "j3g", "c4", "fin"]
P = {
    "G1": ("P10 at N=1: (a) lone16, (b)/(c) one_vs_16", [
        "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=lone16 DUR=30",
        "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=30"]),
    "G2": ("P10 at N=1: (c)/(e) newheavy; stream_vs_dl at N=8 (video)", [
        "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=newheavy DUR=40",
        "N=8 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=50 MIX=stream_vs_dl SBR=1.2 DUR=20"]),
    "G3": ("loss 0.5 / 1 / 2 % at N=1", [
        "N=1 RATE=100 RTT=72 LOSS=%s MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=20" % l for l in ("0.5", "1", "2")]),
    "G4": ("loss 0.5 / 1 / 2 % at N=8", [
        "N=8 RATE=100 RTT=72 LOSS=%s MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=20" % l for l in ("0.5", "1", "2")]),
    "G5": ("25 x 16 next to 25 gamers at N=1 and N=8; the per-connection policer", [
        "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=50 MIX=gamer_vs_dl DUR=20",
        "N=8 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=50 MIX=gamer_vs_dl DUR=20",
        "N=8 RATE=200 RTT=72 LOSS=0 MODE=police:10+fqcodel USERS=200 MIX=lone16 DUR=30"]),
    "G6": ("latency on fq_codel at N=2 and N=8 (TUN ping); deep FIFO (report)", [
        "N=2 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=30",
        "N=8 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=tunping DUR=30",
        "N=1 RATE=25 RTT=72 LOSS=0 MODE=pfifo:600 USERS=7 MIX=s5 DUR=20"]),
    "G7": ("latency and throughput at N=64", [
        "N=64 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=30"]),
    "G8": ("latency and throughput at N=300", [
        "N=300 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=30"]),
    "G9": ("user classes: home on 8 links and on 1", [
        "N=8 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=64 MIX=home SBR=1.2",
        "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=64 MIX=home SBR=1.2"]),
    "G10": ("item 2: one QUIET link (production: its busiest link) reset 15 s into home at N=1 and N=8", [
        "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=64 MIX=home SBR=1.2 KILLQ=15",
        "N=8 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=64 MIX=home SBR=1.2 KILLQ=15"]),
}
for name, (what, cells) in P.items():
    with open("/root/r4j/plans/%s.txt" % name, "w") as f:
        f.write("# %s (regression, 3 reps, %s): %s\n" % (name, " / ".join(B), what))
        for c in cells:
            for b in B:
                f.write("%s BIN=%s\n" % (c, b))
print(" ".join(P))
