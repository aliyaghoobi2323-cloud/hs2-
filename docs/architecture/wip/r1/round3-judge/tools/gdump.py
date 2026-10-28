#!/usr/bin/env python3
"""gdump.py LOG [N]: the goroutines of a Go SIGQUIT dump grouped by their
state and their top frames of hs2's code (engine., smux., tlscarrier.; else
the top two frames), the N (12) largest groups."""
import collections
import re
import sys

text = open(sys.argv[1], errors="replace").read()
n = int(sys.argv[2]) if len(sys.argv) > 2 else 12
blocks = re.split(r"\n(?=goroutine \d+ )", text)
groups = collections.Counter()
for b in blocks:
    m = re.match(r"goroutine \d+ [^\[]*\[([^\]]+)\]", b)
    if not m:
        continue
    state = m.group(1).split(",")[0]
    frames = []
    for line in b.split("\n")[1:]:
        if not line or line.startswith("\t") or line.startswith("created by"):
            continue
        frames.append(line.rsplit("(", 1)[0].split("/")[-1])
    ours = [f for f in frames if re.search(r"^(engine|smux|tlscarrier)\.", f)]
    if not ours:
        ours = frames[:2]
    groups[(state, " < ".join(ours[:3]))] += 1
for (state, sig), c in groups.most_common(n):
    print("%5d [%s] %s" % (c, state, sig))
