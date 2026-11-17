#!/usr/bin/env python3
# mkj95.py: bh95 event jobs, rep-major, arms interleaved (random order per pair): bI/bP n=6, bB n=4 per arm, N=20
import random
r = random.Random(95); out = []
for k in range(1, 7):
    scs = ['bI', 'bP'] + (['bB'] if k <= 4 else [])
    for sc in r.sample(scs, len(scs)):
        for a in r.sample(['S0', 'S1'], 2): out.append((f"e{k}{sc}{a}", a, 20, sc, 950 + k))
for t in out: print(*t)
