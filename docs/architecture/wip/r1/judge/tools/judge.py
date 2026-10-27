#!/usr/bin/env python3
"""judge.py: the R1 judge's tables (task 17), on top of rig-scale/ana.py.

  judge.py s5 RES.jsonl...        per cell and binary label: median [min..max]
                                  of throughput, new/old, retransmits both ends,
                                  probe and TUN ping p99 against their bounds,
                                  CPU, memory, host busy (markdown, Persian)
  judge.py c3 RES.jsonl...        the C3 variant decision (T12): per variant
                                  and prototype, every cell's three gates
  judge.py p10 RES.jsonl...       P10 cells: verdicts (a)-(e), tun, first 5 s
                                  of the existing bulk user after the join
  judge.py json RES.jsonl...      every group's medians as JSON
"""
import gzip
import json
import os
import statistics
import sys

sys.path.insert(0, os.environ.get("RS", "/root/hs2-wt/r1-judge/docs/architecture/wip/rig-scale"))
import ana  # noqa: E402

LABELS = {"old": "پایه", "oldbl": "پایه (خط پایهٔ ۴ آبان)", "as": "الف بی C4-lite", "bs": "ب بی C4-lite",
          "ar": "الف کف نسبی (آزمایشگاهی)", "br": "ب کف نسبی (آزمایشگاهی)", "a": "الف", "b": "ب", "a4": "الف SEGS=4", "b4": "ب SEGS=4",
          "a8": "الف SEGS=8", "b8": "ب SEGS=8", "ag": "الف GATE=0", "bg": "ب GATE=0"}
VAR = {"": "پیش‌فرض سند", "4": "SEGS=4", "8": "SEGS=8", "g": "GATE=0"}


def lab(b):
    return LABELS.get(b, b)


def load(paths):
    """bl:PATH: the baseline's records (task 4), as the binary oldbl"""
    recs = []
    for p in paths:
        bl = p.startswith("bl:")
        q = p[3:] if bl else p
        for l in (gzip.open(q, "rt") if q.endswith(".gz") else open(q)):
            if l.strip():
                r = json.loads(l)
                if bl:
                    r["bin"] = "oldbl"
                recs.append(r)
    return recs


def med(v):
    v = [x for x in v if x is not None]
    if not v:
        return None
    return (statistics.median(v), min(v), max(v))


def f(t, p=1, pct=False):
    if t is None:
        return "-"
    g = (lambda x: "%.0f%%" % (100 * x)) if pct else (lambda x: ("%%.%df" % p) % x)
    if abs(t[1] - t[2]) < 1e-9:
        return g(t[0])
    return "%s [%s..%s]" % (g(t[0]), g(t[1]), g(t[2]))


def extra(rec, k, d=None):
    return rec.get("extra", {}).get(k, d)


def first5(rec, m):
    """the existing bulk user's per-second goodput from the join, over its floor"""
    if rec.get("MIX") != "newheavy":
        return None
    warm = float(extra(rec, "WARM", 5))
    dur = float(extra(rec, "DUR", 30))
    j = int(round(warm + dur / 2))
    ex = next((u for u in rec["users"] if u.get("role") == "exist"), None)
    if not ex or not ex.get("dn_1s"):
        return None
    dirn = extra(rec, "DIR", "dn")
    s = ex.get("dn_1s") if dirn == "dn" else ex.get("up_1s")
    light = sum(((u["dn_mbit"] if dirn == "dn" else u["up_mbit"]).get("after_settled", 0))
                for u in rec["users"] if u.get("role") in ("light", "pinger"))
    floor = (m["cap"] - light) / 2
    w = s[j:j + 5]
    if not w or floor <= 0:
        return None
    return {"min": min(w) / floor, "min_c": min(w) / m["cap"], "secs": w, "floor": floor,
            "before": statistics.mean(s[int(warm):j]) if j > warm else None}


def groups(recs):
    g, order = {}, []
    for r in recs:
        k = (r.get("cell"), r.get("bin"))
        if k not in g:
            g[k] = []
            order.append(k)
        g[k].append(r)
    return g, order


