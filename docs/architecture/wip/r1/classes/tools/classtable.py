#!/usr/bin/env python3
"""classtable.py RIGDIR RES.jsonl [RES2.jsonl ...]: the classes smoke's
tables (markdown), per cell: each class's numbers for each binary, as
"median (the repeats in run order)", from ana.py's derive (RIGDIR holds
ana.py). One-way delays in ms over the class's floor; "late" = late
(> floor + 100 ms; call: 150 ms) and lost, % of sent."""
import json
import os
import statistics
import sys

sys.path.insert(0, os.path.abspath(sys.argv[1]))
import ana  # noqa: E402

ROWS = [
    ("context", [
        ("all users dn / cap_P10 %", "util_dn", 100, 0),
        ("all users up / cap_P10 %", "util_up", 100, 0),
        ("heavy user mean Mbit/s", "heavy_mean", 1, 2),
        ("heavy user min Mbit/s", "heavy_min", 1, 2),
    ]),
]
for k, name in (("game", "game TCP"), ("gameudp", "game UDP"), ("call", "call")):
    rows = []
    for d, dn in (("up", "user to server"), ("dn", "server to user")):
        p = "%s_%s_" % (k, d)
        rows += [
            ("%s p50 +ms" % dn, p + "p50", 1, 1),
            ("%s p99 +ms" % dn, p + "p99", 1, 1),
            ("%s p99.9 +ms" % dn, p + "p999", 1, 1),
            ("%s jitter p99 ms" % dn, p + "jit99", 1, 1),
            ("%s late+lost %%" % dn, p + "late", 100, 2),
            ("%s floor - RTT/2 ms" % dn, p + "floor_path", 1, 2),
        ]
        if k == "call":
            rows.append(("%s delivered %%" % dn, p + "deliv", 100, 1))
    ROWS.append((name, rows))
ROWS += [
    ("stream", [
        ("bitrate / fair share Mbit/s", None, None, None),
        ("startup p50 s", "stream_startup_p50", 1, 2),
        ("startup max s", "stream_startup_max", 1, 2),
        ("not started", "stream_notstarted", 1, 0),
        ("rebuffers (all viewers)", "stream_rebuf", 1, 0),
        ("viewers with a rebuffer", "stream_rebuf_users", 1, 0),
        ("rebuffer time s (all)", "stream_rebuf_s", 1, 1),
        ("segment rate Mbit/s", "stream_seg_mbit", 1, 2),
    ]),
    ("web", [
        ("page load p50 ms", "web_p50", 1, 0),
        ("page load p95 ms", "web_p95", 1, 0),
        ("first byte p50 ms", "web_fb50", 1, 0),
        ("first byte p95 ms", "web_fb95", 1, 0),
        ("pages (failed)", None, None, None),
    ]),
]


def cellfmt(vals, mult, prec):
    v = [x for x in vals if x is not None]
    if not v:
        return "-"
    f = lambda x: "inf" if x >= ana.INF else ("%%.%df" % prec) % (x * mult)
    if len(v) == 1:
        return f(v[0])
    return "%s (%s)" % (f(statistics.median(v)), ", ".join(f(x) for x in v))


def main():
    recs = []
    for p in sys.argv[2:]:
        recs += [json.loads(l) for l in open(p) if l.strip()]
    cells, order = {}, []
    for r in recs:
        if not r.get("ok"):
            continue
        c = r["cell"]
        if c not in cells:
            cells[c] = {}
            order.append(c)
        cells[c].setdefault(r["bin"], []).append(r)
    for c in order:
        bins = sorted(cells[c], key=lambda b: b != "old")
        ms = {b: [ana.derive(r) for r in sorted(cells[c][b], key=lambda r: r.get("start") or "")] for b in bins}
        print("### `%s`\n" % c)
        print("| | " + " | ".join("`%s` (%d)" % (b, len(ms[b])) for b in bins) + " |")
        print("|---|" + "---|" * len(bins))
        for title, rows in ROWS:
            have = any(k and any(k in m for b in bins for m in ms[b]) for _, k, _, _ in rows)
            if not have:
                continue
            print("| **%s** |%s" % (title, " |" * len(bins)))
            for label, k, mult, prec in rows:
                if label.startswith("bitrate"):
                    cols = [cellfmt([m.get("stream_sbr") for m in ms[b]], 1, 2) + " / " +
                            cellfmt([m.get("stream_fair") for m in ms[b]], 1, 2) for b in bins]
                elif label.startswith("pages"):
                    cols = ["%s (%s)" % (cellfmt([m.get("web_pages") for m in ms[b]], 1, 0),
                                         cellfmt([m.get("web_failed") for m in ms[b]], 1, 0)) for b in bins]
                else:
                    if not any(k in m for b in bins for m in ms[b]):
                        continue
                    cols = [cellfmt([m.get(k) for m in ms[b]], mult, prec) for b in bins]
                print("| %s | %s |" % (label, " | ".join(cols)))
        print()


if __name__ == "__main__":
    main()
