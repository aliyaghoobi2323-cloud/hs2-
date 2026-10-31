#!/usr/bin/env python3
"""switches.py RAWDIR: per run of a rig block (RAWDIR/raw/ID), the QUIET
links' congestion-control switches under random loss (quietcc.go's log
lines "a QUIET link sees random loss", "back to"), the slow QUIET links
replaced (quietreplace.go), the light link's moves and the waits for a QUIET
link (quiet.go's place line): each switch's time after the client started
(s), by end. R1 round 5."""
import datetime
import json
import os
import re
import sys


def clock(line, day):
    m = re.match(r"(\d\d):(\d\d):(\d\d\.\d+)", line)
    if not m:
        return None
    h, mi, s = int(m.group(1)), int(m.group(2)), float(m.group(3))
    return day + h * 3600 + mi * 60 + s


def main():
    raw = os.path.join(sys.argv[1], "raw")
    for rid in sorted(os.listdir(raw)):
        r = os.path.join(raw, rid)
        try:
            t0 = float(open(os.path.join(r, "client-t0.txt")).read().split()[0])
        except OSError:
            continue
        d = datetime.datetime.fromtimestamp(t0)
        day = t0 - (d.hour * 3600 + d.minute * 60 + d.second + d.microsecond / 1e6)
        out = []
        for side in ("edge", "exit"):
            try:
                lines = open(os.path.join(r, side + ".log"), errors="replace").read().splitlines()
            except OSError:
                continue
            for l in lines:
                k = None
                if "sees random loss" in l:
                    k = "loss->" + re.search(r": (\w+) until", l).group(1)
                elif "without it" not in l and "back to" in l and "quiet:" in l:
                    k = "back"
                elif "is slow next to the BULK links" in l:
                    k = "replace"
                elif "closed: replaced" in l:
                    k = "replaced-closed"
                if k:
                    t = clock(l, day)
                    out.append("%s %s %+.1f" % (side, k, t - t0 if t else float("nan")))
        place = ""
        try:
            m = re.search(r"light quiet .*?\)(; waited for quiet \d+ \(got one \d+\))?(; slow quiet replaced \d+ \(now \d+\))?", open(os.path.join(r, "status-edge.txt")).read())
            place = m.group(0) if m else ""
        except OSError:
            pass
        print("%-28s %s %s" % (rid, " | ".join(out) or "-", place))


if __name__ == "__main__":
    main()
