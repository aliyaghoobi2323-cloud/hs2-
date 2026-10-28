#!/usr/bin/env python3
"""quick.py RES.jsonl [grep words]: one line per run of a res.jsonl (the
judge's quick look while a block runs): id, ok, dn/up sums of the main
window, the probe p99 over its floor, the class p99s, retransmits, and the
status lines that hold any of the words (default: classes, sendcap link 1,
place, groups, budget link 1, alpha)."""
import json, sys

words = sys.argv[2:] or ["classes", "sendcap link 1", "place:", "groups:", "budget link 1", "hint", "backlog"]


def p(v, k="main"):
    if isinstance(v, dict):
        v = v.get(k)
    return "-" if v is None else ("%.1f" % v if isinstance(v, (int, float)) else str(v))


for l in open(sys.argv[1]):
    if not l.strip():
        continue
    r = json.loads(l)
    out = [r["id"], "ok" if r.get("ok") else "FAIL", "dn", p(r.get("dn_sum_mbit")), "up", p(r.get("up_sum_mbit"))]
    pr = r.get("probe") or {}
    m = pr.get("main") if isinstance(pr, dict) else None
    if m and m.get("p99") is not None:
        out += ["probe99", "%.1f" % m["p99"]]
    cl = r.get("classes") or {}
    for k in ("game", "gameudp", "call"):
        c = cl.get(k)
        if not c:
            continue
        fl = c.get("floor_ms") or {}
        for d in ("up", "dn"):
            w = (c.get(d) or {}).get("main") or {}
            if w.get("p99") is not None and fl.get(d) is not None:
                out.append("%s.%s99+%.0f" % (k, d, w["p99"] - fl[d]))
    st = cl.get("stream")
    if st:
        out.append("stream start %s rebuf %s" % (st.get("start_p50") or st.get("startup_p50"), st.get("rebuffers")))
    rt = r.get("retrans") or {}
    out.append("rtx %s/%s" % (p((rt.get("exit") or {}).get("pct")), p((rt.get("edge") or {}).get("pct"))))
    print(" ".join(str(x) for x in out))
    sl = r.get("status_lines") or {}
    for side in ("edge", "exit"):
        lines = sl.get(side) or []
        if isinstance(lines, str):
            lines = lines.splitlines()
        for x in lines:
            if any(w in x for w in words):
                print("    %s: %s" % (side, x.strip()[:220]))
