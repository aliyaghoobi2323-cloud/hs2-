#!/usr/bin/env python3
# mksoak.py SEED: bh95 soak heavy users over 1800 s in 240 s blocks; blocks 2, 5 idle (crowd only);
# busy blocks: 6-12 users starting in the first 100 s of the block, 10-60 s each (churn)
import random, sys
r = random.Random(int(sys.argv[1])); i = 0
for blk in range(7):
    if blk in (2, 5): continue
    for _ in range(r.randint(6, 12)):
        i += 1; x = r.random(); kind = 'f' if x < .5 else ('l' if x < .8 else 'u')
        st = r.choice([1, 1, 2, 4, 4, 8]) if kind != 'u' else r.choice([1, 2, 4])
        print(i, kind, st, round(blk * 240 + r.uniform(5, 100), 1), r.randint(10, 60), '%dmbit' % r.choice([50, 80, 100, 150]))
