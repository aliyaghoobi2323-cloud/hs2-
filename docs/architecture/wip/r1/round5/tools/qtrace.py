#!/usr/bin/env python3
"""qtrace.py RAWRUN [FROM TO]: the link sockets of one rig run through its
TRACE_SS samples (run.sh, sstrace-exit.txt and sstrace-edge.txt, ss -ti every
0.5 s): per end (exit: the download's sender; edge: the upload's) and class
(QUIET: any CC but bbr; BULK: bbr), over the measuring window [FROM, TO] s
after the client's start (default WARM 5 to the end):
  goodput (bytes_acked a second, Mbit/s), delivery_rate mean, cwnd mean and
  p10, ssthresh, srtt - min_rtt mean and p90 (ms), Send-Q mean and max (KB),
  notsent mean and max (KB), retransmitted segments a second and their share
  of data segments, the CC seen.
R1 round 5: the proof that the QUIET link's window, not its demand, holds
the light classes under random loss."""
import json, os, re, sys

def num(pat, s, f=float):
    m = re.search(pat, s)
    return f(m.group(1)) if m else None

def parse(path):
    samples = []  # (t, [(sendq, detail), ...])
    cur = None
    hdr = None
    for line in open(path, errors="replace"):
        if line.startswith("T "):
            cur = (float(line.split()[1]), [])
            samples.append(cur)
            continue
        if cur is None:
            continue
        if line.startswith("\t") or line.startswith(" "):
            if hdr is not None:
                cur[1].append((hdr, line.strip()))
                hdr = None
        else:
            p = line.split()
            hdr = (int(p[0]), int(p[1]), p[2], p[3]) if len(p) >= 4 else None
    return samples

def sock(hdr, d):
    cc = d.split()[0]
    retr = re.search(r"retrans:(\d+)/(\d+)", d)
    return {
        "cc": cc, "cls": "bulk" if cc == "bbr" else "quiet",
        "key": hdr[2] + ">" + hdr[3], "sendq": hdr[1],
        "cwnd": num(r"cwnd:(\d+)", d, int), "ssthresh": num(r"ssthresh:(\d+)", d, int),
        "rtt": num(r"rtt:([\d.]+)/", d), "minrtt": num(r"minrtt:([\d.]+)", d),
        "dlv": num(r"delivery_rate (\d+)bps", d, int), "acked": num(r"bytes_acked:(\d+)", d, int) or 0,
        "segs": num(r"data_segs_out:(\d+)", d, int) or 0, "retr": int(retr.group(2)) if retr else 0,
        "notsent": num(r"notsent:(\d+)", d, int) or 0, "applim": " app_limited" in d,
    }

def pct(v, q):
    v = sorted(v)
    return v[min(len(v) - 1, int(q * len(v)))] if v else float("nan")

def summary(path, t0, lo, hi):
    out = {}
    s = parse(path)
    byk = {}
    for t, socks in s:
        rel = t - t0
        if rel < lo or rel > hi:
            continue
        for hdr, d in socks:
            x = sock(hdr, d)
            x["t"] = rel
            byk.setdefault((x["cls"], x["key"]), []).append(x)
    for (cls, key), xs in byk.items():
        if len(xs) < 3:
            continue
        a, b = xs[0], xs[-1]
        dt = b["t"] - a["t"]
        if dt <= 0:
            continue
        r = out.setdefault(cls, {"socks": 0, "cc": set(), "gput": 0.0, "dlv": [], "cwnd": [], "ssth": [], "q": [], "sendq": [], "notsent": [], "retr": 0, "segs": 0, "applim": 0, "n": 0, "dt": dt})
        r["socks"] += 1
        r["cc"].add(a["cc"])
        r["gput"] += (b["acked"] - a["acked"]) * 8 / dt / 1e6
        r["retr"] += b["retr"] - a["retr"]
        r["segs"] += b["segs"] - a["segs"]
        for x in xs:
            r["n"] += 1
            r["applim"] += x["applim"]
            if x["dlv"] is not None: r["dlv"].append(x["dlv"] / 1e6)
            if x["cwnd"] is not None: r["cwnd"].append(x["cwnd"])
            if x["ssthresh"] is not None: r["ssth"].append(x["ssthresh"])
            if x["rtt"] is not None and x["minrtt"] is not None: r["q"].append(x["rtt"] - x["minrtt"])
            r["sendq"].append(x["sendq"] / 1024)
            r["notsent"].append(x["notsent"] / 1024)
    res = {}
    for cls, r in out.items():
        m = lambda v: sum(v) / len(v) if v else float("nan")
        res[cls] = {
            "socks": r["socks"], "cc": ",".join(sorted(r["cc"])), "goodput_mbit": round(r["gput"], 2),
            "dlv_mean_mbit": round(m(r["dlv"]), 2), "cwnd_mean": round(m(r["cwnd"]), 1), "cwnd_p10": pct(r["cwnd"], 0.1),
            "ssthresh_mean": round(m(r["ssth"]), 1) if r["ssth"] else None,
            "q_mean_ms": round(m(r["q"]), 1), "q_p90_ms": round(pct(r["q"], 0.9), 1),
            "sendq_mean_kb": round(m(r["sendq"]), 1), "sendq_max_kb": round(max(r["sendq"]), 1),
            "notsent_mean_kb": round(m(r["notsent"]), 1), "notsent_max_kb": round(max(r["notsent"]), 1),
            "retr_per_s": round(r["retr"] / r["dt"], 2), "retr_share_pct": round(100 * r["retr"] / r["segs"], 3) if r["segs"] else None,
            "app_limited_share": round(r["applim"] / r["n"], 2),
        }
    return res

def main():
    run = sys.argv[1]
    t0 = float(open(os.path.join(run, "client-t0.txt")).read().split()[0])
    spec = json.load(open(os.path.join(run, "spec.json")))
    lo = float(sys.argv[2]) if len(sys.argv) > 2 else float(spec.get("warm", 5))
    hi = float(sys.argv[3]) if len(sys.argv) > 3 else 1e9
    res = {}
    for side in ("exit", "edge"):
        p = os.path.join(run, "sstrace-%s.txt" % side)
        if os.path.exists(p):
            res[side] = summary(p, t0, lo, hi)
    print(json.dumps(res, sort_keys=True))

if __name__ == "__main__":
    main()
