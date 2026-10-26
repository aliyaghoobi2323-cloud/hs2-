#!/usr/bin/env python3
# mktables.py BASELINE_DIR: Markdown tables for baseline.fa.md from the rig
# results (p10/, s5/, extra/ res.jsonl via ana.py --json) and the field runs
# (field/s5.jsonl). Median of the repeats with [min..max].
import json, os, subprocess, sys

B = sys.argv[1]
ANA = os.path.join(B, "../../rig-scale/ana.py")


def summ(block):
    p = os.path.join(B, block, "res.jsonl")
    if not os.path.exists(p) or not os.path.getsize(p):
        return []
    out = subprocess.run([sys.executable, ANA, p, "--json"], capture_output=True, text=True, check=True).stdout
    return json.loads(out)


def cell(s):
    return dict(t.split("=", 1) for t in s["cell"].split())


def f(t, p=1, pct=False, sign=False):
    if not t:
        return "-"
    if pct:
        g = lambda x: "%.0f%%" % (100 * x)
    else:
        g = lambda x: (("%+." if sign else "%.") + "%df" % p) % x
    if abs(t[1] - t[2]) < 1e-9:
        return g(t[0])
    return "%s [%s..%s]" % (g(t[0]), g(t[1]), g(t[2]))


def v(s, k):
    x = (s.get("verdict") or {}).get(k)
    if not x:
        return "-"
    return ("✓" if x["pass"] else "✗") + " " + x["runs"]


def d(md, a, b):
    if md.get(a) and md.get(b):
        return [md[a][0] - md[b][0], md[a][1] - md[b][1], md[a][2] - md[b][2]]
    return None


def over(md):
    # p99 above RTT_floor, against the allowance (bound - floor)
    if not md.get("p99") or not md.get("rtt_floor"):
        return "-"
    return "%+.0f / %.0f" % (md["p99"][0] - md["rtt_floor"][0], md["d_bound"][0] - md["rtt_floor"][0])


def tover(md):
    if not md.get("t99") or not md.get("tfloor"):
        return "-"
    return "%+.0f / %.0f" % (md["t99"][0] - md["tfloor"][0], md["t_bound"][0] - md["tfloor"][0])


def runs(s):
    return "%d/%d" % (s["ok"], s["runs"])


def p10():
    S = summ("p10")
    if not S:
        print("(no P10 results)\n")
        return
    by = {}
    for s in S:
        c = cell(s)
        by.setdefault(c["MIX"], []).append((int(c["N"]), s))
    for mix in ("lone16", "one_vs_16", "newheavy", "tunping"):
        rows = sorted(by.get(mix, []), key=lambda x: x[0])
        if not rows:
            continue
        print("#### `%s`\n" % mix)
        if mix == "lone16":
            print("| N | اجرا | لینک | دانلود Mbit/s | (الف) کاربر تنها / ظرفیت | (ج) جمع / ظرفیت | p50 کاوشگر | p99 کاوشگر | کف | p99−کف / مجاز | CPU لبه/خروجی ٪ | پرکاری میزبان ٪ | الف | ج | د |")
            print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
            for n, s in rows:
                md = s["med"]
                print("| %d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s / %s | %s | %s | %s | %s |" % (
                    n, runs(s), f(md.get("links"), 0), f(md.get("dn")), f(md.get("a_ratio"), pct=True),
                    f(md.get("c_ratio"), pct=True), f(md.get("p50")), f(md.get("p99")), f(md.get("rtt_floor")),
                    over(md), f(md.get("cpu_edge"), 0), f(md.get("cpu_exit"), 0), f(md.get("busy"), 0),
                    v(s, "a"), v(s, "c"), v(s, "d")))
        elif mix == "one_vs_16":
            print("| N | اجرا | لینک | جمع دانلود | کاربر ۱ اتصالی Mbit/s | (ب) ۱ اتصالی / کفش | (ج) جمع / ظرفیت | p50 کاوشگر | p99 کاوشگر | کف | p99−کف / مجاز | CPU لبه/خروجی ٪ | پرکاری ٪ | ب | ج | د |")
            print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
            for n, s in rows:
                md = s["med"]
                print("| %d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s / %s | %s | %s | %s | %s |" % (
                    n, runs(s), f(md.get("links"), 0), f(md.get("dn")), f(md.get("one")), f(md.get("b_ratio"), pct=True),
                    f(md.get("c_ratio"), pct=True), f(md.get("p50")), f(md.get("p99")), f(md.get("rtt_floor")), over(md),
                    f(md.get("cpu_edge"), 0), f(md.get("cpu_exit"), 0), f(md.get("busy"), 0),
                    v(s, "b"), v(s, "c"), v(s, "d")))
        elif mix == "newheavy":
            print("| N | اجرا | لینک | حجیم موجود پیش / پس Mbit/s | سنگین تازه پس | (هـ) موجود / کفش | (ج) جمع / ظرفیت | p99 کاوشگر پیش / پس | کف | p99 پس−کف / مجاز | CPU لبه/خروجی ٪ | پرکاری ٪ | ج | هـ |")
            print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
            for n, s in rows:
                md = s["med"]
                ov = "-"
                if md.get("p99_after") and md.get("rtt_floor"):
                    ov = "%+.0f / %.0f" % (md["p99_after"][0] - md["rtt_floor"][0], md["d_bound"][0] - md["rtt_floor"][0])
                print("| %d | %s | %s | %s / %s | %s | %s | %s | %s / %s | %s | %s | %s / %s | %s | %s | %s |" % (
                    n, runs(s), f(md.get("links"), 0), f(md.get("exist_before")), f(md.get("exist_after")),
                    f(md.get("heavy_after")), f(md.get("e_bulk_ratio"), pct=True), f(md.get("c_ratio"), pct=True),
                    f(md.get("p99_before")), f(md.get("p99_after")), f(md.get("rtt_floor")), ov,
                    f(md.get("cpu_edge"), 0), f(md.get("cpu_exit"), 0), f(md.get("busy"), 0), v(s, "c"), v(s, "e")))
        else:
            print("| N | اجرا | لینک | دانلود TUN درونی Mbit/s | (ج) جمع / ظرفیت | p50 پینگ TUN | p99 پینگ TUN | کف پینگ | p99−کف / مجاز | p99 کاوشگر درگاه | p99−کف / مجاز | CPU لبه/خروجی ٪ | پرکاری ٪ | ج | tun | د |")
            print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
            for n, s in rows:
                md = s["med"]
                print("| %d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s / %s | %s | %s | %s | %s |" % (
                    n, runs(s), f(md.get("links"), 0), f(md.get("tun_dn")), f(md.get("c_ratio"), pct=True),
                    f(md.get("t50")), f(md.get("t99")), f(md.get("tfloor")), tover(md), f(md.get("p99")), over(md),
                    f(md.get("cpu_edge"), 0), f(md.get("cpu_exit"), 0), f(md.get("busy"), 0),
                    v(s, "c"), v(s, "tun"), v(s, "d")))
        print()


