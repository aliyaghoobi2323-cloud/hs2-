#!/usr/bin/env python3
"""g2lines.py RES.jsonl...: γ2's per-run lines from hs2 status (round 3): the
QUIET links' records (records over the run, data and pad bytes, tails held:
the batching measure), each run's retransmits, and its BULK links' budget
lines (mode, B, queue). One block of lines per run, by cell and binary."""
import json
import re
import sys

REC = re.compile(r"quiet records link (\d+): .*\(all: (\d+) records, (\d+) B data, (\d+) B pad(?:, (\d+) tails held)?\)")


def main():
    for p in sys.argv[1:]:
        for l in open(p):
            if not l.strip():
                continue
            r = json.loads(l)
            tot = (r.get("total_s") or 0) or 1
            print("== %s | %s rep %s" % (r.get("cell"), r.get("bin"), r.get("rep")))
            rt = r.get("retrans") or {}
            print("   retrans %% exit %s edge %s" % ((rt.get("exit") or {}).get("pct"), (rt.get("edge") or {}).get("pct")))
            for end, v in (r.get("status_lines") or {}).items():
                lines = v if isinstance(v, list) else str(v).splitlines()
                for line in lines:
                    m = REC.search(line)
                    if m:
                        n, d, pd = int(m.group(2)), int(m.group(3)), int(m.group(4))
                        held = m.group(5) or "-"
                        print("   %s quiet q%s: %.0f records/s, %.0f B data a record, pad %.1f%%, wire %.2f Mbit/s, tails held %s" % (
                            end, m.group(1), n / tot, d / max(n, 1), 100 * pd / max(d + pd, 1), (d + pd + 26 * n) * 8 / tot / 1e6, held))
                    elif "budget link" in line:
                        print("   %s %s" % (end, line.strip().replace("cores:", "").strip()[:200]))


if __name__ == "__main__":
    main()
