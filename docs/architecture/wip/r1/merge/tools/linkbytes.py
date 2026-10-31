#!/usr/bin/env python3
"""linkbytes.py RAWDIR: for each run of a rig block, how many BULK and QUIET
link sockets (bbr / cubic, the edge's ss at the end of the users' window)
carried user data: bytes received by the edge, at least 100 KiB (more than
keepalives), and the least, median and most per BULK socket."""
import glob, json, os, re, statistics, sys

raw = sys.argv[1]
print("| run | cell | BULK sockets | with >= 100 KiB received | received per BULK socket: least / median / most, MiB | QUIET sockets | QUIET received, MiB (each) |")
print("|---|---|---|---|---|---|---|")
for d in sorted(glob.glob(os.path.join(raw, "raw", "*"))):
    p = os.path.join(d, "ss-edge-end.txt")
    if not os.path.exists(p):
        continue
    bulk, quiet = [], []
    peer = None
    for line in open(p):
        if re.match(r"^\d", line):
            peer = line
            continue
        if peer:
            m = re.search(r"bytes_received:(\d+)", line)
            b = int(m.group(1)) if m else 0
            (quiet if " cubic " in line else bulk).append(b)
            peer = None
    meta = json.load(open(os.path.join(d, "meta.json")))
    cell = re.sub(r"RATE=\S+ RTT=\S+ LOSS=\S+ MODE=\S+ ", "", meta.get("cell", ""))
    mb = lambda v: "%.2f" % (v / (1 << 20))
    print("| %s | %s | %d | %d | %s / %s / %s | %d | %s |" % (
        os.path.basename(d), cell, len(bulk), sum(1 for b in bulk if b >= 100 << 10),
        mb(min(bulk)) if bulk else "-", mb(statistics.median(bulk)) if bulk else "-", mb(max(bulk)) if bulk else "-",
        len(quiet), " ".join(mb(b) for b in quiet)))
