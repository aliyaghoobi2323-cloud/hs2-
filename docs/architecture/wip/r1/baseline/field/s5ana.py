#!/usr/bin/env python3
# s5ana.py RUNDIR... : the field S5 numbers of s5-iran.sh runs (R1 baseline).
# Samples are taken from the load window [load_from + WARM, load_to] (WARM 2 s).
# RTT_floor = p1 of the probe in the same window; the TUN ping's floor = p1 of
# the ping. Bounds (R1, section 6): probe p99 <= floor + max(20 ms, 3 x 1500 B /
# rate), TUN ping p99 <= floor + max(10 ms, 3 x 1500 B / rate), with rate the
# run's measured delivery rate (the slower direction). --json: one JSON line per run.
import json, re, sys

WARM = 2.0


def pct(v, q):
    v = sorted(v)
    if not v:
        return float("nan")
    i = q * (len(v) - 1)
    lo = int(i)
    hi = min(lo + 1, len(v) - 1)
    return v[lo] + (v[hi] - v[lo]) * (i - lo)


def run(d):
    meta = open(d + "/meta.txt").read()
    a = float(re.search(r"load_from=([\d.]+)", meta).group(1)) + WARM
    b = float(re.search(r"load_to=([\d.]+)", meta).group(1))
    probe, plost = [], 0
    for l in open(d + "/probe.txt"):
        if l.startswith("#"):
            if "err" in l:
                plost += 1
            continue
        t, ms = l.split()
        if a <= float(t) <= b:
            probe.append(float(ms))
    ping, seqs = [], set()
    for l in open(d + "/ping.txt"):
        m = re.match(r"\[([\d.]+)\].*icmp_seq=(\d+).*time=([\d.]+) ms", l)
        if m and a <= float(m.group(1)) - float(m.group(3)) / 1000 <= b:
            ping.append(float(m.group(3)))
            seqs.add(int(m.group(2)))
    sent = int((b - a) / 0.2)
    j = json.load(open(d + "/iperf3.json"))
    e = j.get("end", {})
    up = e.get("sum_received", {}).get("bits_per_second", 0) / 1e6
    dn = e.get("sum_received_bidir_reverse", {}).get("bits_per_second", 0) / 1e6
    rex_up = e.get("sum_sent", {}).get("retransmits")
    rex_dn = e.get("sum_sent_bidir_reverse", {}).get("retransmits")
    # inner TCP retransmissions as a share of the segments sent (inner MSS:
    # hs0 MTU 1320 - 52 B of IPv4 + TCP headers with timestamps)
    def rpct(k, rex):
        b_ = e.get(k, {}).get("bytes", 0)
        return round(100.0 * rex * 1268 / b_, 1) if rex is not None and b_ else None
    rate = min(x for x in (dn, up) if x > 0) if (dn > 0 or up > 0) else float("nan")
    pf, tf = pct(probe, 0.01), pct(ping, 0.01)
    pb = pf + max(20.0, 3 * 1500 * 8 / (rate * 1e6) * 1000)
    tb = tf + max(10.0, 3 * 1500 * 8 / (rate * 1e6) * 1000)
    r = {"run": d.rstrip("/").split("/")[-1], "dn_mbit": round(dn, 1), "up_mbit": round(up, 1),
         "iperf_retrans_dn": rex_dn, "iperf_retrans_up": rex_up,
         "iperf_retrans_pct_dn": rpct("sum_sent_bidir_reverse", rex_dn), "iperf_retrans_pct_up": rpct("sum_sent", rex_up),
         "probe": {"n": len(probe), "err": plost, "p1": round(pf, 1), "p50": round(pct(probe, .5), 1),
                   "p99": round(pct(probe, .99), 1), "max": round(max(probe), 1) if probe else None,
                   "bound": round(pb, 1), "ok": pct(probe, .99) <= pb},
         "ping": {"n": len(ping), "sent_est": sent, "lost_pct": round(100.0 * max(0, sent - len(ping)) / sent, 1) if sent else None,
                  "p1": round(tf, 1), "p50": round(pct(ping, .5), 1),
                  "p99": round(pct(ping, .99), 1), "max": round(max(ping), 1) if ping else None,
                  "bound": round(tb, 1), "ok": pct(ping, .99) <= tb},
         "window_s": round(b - a, 1)}
    return r


def main():
    js = "--json" in sys.argv
    for d in [x for x in sys.argv[1:] if x != "--json"]:
        r = run(d)
        if js:
            print(json.dumps(r))
            continue
        p, t = r["probe"], r["ping"]
        print("%-12s dn %6.1f up %6.1f Mbit/s | probe n %d p1 %.1f p50 %.1f p99 %.1f max %.1f bound %.1f %s"
              " | ping n %d lost %s%% p1 %.1f p50 %.1f p99 %.1f bound %.1f %s | inner retrans dn %s%% up %s%%" % (
                  r["run"], r["dn_mbit"], r["up_mbit"], p["n"], p["p1"], p["p50"], p["p99"], p["max"] or 0,
                  p["bound"], "ok" if p["ok"] else "FAIL", t["n"], t["lost_pct"], t["p1"], t["p50"], t["p99"], t["bound"],
                  "ok" if t["ok"] else "FAIL", r["iperf_retrans_pct_dn"], r["iperf_retrans_pct_up"]))


main()
