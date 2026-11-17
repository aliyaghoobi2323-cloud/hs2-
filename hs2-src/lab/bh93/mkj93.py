#!/usr/bin/env python3
# mkj93.py: bh93 jobs, rep-major, arms interleaved (random order per pair): bL/sL n=6, q n=3, mixed, sQ n=3 (N=0)
import random
r = random.Random(93)
out = []
for k in range(1, 7):
    for sc in r.sample(['bL', 'sL'], 2):
        for a in r.sample(['S0', 'S1'], 2): out.append((f"r{k}{sc}{a}", a, 40, sc, 930 + k))
for k in range(1, 4):
    for a in r.sample(['S0', 'S1'], 2): out.append((f"r{k}q{a}", a, 40, 'q', 940 + k))
out += [("m1bLM1", 'M1', 40, 'bL', 951), ("m1sLM1", 'M1', 40, 'sL', 952), ("m1bLM2", 'M2', 40, 'bL', 951)]
for k in range(1, 4):
    for a in r.sample(['S0', 'S1'], 2): out.append((f"r{k}sQ{a}", a, 0, 'sQ', 960 + k))
for t in out: print(*t)
