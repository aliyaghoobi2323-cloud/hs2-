# anaA.py [OUT.json]: Part A (fixed windows): per run the signals the window core uses, over
# the measure window; per cell (path, loss, dir, window) the median over runs.
import sys, json, math
from z11lib import *

def rsd(v):
    v = [x for x in v if x is not None]
    if len(v) < 3:
        return None
    m = pct(v, 0.5)
    return 1.4826 * pct([abs(x - m) for x in v], 0.5)

def runA(d):
    o = basic(d)
    rid = d["_id"]
    up = isup(d)
    side = "exit" if up else "edge"
    w0, w1 = window(d)
    # ground truth: the bottleneck queue of the bulk direction, every 100 ms (ms)
    q = t2win(d, "qu" if up else "qd")
    o["qtrue"] = mean(q)
    o["qtrue90"] = pct(q, 0.9)
    # the sender socket's srtt - min_rtt (exit for downloads, edge for uploads), every 100 ms
    sx = "e" if up else "x"
    rx = "x" if up else "e"
    sr, mr = t2win(d, "srtt" + sx), t2win(d, "minrtt" + sx)
    o["qsnd_dense"] = mean([a - b for a, b in zip(sr, mr) if a and b])
    sr, mr = t2win(d, "srtt" + rx), t2win(d, "minrtt" + rx)
    o["qown_dense"] = mean([a - b for a, b in zip(sr, mr) if a and b])
    # the control ping (edge's), latest every 100 ms, over its floor
    c, cm = t2win(d, "ctrl"), t2win(d, "ctrlMin")
    qc = [a - b for a, b in zip(c, cm) if a > 0 and b > 0]
    o["qctrl_dense"] = mean(qc)
    # BBR sender: PROBE_RTT events (cwnd <= 4) over the whole run
    cw = (d.get("t2") or {}).get("cwnd" + sx) or []
    ev, inp = 0, False
    for k, v in enumerate(cw):
        if k > 10 and v <= 4 and not inp:
            ev += 1
        inp = v <= 4
    o["probertt"] = ev
    # pacing gain the sender holds: pacing / delivered rate
    pace = t2win(d, "pace" + sx)
    o["pace"] = mean(pace)
    # trace lines of the receiving core in the window
    L = trace(rid, side)
    if L:
        t00 = L[0]["_t"]
        L = [x for x in L if w0 + 0.5 <= x["_t"] - t00 <= w1 and x["dt_s"] > 0]
    o["n_eval"] = len(L)
    qt, qown, qsnd, qctrl, ooo, hit, lag, inF, tau, spr, tmin, tlast, tb, wwin, ceil = ([] for _ in range(15))
    sndrec = []
    for x in L:
        if x["q_turn_ok"]:
            qt.append(x["q_turn_ms"])
        if x["srtt_ms"] > 0 and x["t_base_ms"] > 0:
            qown.append(x["q_own_ms"])
        if x.get("snd_srtt_ms") and x.get("snd_min_rtt_ms"):
            sndrec.append(x["snd_srtt_ms"] - x["snd_min_rtt_ms"])
        for p in x.get("pongs_ms") or []:
            if x["ctrl_min_ms"] > 0:
                qctrl.append(p - x["ctrl_min_ms"])
        if x["ooo_ok"]:
            ooo.append(x["ooo"])
        tb.append(x["turn_base_ms"])
        ss = [s for s in (x["streams"] or []) if s["win"] >= 1 << 20]
        s = max(ss, key=lambda s: s["read"]) if ss else None
        if s:
            inF.append(1 if s["f"] else 0)
            ceil.append(s["ceil"])
            wwin.append(s["win"])
            if s["upd"] > 0:
                hit.append(s["hit"] / s["upd"])
                lag.append(s["lag"] / s["upd"])
            if s["turn_min_ms"] > 0:
                tmin.append(s["turn_min_ms"])
            if s["turn_last_ms"] > 0:
                tlast.append(s["turn_last_ms"])
            if s["f"] and x["r_f"] > 0 and x["t_base_ms"] > 0:
                hs = x["h_step"]
                tau.append(((x["w_f"] - hs) / x["r_f"]) * 1000 - x["t_base_ms"])
        if x["spread_ms"]:
            spr.append(x["spread_ms"])
    o["qturn"], o["qturn_sd"], o["qturn_p90"] = mean(qt), sd(qt), pct(qt, 0.9)
    o["qturn_med"], o["qturn_rsd"] = pct(qt, 0.5), rsd(qt)
    rfs = [x["r_f"] for x in L if x["n_f"] > 0 and x["r_f"] > 0 and x["dt_s"] >= 0.35]
    o["rf_cv"] = sd(rfs) / mean(rfs) if len(rfs) > 2 else None
    sv = [100 * x["snd"] for x in L if x["snd_ok"]]
    o["snd"], o["snd_sd"], o["snd_max"] = mean(sv), sd(sv), (max(sv) if sv else None)
    ov = [100 * x["ooo"] for x in L if x["ooo_ok"]]
    o["ooo_sd"], o["ooo_med"] = sd(ov), pct(ov, 0.5)
    o["dt"] = mean([x["dt_s"] for x in L])
    # calm intervals: no packet behind a hole (no loss event in them)
    calm = [x for x in L if x["ooo_ok"] and x["ooo"] == 0]
    o["calm"] = len(calm) / len(L) if L else None
    cq = [x["q_turn_ms"] for x in calm if x["q_turn_ok"]]
    o["qturn_calm"], o["qturn_calm_sd"] = mean(cq), sd(cq)
    co = [x["q_own_ms"] for x in calm if x["srtt_ms"] > 0]
    o["qown_calm"], o["qown_calm_sd"] = mean(co), sd(co)
    cs = [x["snd_srtt_ms"] - x["snd_min_rtt_ms"] for x in calm if x.get("snd_srtt_ms") and x.get("snd_min_rtt_ms")]
    o["qsnd_calm"], o["qsnd_calm_sd"] = mean(cs), sd(cs)
    cc = [p - x["ctrl_min_ms"] for x in calm for p in (x.get("pongs_ms") or []) if x["ctrl_min_ms"] > 0]
    o["qctrl_calm"], o["qctrl_calm_sd"] = mean(cc), sd(cc)
    o["qown"], o["qown_sd"] = mean(qown), sd(qown)
    o["qsnd"], o["qsnd_sd"], o["qsnd_n"] = mean(sndrec), sd(sndrec), len(sndrec)
    o["qctrl"], o["qctrl_sd"], o["qctrl_p50"], o["qctrl_n"] = mean(qctrl), sd(qctrl), pct(qctrl, 0.5), len(qctrl)
    o["ooo"] = mean(ooo) * 100 if ooo else None
    o["hit"], o["hit_sd"] = mean(hit), sd(hit)
    o["lag"] = mean(lag)
    o["inF"] = mean(inF)
    o["ceil0"] = sum(1 for c in ceil if c == 0)  # evaluations with the fixed window not in force
    o["win"] = mean(wwin)
    o["tau"], o["tau_sd"] = mean(tau), sd(tau)
    o["tbase"] = mean([x["t_base_ms"] for x in L]) if L else None
    o["turnbase"] = mean(tb)
    o["tmin"] = mean(tmin)
    o["tlast"] = mean(tlast)
    o["spread"] = mean(spr)
    # every turnaround sample of the bulk stream in the window (Z5_TURNS)
    tv = (d.get("turns") or {}).get("u" if up else "f") or []
    smp = [(tv[k] / 1000, tv[k + 1]) for k in range(0, len(tv) - 1, 2)]
    base = o["turnbase"] or (min(v for _, v in smp) if smp else None)
    win = [v - base for t, v in smp if w0 <= t <= w1] if base else []
    o["ts_n"] = len(win)
    if win:
        o["ts_min"], o["ts_p25"], o["ts_p50"], o["ts_p90"], o["ts_mean"] = min(win), pct(win, 0.25), pct(win, 0.5), pct(win, 0.9), mean(win)
        # per evaluation interval (~0.4 s): the p25 of the samples, vs the controller's q_turn (min)
        per = {}
        for t, v in smp:
            if w0 <= t <= w1:
                per.setdefault(int(t / 0.4), []).append(v - base)
        p25s = [pct(v, 0.25) for v in per.values() if len(v) >= 3]
        mins = [min(v) for v in per.values() if len(v) >= 3]
        o["ts_ip25"], o["ts_ip25_sd"] = mean(p25s), sd(p25s)
        o["ts_imin"], o["ts_imin_sd"] = mean(mins), sd(mins)
        o["ts_ip25_med"], o["ts_ip25_rsd"] = pct(p25s, 0.5), rsd(p25s)
        o["ts_imin_med"], o["ts_imin_rsd"] = pct(mins, 0.5), rsd(mins)
    return o

