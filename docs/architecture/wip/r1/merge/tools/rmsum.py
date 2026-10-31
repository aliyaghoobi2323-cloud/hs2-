#!/usr/bin/env python3
"""rmsum.py RAWDIR RES.jsonl: many active links, per run: download, each user's
share (least, median, most, Mbit/s), the slowest second in the measured window,
resends at the exit, probe p99 over its floor, probes lost, links logged stuck,
links the exit lost, BULK sockets with at least 100 KiB received, host TCP pages
at most and samples past tcp_mem[1]."""
import json, os, re, statistics, sys

raw, res = sys.argv[1], sys.argv[2]
rows = [json.loads(l) for l in open(res) if l.strip()]
print("| cell | binary | rep | down Mbit/s | user least / median / most | slowest second | resends % | probe p99 - floor ms; lost | stuck lines | exit links down | BULK with data | host pages; past tcp_mem[1] |")
print("|---|---|---|---|---|---|---|---|---|---|---|---|")
order = {"prod": 0, "fin2": 1, "m": 2}
for r in sorted(rows, key=lambda r: (r["N"], order.get(r["bin"], 9), r["rep"])):
    d = os.path.join(raw, "raw", r["id"])
    us = sorted(u["dn_mbit"]["main"] for u in r["users"] if u.get("role") == "bulk")
    w = next(x for x in r["windows"] if x["name"] == "main")
    sec = [x for x in r["dn_1s"][w["from"]:w["to"]]]
    p = r["probe"]["all"]
    edge = open(os.path.join(d, "edge.log"), errors="replace").read()
    exitl = open(os.path.join(d, "exit.log"), errors="replace").read()
    stuck = len(re.findall(r"mtcp: link \d+ stuck", edge))
    down = len(re.findall(r"exit link down", exitl))
    bulk = []
    peer = None
    for line in open(os.path.join(d, "ss-edge-end.txt")):
        if re.match(r"^\d", line):
            peer = line
            continue
        if peer:
            m = re.search(r"bytes_received:(\d+)", line)
            if " cubic " not in line or r["bin"] == "prod":
                bulk.append(int(m.group(1)) if m else 0)
            peer = None
    h = r["hostmem"]
    print("| N=%d | %s | %d | %.1f | %.2f / %.2f / %.2f | %.0f | %.1f | %.1f; %d | %d | %d | %d of %d | %d; %d |" % (
        r["N"], r["bin"], r["rep"], r["dn_sum_mbit"]["main"], us[0], statistics.median(us), us[-1], min(sec),
        r["retrans"]["exit"]["pct"], p["p99"] - p["p1"], p["lost"], stuck, down,
        sum(1 for b in bulk if b >= 100 << 10), len(bulk), h["max_pages"], h["samples_past_tcp_mem1"]))
