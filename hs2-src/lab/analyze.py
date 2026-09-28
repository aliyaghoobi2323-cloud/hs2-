#!/usr/bin/env python3
"""Summarise lab/sweep.py results: median over repetitions, per label.

    lab/analyze.py results.jsonl [results2.jsonl ...]

Columns: throughput (Mbit/s), echo latency p50/p95 under load (ms), new
connection time p50 (ms), failed new connections, hs0 ping avg (ms, modes with
a TUN), and the number of repetitions.
"""
import json, sys, statistics as st
from collections import defaultdict

rows = defaultdict(list)
for path in sys.argv[1:]:
    for line in open(path):
        try:
            d = json.loads(line)
        except Exception:
            continue
        p = (d.get('result') or {}).get('probe')
        if not p:
            continue
        ping = (d['result'].get('ping_avg_max') or '').split('/')[0]
        rows[d['run'].get('label', '?')].append((
            p['mbps'], p['echo_p50_ms'], p['echo_p95_ms'], p['conn_p50_ms'],
            p['conn_fail'] + p['bulk_errors'], float(ping) if ping else None))

def med(v):
    v = [x for x in v if x is not None and x >= 0]
    return st.median(v) if v else float('nan')

print(f"{'label':34s} {'Mbit':>6s} {'echo50':>7s} {'echo95':>7s} {'conn50':>7s} {'fail':>4s} {'ping':>6s} n")
for label in sorted(rows):
    r = rows[label]
    cols = list(zip(*r))
    print(f"{label:34s} {med(cols[0]):6.1f} {med(cols[1]):7.0f} {med(cols[2]):7.0f} "
          f"{med(cols[3]):7.0f} {sum(cols[4]):4d} {med(cols[5]):6.0f} {len(r)}")