def summarize(rs):
    good = [r for r in rs if r.get("ok")]
    ms = []
    for r in good:
        m = ana.derive(r)
        m["_busy"] = r.get("host_busy_pct")
        mem = r.get("mem", {})
        m["hwm_edge"] = (mem.get("edge") or {}).get("hwm_kb")
        m["hwm_exit"] = (mem.get("exit") or {}).get("hwm_kb")
        x = first5(r, m) if "cap" in m else None
        if x:
            m["f5"] = x["min"]
            m["f5c"] = x["min_c"]
            m["_f5secs"] = x["secs"]
        ms.append(m)
    keys = sorted({k for m in ms for k in m if not k.startswith("_") and not isinstance(m[k], bool)})
    s = {k: med([m.get(k) for m in ms]) for k in keys}
    s["_n"] = len(good)
    s["_runs"] = len(rs)
    s["_ms"] = ms
    s["_fail"] = [r.get("errs") for r in rs if not r.get("ok")]
    return s


def cellname(rec):
    e = rec.get("extra", {})
    x = " ".join("%s=%s" % (k, v) for k, v in sorted(e.items()) if k not in ("DUR", "WARM", "SETTLE"))
    return "%s %s Mbit/s, RTT %s, اتلاف %s٪، N=%s%s" % (rec["MIX"], rec["RATE"], rec["RTT"], rec["LOSS"], rec["N"],
                                                       (", " + x) if x else "")


def by_cell(recs):
    g, order = groups(recs)
    cells, corder = {}, []
    for (c, b) in order:
        if c not in cells:
            cells[c] = {}
            corder.append(c)
        cells[c][b] = summarize(g[(c, b)])
        cells[c][b]["_rec"] = g[(c, b)][0]
    return cells, corder


def ratio(s, o, d):
    """new/old of the medians (o: the base of the same block, else the baseline's)"""
    if not s.get(d) or not o or not o.get(d) or o[d][0] <= 0.01:
        return None
    return s[d][0] / o[d][0]


