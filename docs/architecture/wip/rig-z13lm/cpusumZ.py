# cpusumZ.py: A9 cpubench summary: per stream count and variant, median [runs] of CPU s per GiB,
# Gbit/s, and the ratio to base (B); the pool left at the end and the largest window/extra seen.
import json, statistics as st, os, sys
f = os.path.join(os.path.dirname(os.path.abspath(__file__)), "res", "A9-cpubench.jsonl")
R = [json.loads(l) for l in open(f) if l.strip()]
by = {}
for r in R:
    by.setdefault(r["streams"], {}).setdefault(r["v"], []).append(r)
for s in sorted(by):
    b = st.median(x["cpu_s_per_gib"] for x in by[s].get("B", [])) if "B" in by[s] else None
    for v in ("B", "N", "C", "C0"):
        xs = by[s].get(v, [])
        if not xs:
            continue
        c = [x["cpu_s_per_gib"] for x in xs]
        g = [x["gbit"] for x in xs]
        m = st.median(c)
        print(f"streams {s} {v:3} cpu s/GiB {m:.3f} [{' '.join(f'{x:.3f}' for x in c)}]  x base {m/b:.2f}  Gbit/s {st.median(g):.2f}  "
              f"pool left {sorted(set(x['pool_left_mib'] for x in xs))} MiB  ext max {max(x['ext_max_mib'] for x in xs)} MiB  "
              f"core win max {max(x.get('core_win_max_mib', 0) for x in xs):.1f} MiB")
