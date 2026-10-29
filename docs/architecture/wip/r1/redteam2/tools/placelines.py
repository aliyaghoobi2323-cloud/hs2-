#!/usr/bin/env python3
"""placelines.py RAWDIR...: each run's placement figures from the edge's and
the exit's status at the end (hs2 status): connections opened on QUIET,
moved to BULK, exit hints, two-way kept, and the exit's groups line; and the
QUIET links' records and the links up. One markdown row a run."""
import glob
import os
import re
import sys


def grab(path, pat):
    try:
        for l in open(path, errors="replace"):
            m = re.search(pat, l)
            if m:
                return m
    except FileNotFoundError:
        pass
    return None


print("| run | opened on QUIET | moved to BULK (not yet) | exit hints | two-way kept | exit: groups open, moved |")
print("|---|---|---|---|---|---|")
for raw in sys.argv[1:]:
    for d in sorted(glob.glob(os.path.join(raw, "raw", "*"))):
        e = grab(d + "/status-edge.txt", r"opened on quiet (\d+).*moved to bulk (\d+) \(not yet (\d+).*exit hints (\d+).*two-way kept (\d+)")
        x = grab(d + "/status-exit.txt", r"groups: (\d+) open; moved to bulk (\d+)")
        print("| %s | %s | %s | %s | %s | %s |" % (
            os.path.basename(d),
            e.group(1) if e else "-", ("%s (%s)" % (e.group(2), e.group(3))) if e else "-",
            e.group(4) if e else "-", e.group(5) if e else "-",
            ("%s, %s" % (x.group(1), x.group(2))) if x else "-"))