KEYS = ["snd", "snd_sd", "snd_max", "ooo_sd", "ooo_med", "rf_cv", "dt", "qturn_med", "qturn_rsd", "calm", "qturn_calm", "qturn_calm_sd", "qown_calm", "qown_calm_sd", "qsnd_calm", "qsnd_calm_sd",
        "qctrl_calm", "qctrl_calm_sd", "ts_ip25_med", "ts_ip25_rsd", "ts_imin_med", "ts_imin_rsd", "qturn_p90", "ts_mean", "tbase", "turnbase", "tmin", "tlast", "spread", "hit_sd", "qsnd_n", "qctrl_n", "ts_imin", "T", "rs", "rs3", "drops", "p50", "p99", "qtrue", "qtrue90", "qturn", "qturn_sd", "ts_min", "ts_p25", "ts_p50", "ts_p90",
        "ts_ip25", "ts_ip25_sd", "ts_imin_sd", "qsnd", "qsnd_sd", "qsnd_dense", "qown", "qown_sd", "qown_dense", "qctrl", "qctrl_p50",
        "qctrl_sd", "qctrl_dense", "ooo", "hit", "lag", "inF", "ceil0", "tau", "tau_sd", "probertt", "pace", "win", "cpu", "n_eval", "ts_n"]

def cellkey(d):
    e = d["env"]
    return (("ul" if isup(d) else "dl"), int(e["Z5_DN_MBIT"]), int(e["Z5_RTT_MS"]), int(e["Z5_QMS"]), float(e["Z5_LOSS"]) * 100,
            float(d["ver"][1:]))