def s5(block):
    S = summ(block)
    if not S:
        print("(no results)\n")
        return
    rows = []
    for s in S:
        c = cell(s)
        rows.append(((c["MIX"], float(c["RATE"]), int(c["RTT"]), float(c["LOSS"]), int(c["N"])), c, s))
    rows.sort(key=lambda x: x[0])
    print("| آمیزه | RATE | RTT | اتلاف ٪ | N | اجرا | دانلود | آپلود | (ب) بدترین حجیم / کف | (ج) جمع / ظرفیت | p50 کاوشگر | p99 کاوشگر | کف | p99−کف / مجاز | p50 پینگ TUN | p99 پینگ TUN | p99−کف / مجاز | CPU لبه/خروجی ٪ | بازارسال خروجی/لبه ٪ | پرکاری ٪ | ب | ج | د | tun |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for k, c, s in rows:
        md = s["med"]
        mix = c["MIX"] + (" v6" if c.get("TUNV6") == "1" else "")
        print("| %s | %g | %s | %g | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s / %s | %s / %s | %s | %s | %s | %s | %s |" % (
            mix, k[1], c["RTT"], k[3], c["N"], runs(s), f(md.get("dn"), 2), f(md.get("up"), 2), f(md.get("b_ratio"), pct=True),
            f(md.get("c_ratio"), pct=True), f(md.get("p50")), f(md.get("p99")), f(md.get("rtt_floor")), over(md),
            f(md.get("t50")), f(md.get("t99")), tover(md), f(md.get("cpu_edge"), 0), f(md.get("cpu_exit"), 0),
            f(md.get("rt_exit"), 2), f(md.get("rt_edge"), 2), f(md.get("busy"), 0),
            v(s, "b"), v(s, "c"), v(s, "d"), v(s, "tun")))
    print()


def field():
    p = os.path.join(B, "field", "s5.jsonl")
    if not os.path.exists(p):
        print("(no field results)\n")
        return
    rs = [json.loads(l) for l in open(p) if l.strip()]
    print("| اجرا | دانلود Mbit/s | آپلود Mbit/s | بازارسال TCP درونی دانلود/آپلود ٪ | کاوشگر n | p1 (کف) | p50 | p99 | بیشینه | p99−کف / مجاز | پینگ TUN n | گم‌شده ٪ | p1 (کف) | p50 | p99 | p99−کف / مجاز | کاوشگر | پینگ |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for r in rs:
        p_, t = r["probe"], r["ping"]
        print("| %s | %.1f | %.1f | %s / %s | %d | %.1f | %.1f | %.1f | %.1f | %+.0f / %.0f | %d | %s | %.1f | %.1f | %.1f | %+.0f / %.0f | %s | %s |" % (
            r["run"], r["dn_mbit"], r["up_mbit"], r.get("iperf_retrans_pct_dn"), r.get("iperf_retrans_pct_up"),
            p_["n"], p_["p1"], p_["p50"], p_["p99"], p_["max"] or 0,
            p_["p99"] - p_["p1"], p_["bound"] - p_["p1"], t["n"], t.get("lost_pct"), t["p1"], t["p50"], t["p99"], t["p99"] - t["p1"],
            t["bound"] - t["p1"], "✓" if p_["ok"] else "✗", "✓" if t["ok"] else "✗"))
    import statistics as st
    def m(k1, k2=None):
        x = [r[k1] if k2 is None else r[k1][k2] for r in rs]
        return "%.1f [%.1f..%.1f]" % (st.median(x), min(x), max(x))
    print("| **میانه** | %s | %s | | | %s | %s | %s | | | | | %s | %s | %s | | | |" % (
        m("dn_mbit"), m("up_mbit"), m("probe", "p1"), m("probe", "p50"), m("probe", "p99"),
        m("ping", "p1"), m("ping", "p50"), m("ping", "p99")))
    print()


which = sys.argv[2] if len(sys.argv) > 2 else "all"
if which in ("all", "p10"):
    p10()
if which in ("all", "s5"):
    s5("s5")
if which in ("all", "extra"):
    s5("extra")
if which in ("all", "field"):
    field()
