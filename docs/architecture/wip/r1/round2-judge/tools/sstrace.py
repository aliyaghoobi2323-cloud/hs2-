#!/usr/bin/env python3
"""sstrace.py RUNDIR [exit|edge]: the link sockets of one rig run over time,
from run.sh's TRACE_SS=1 samples (ss -ti each 0.5 s): per socket and sample,
the time from the client's start, the congestion control, srtt - min_rtt
(the queue α reads), the pacing rate and SO_MAX_PACING_RATE (α's or
C4-lite's cap), the delivery rate, and the rate the socket's bytes were
acknowledged since the sample before (what it really sent), Mbit/s."""
import re
import sys


def parse(path, t0):
    out = {}
    t = None
    key = None
    for l in open(path):
        if l.startswith("T "):
            t = float(l.split()[1]) - t0
            continue
        if not l.strip():
            continue
        if not l[0].isspace():
            f = l.split()
            key = " ".join(f[-2:]) if len(f) >= 2 else l
            continue
        d = {"t": t}
        m = re.search(r"^\s*(\w+) wscale", l)
        d["cc"] = m.group(1) if m else "?"
        for k, rx in (("rtt", r"\brtt:([\d.]+)/"), ("minrtt", r"\bminrtt:([\d.]+)"),
                      ("acked", r"\bbytes_acked:(\d+)"), ("rcvd", r"\bbytes_received:(\d+)"),
                      ("retr", r"\bretrans:\d+/(\d+)"), ("cwnd", r"\bcwnd:(\d+)")):
            m = re.search(rx, l)
            if m:
                d[k] = float(m.group(1))
        m = re.search(r"\bpacing_rate (\d+)bps(?:/(\d+)bps)?", l)
        if m:
            d["pace"] = int(m.group(1)) / 1e6
            d["cap"] = int(m.group(2)) / 1e6 if m.group(2) else None
        m = re.search(r"\bdelivery_rate (\d+)bps", l)
        if m:
            d["dlv"] = int(m.group(1)) / 1e6
        d["app"] = " app_limited" in l
        out.setdefault(key, []).append(d)
    return out


def main():
    r = sys.argv[1]
    side = sys.argv[2] if len(sys.argv) > 2 else "exit"
    try:
        t0 = float(open(r + "/client-t0.txt").read())
    except OSError:
        t0 = 0.0
    socks = parse("%s/sstrace-%s.txt" % (r, side), t0)
    for k, ss in socks.items():
        if len(ss) < 3:
            continue
        print("== %s %s (%s), %d samples" % (side, k, ss[-1].get("cc"), len(ss)))
        print("   t      q(ms)  cap   pace   dlv   sent  rcvd  retr app")
        prev = None
        for d in ss:
            sent = rcvd = 0.0
            if prev and d["t"] > prev["t"]:
                dt = d["t"] - prev["t"]
                sent = (d.get("acked", 0) - prev.get("acked", 0)) * 8 / dt / 1e6
                rcvd = (d.get("rcvd", 0) - prev.get("rcvd", 0)) * 8 / dt / 1e6
            q = d.get("rtt", 0) - d.get("minrtt", 0)
            print("   %5.1f %6.1f %5s %6.1f %5.1f %5.1f %5.1f %5d %s" % (
                d["t"], q, "-" if d.get("cap") is None else "%.1f" % d["cap"], d.get("pace", 0),
                d.get("dlv", 0), sent, rcvd, int(d.get("retr", 0)), "A" if d["app"] else ""))
            prev = d


if __name__ == "__main__":
    main()