def s5table(recs):
    cells, corder = by_cell(recs)
    out = []
    for c in corder:
        bs = cells[c]
        any_ = next(iter(bs.values()))["_rec"]
        out.append("\n#### %s\n" % cellname(any_))
        out.append("| باینری | اجرا | دانلود | آپلود | تازه/پایه (دانلود، آپلود) | بازارسال خروجی ٪ | بازارسال لبه ٪ | p99 کاوشگر | کف | p99−کف / مجاز | p99 پینگ TUN (گم‌شده) | p99−کف / مجاز | CPU لبه/خروجی ٪ | پرکاری ٪ | د | tun |")
        out.append("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
        o = bs.get("old")
        for b, s in bs.items():
            if not s["_n"]:
                out.append("| %s | 0/%d | - |" % (lab(b), s["_runs"]))
                continue
            rd, ru = ratio(s, o, "dn"), ratio(s, o, "up")
            rr = "-" if b == "old" or rd is None else "%.3f، %.3f" % (rd, ru if ru else float("nan"))
            allow = s["d_bound"][0] - s["rtt_floor"][0] if s.get("d_bound") else None
            tallow = s["t_bound"][0] - s["tfloor"][0] if s.get("t_bound") else None
            dv = "-" if not s.get("p99") else ("✓" if s["p99"][0] <= s["d_bound"][0] else "✗")
            dk = sum(1 for m in s["_ms"] if m.get("d"))
            tv = "-" if not s.get("t99") else ("✓" if s["t99"][0] <= s["t_bound"][0] else "✗")
            tk = sum(1 for m in s["_ms"] if m.get("tun"))
            out.append("| %s | %d/%d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s / %s | %s | %s %d/%d | %s %d/%d |" % (
                lab(b), s["_n"], s["_runs"], f(s.get("dn"), 2), f(s.get("up"), 2), rr,
                f(s.get("rt_exit"), 2), f(s.get("rt_edge"), 2), f(s.get("p99")), f(s.get("rtt_floor")),
                "+%.0f / %.0f" % (s["p99"][0] - s["rtt_floor"][0], allow) if s.get("p99") else "-",
                "%s (%s)" % (f(s.get("t99")), f(s.get("tlost"), 0)), "+%.0f / %.0f" % (s["t99"][0] - s["tfloor"][0], tallow) if s.get("t99") else "-",
                f(s.get("cpu_edge"), 0), f(s.get("cpu_exit"), 0), f(s.get("busy"), 0),
                dv, dk, s["_n"], tv, tk, s["_n"]))
    return "\n".join(out)


def c3(recs):
    cells, corder = by_cell(recs)
    res = {}
    lines = []
    for proto in ("a", "b"):
        for v in ("", "4", "8", "g"):
            b = proto + v
            ok_all, rows = True, []
            for c in corder:
                bs = cells[c]
                s, o = bs.get(b), bs.get("old")
                if not s or not s["_n"] or not o or not o["_n"]:
                    continue
                rx = s["rt_exit"][0] <= o["rt_exit"][0] + 1
                re_ = s["rt_edge"][0] <= o["rt_edge"][0] + 1
                pd = s["p99"][0] <= s["d_bound"][0]
                pt = s["t99"][0] <= s["t_bound"][0] if s.get("t99") else None
                rd, ru = ratio(s, o, "dn"), ratio(s, o, "up")
                th = (rd is None or rd >= 0.97) and (ru is None or ru >= 0.97)
                g = rx and re_ and pd and th
                ok_all &= g
                rows.append((c, rx, re_, pd, pt, th, s, o, rd, ru))
            res[b] = (ok_all, rows)
    for b, (ok_all, rows) in res.items():
        lines.append("\n**%s**: %s" % (lab(b) if b in LABELS else b, "همهٔ دروازه‌ها ✓" if ok_all else "دست‌کم یک دروازه ✗"))
        lines.append("")
        lines.append("| خانه | بازارسال خروجی (پایه+۱) | بازارسال لبه (پایه+۱) | p99 کاوشگر − کران | p99 TUN − کران | توان تازه/پایه | حکم |")
        lines.append("|---|---|---|---|---|---|---|")
        for (c, rx, re_, pd, pt, th, s, o, rd, ru) in rows:
            r0 = s["_rec"]
            lines.append("| %s %s/N=%s/%s٪ | %.2f (%.2f) %s | %.2f (%.2f) %s | %+.0f %s | %s | %s، %s %s | %s |" % (
                r0["MIX"], r0["RATE"], r0["N"], r0["LOSS"],
                s["rt_exit"][0], o["rt_exit"][0] + 1, "✓" if rx else "✗",
                s["rt_edge"][0], o["rt_edge"][0] + 1, "✓" if re_ else "✗",
                s["p99"][0] - s["d_bound"][0], "✓" if pd else "✗",
                ("%+.0f %s" % (s["t99"][0] - s["t_bound"][0], "✓" if pt else "✗")) if pt is not None else "-",
                "%.3f" % rd if rd else "-", "%.3f" % ru if ru else "-", "✓" if th else "✗",
                "✓" if (rx and re_ and pd and th) else "✗"))
    return "\n".join(lines), res


def p10table(recs):
    cells, corder = by_cell(recs)
    out = []
    for c in corder:
        bs = cells[c]
        if not any(k in bs for k in ("a", "b")):
            continue
        r0 = next(iter(bs.values()))["_rec"]
        mix = r0["MIX"]
        out.append("\n#### %s\n" % cellname(r0))
        out.append("| باینری | اجرا | لینک | دانلود | آپلود | تازه/پایه | الف | ب | ج | p99 کاوشگر | کف | p99−کف / مجاز | د | هـ | p99 پینگ TUN | tun | ۵ ثانیهٔ نخست (کمینه/کف) | CPU لبه/خروجی ٪ | حافظهٔ بیشینه لبه/خروجی MiB | پرکاری ٪ |")
        out.append("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
        o = bs.get("old") or bs.get("oldbl")
        for b, s in bs.items():
            if not s["_n"]:
                out.append("| %s | 0/%d | %s |" % (lab(b), s["_runs"], " ".join(str(x) for x in s["_fail"])[:80]))
                continue
            rd, ru = ratio(s, o, "dn"), ratio(s, o, "up")

            def vd(flag, key, thr, ge=True):
                if not s.get(key):
                    return "-"
                v = s[key][0] >= thr if ge else s[key][0] <= thr
                k = sum(1 for m in s["_ms"] if m.get(flag))
                return "%s %s %d/%d" % (f(s[key], pct=True) if ge else "", "✓" if v else "✗", k, s["_n"])
            a = vd("a", "a_ratio", 0.95)
            bb = vd("b", "b_ratio", 0.9) if mix in ("one_vs_16", "s5", "newheavy") else "-"
            cc = vd("c", "c_ratio", 0.97)
            p99key = "p99_after" if mix == "newheavy" else "p99"
            d = "-"
            if s.get(p99key) and s.get("d_bound"):
                dk = sum(1 for m in s["_ms"] if (m.get("p99_after", m.get("p99", 1e9)) <= m.get("d_bound", 0)))
                d = "%s %d/%d" % ("✓" if s[p99key][0] <= s["d_bound"][0] else "✗", dk, s["_n"])
            e = "-"
            if mix == "newheavy" and s.get("p99_after"):
                ek = sum(1 for m in s["_ms"] if m.get("e"))
                ev = s["p99_after"][0] <= s["d_bound"][0] and s["e_bulk_ratio"][0] >= 0.9
                e = "%s %d/%d" % ("✓" if ev else "✗", ek, s["_n"])
            t = "-"
            if s.get("t99"):
                tk = sum(1 for m in s["_ms"] if m.get("tun"))
                t = "%s %d/%d" % ("✓" if s["t99"][0] <= s["t_bound"][0] else "✗", tk, s["_n"])
            hw = "%s / %s" % (f(tuple(x / 1024 for x in s["hwm_edge"]), 0) if s.get("hwm_edge") else "-",
                              f(tuple(x / 1024 for x in s["hwm_exit"]), 0) if s.get("hwm_exit") else "-")
            out.append("| %s | %d/%d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s / %s | %s | %s |" % (
                lab(b), s["_n"], s["_runs"], f(s.get("links"), 0), f(s.get("dn"), 2), f(s.get("up"), 2),
                "-" if b in ("old", "oldbl") or rd is None else "%.3f" % rd if ru is None else "%.3f، %.3f" % (rd, ru),
                a, bb, cc, f(s.get(p99key)), f(s.get("rtt_floor")),
                "+%.0f / %.0f" % (s[p99key][0] - s["rtt_floor"][0], s["d_bound"][0] - s["rtt_floor"][0]) if s.get(p99key) else "-",
                d, e, f(s.get("t99")), t, f(s.get("f5"), pct=True) if s.get("f5") else "-",
                f(s.get("cpu_edge"), 0), f(s.get("cpu_exit"), 0), hw, f(s.get("busy"), 0)))
    return "\n".join(out)


def main():
    a = sys.argv[1:]
    recs = load(a[1:])
    if a[0] == "s5":
        print(s5table(recs))
    elif a[0] == "c3":
        print(c3(recs)[0])
    elif a[0] == "p10":
        print(p10table(recs))
    elif a[0] == "json":
        cells, corder = by_cell(recs)
        out = []
        for c in corder:
            for b, s in cells[c].items():
                out.append({"cell": c, "bin": b, "n": s["_n"], "runs": s["_runs"],
                            "med": {k: v for k, v in s.items() if not k.startswith("_")},
                            "f5secs": [m.get("_f5secs") for m in s["_ms"] if m.get("_f5secs")]})
        print(json.dumps(out, indent=1))


if __name__ == "__main__":
    main()
