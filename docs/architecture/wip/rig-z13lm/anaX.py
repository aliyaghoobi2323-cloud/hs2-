# anaX.py: extra class A summaries -- the stall matrix's stalls and relays ended per version and
# kind, the interactive users of the multi-link and bad-link cells, load at the runs' starts.
import sys, os, collections, statistics as st, re
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from z11lib import load
from anaZ import metrics
print("== A6: stall seconds (bulk < 1 Mbit/s) and relays ended (guard), summed over runs, per kind: version (stall s, ended)")
agg = collections.defaultdict(lambda: [0, 0, 0, 0, 0])
k = collections.defaultdict(lambda: collections.defaultdict(list))
for d in load('A6'):
    m = metrics(d); v = d['ver']
    a = agg[v]; a[0] += 1; a[1] += m.get('stall', 0); a[2] += (m.get('stall', 0) > 0); a[3] += d.get('relaysEnded', 0); a[4] += (d.get('relaysEnded', 0) > 0)
    k[d['scen'].split('@')[0]][v].append((m.get('stall', 0), d.get('relaysEnded', 0)))
for v, a in agg.items():
    print(f"   {v}: runs {a[0]}, stall s {a[1]} in {a[2]} runs, relays ended {a[3]} in {a[4]} runs")
for kind in k:
    print(f"   {kind:7} " + "  ".join(f"{v} ({sum(x for x, y in L)}, {sum(y for x, y in L)})" for v, L in k[kind].items()))
print("\n== A7: interactive users (multi-link and bad-link cells)")
for sc in ('lmi@300-150-l0.1', 'lmbad@300-150'):
    print("  ", sc)
    for v in ('base', 'n4', 'core'):
        ds = [d for d in load('A7') if d['scen'] == sc and d['ver'] == v]
        ints = [i for d in ds for i in d.get('ints') or []]
        p50 = [i['lat']['p50'] for i in ints if i.get('lat')]; p99 = [i['lat']['p99'] for i in ints if i.get('lat')]
        print(f"     {v}: users {len(ints)}, on the download's link {sum(i['sameDl'] for i in ints)}, on the bad link {sum(i.get('bad', False) for i in ints)}, "
              f"download link pressed {sum(i['dlPressed'] for i in ints)}, links serving {sorted(set(i['links'] for i in ints))}, "
              f"p50 {st.median(p50):.0f} [{min(p50):.0f}-{max(p50):.0f}] p99 {st.median(p99):.0f} [{min(p99):.0f}-{max(p99):.0f}]")
print("\n== load at the runs' starts (1-min load; 4 cores)")
for f in ('A1', 'A7', 'A8', 'A6'):
    L = [float(l.split()[4]) for l in open(f"res/{f}.jsonl.log") if " start " in l]
    W = [l for l in open(f"res/{f}.jsonl.log") if " start " in l and not l.strip().endswith("waited 0s")]
    print(f"   {f}: runs {len(L)}, mean {st.mean(L):.2f}, max {max(L):.2f}, runs that waited {len(W)}")