if __name__ == "__main__":
    R = load(os.environ.get("ASET", "A"))
    cells = {}
    for d in R:
        cells.setdefault(cellkey(d), []).append(runA(d))
    out = []
    for k in sorted(cells):
        rs = cells[k]
        m = {"cell": k, "n": len(rs)}
        for key in KEYS:
            v = [r.get(key) for r in rs if r.get(key) is not None]
            m[key] = st.median(v) if v else None
            m[key + "_runs"] = v
        out.append(m)
    json.dump(out, open(sys.argv[1] if len(sys.argv) > 1 else os.path.join(S, "res", "A-cells.json"), "w"))
    def f(x, nd=1):
        return "-" if x is None else f"{x:.{nd}f}"
    hdr = "dir path q loss W | n T rs rs3 p50 p99 | qtrue q90 | qturn(sd) ts_min ts_p25 ts_p50 ts_p90 ip25(sd) | qsnd(sd,n) qsndD | qown(sd) qownD | qctrl p50 qctrlD | ooo% hit lag F ceil0 | tau(sd) | prtt pace"
    print(hdr)
    for m in out:
        dr, mb, rtt, q, loss, w = m["cell"]
        print(f"{dr} {mb}/{rtt} q{q} l{loss:g} W{w:g} | {m['n']} {f(m['T'])} {f(m['rs'],2)} {f(m['rs3'],1)} {f(m['p50'],0)} {f(m['p99'],0)} | "
              f"{f(m['qtrue'])} {f(m['qtrue90'])} | {f(m['qturn'])}({f(m['qturn_sd'])}) {f(m['ts_min'])} {f(m['ts_p25'])} {f(m['ts_p50'])} {f(m['ts_p90'])} "
              f"{f(m['ts_ip25'])}({f(m['ts_ip25_sd'])}) | {f(m['qsnd'])}({f(m['qsnd_sd'])},{f(m['qsnd_n'] if 'qsnd_n' in m else None,0)}) {f(m['qsnd_dense'])} | "
              f"{f(m['qown'])}({f(m['qown_sd'])}) {f(m['qown_dense'])} | {f(m['qctrl'])} {f(m['qctrl_p50'])} {f(m['qctrl_dense'])} | "
              f"{f(m['ooo'],2)} {f(m['hit'],2)} {f(m['lag'],2)} {f(m['inF'],2)} {f(m['ceil0'],0)} | {f(m['tau'])}({f(m['tau_sd'])}) | {f(m['probertt'],0)} {f(m['pace'],0)}")
