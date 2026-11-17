#!/usr/bin/env python3
# ssq81.py SS.TXT T0 T1: kernel ground truth per path per 5 s sample from the
# lbv81 sampler (kharej sockets to Iran :2096): sockets, busy sockets (acked
# >= 64 KiB since the last sample), MB acked, median and p90 of rtt-minrtt on
# the busy ones (what the judge's queue reading uses), and the same on the
# quiet ones (light users' view of the path).
import re, sys, collections
f, a, b = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
P = {"192.168.50.1": "a", "[fd00:51::1]": "b"}
for k in range(2, 10):
    P[f"192.168.5{k}.1"] = "abcdefghij"[k]
prev = {}
t = None
cur = collections.defaultdict(list)
def med(v, q=0.5):
    v = sorted(v)
    return v[min(len(v) - 1, int(q * (len(v) - 1) + .5))] if v else float("nan")
def flush():
    if t is None or not (a <= t <= b):
        return
    row = []
    for p in "abcdefghij":
        L = cur.get(p, [])
        busy = [q for q, d in L if d >= 65536]
        quiet = [q for q, d in L if 0 < d < 65536]
        mb = sum(d for _, d in L) / 1e6
        row.append(f"{p}:{len(L):3d}/{len(busy):2d} {mb:6.1f}MB q{med(busy):5.1f}/{med(busy,.9):5.1f} lq{med(quiet):5.1f}")
    print(f"t={t:4d} " + " | ".join(row))
key = None
for line in open(f):
    if line.startswith("@T"):
        flush(); cur = collections.defaultdict(list); t = int(line.split()[1]); continue
    m = re.match(r"\s*\d+\s+\d+\s+(\S+)\s+(\S+):2096", line)
    if m:
        key, dst = m.group(1), m.group(2); continue
    if key and "rtt:" in line:
        r = re.search(r" rtt:([0-9.]+)", line); mr = re.search(r"minrtt:([0-9.]+)", line)
        ac = re.search(r"bytes_acked:(\d+)", line)
        if r and mr and ac:
            acked = int(ac.group(1)); d = acked - prev.get(key, acked); prev[key] = acked
            cur[P.get(dst, dst)].append((float(r.group(1)) - float(mr.group(1)), d))
        key = None
flush()
