#!/usr/bin/env python3
# mkusers.py N SEED T: N heavy users, ~50% fast dl, ~30% LTE dl (50-150 Mbit), ~20% upload,
# 1-8 streams, staggered starts in [1, T/3], durations 6..T-start-2 s (churn)
import random, sys
n, seed, T = int(sys.argv[1]), int(sys.argv[2]), int(sys.argv[3])
r = random.Random(seed)
for i in range(1, n + 1):
    x = r.random(); kind = 'f' if x < .5 else ('l' if x < .8 else 'u')
    st = r.choice([1, 1, 2, 4, 4, 8]) if kind != 'u' else r.choice([1, 2, 4])
    start = round(r.uniform(1, T / 3), 1)
    dur = r.randint(6, max(6, int(T - start - 3)))
    print(i, kind, st, start, dur, '%dmbit' % r.choice([50, 80, 100, 150]))
