# anaB.py: Part B (base vs core): the decision table, median [runs], with the acceptance targets,
# and per core run what its window core did (from its trace).
import sys, re
from z11lib import *

# N4 and base of the delay round (z5-round2-delay-results.fa.md), for the gain target
N4OLD = {"dl1i@300-150": (70.8, 172), "ul1i@300-150": (89.8, 174), "dl1i@1000-100": (110, 496), "dl1i@100-100": (82.8, 66.7),
         "dl1i@300-150-l0.1": (51.1, 146), "dl1i@1000-100-l0.1": (75.6, 342), "dl4@300-150": (167, 191)}

def cell(xs, nd=None):
    xs = [x for x in xs if x is not None]
    if not xs:
        return "-"
    m = st.median(xs)
    if nd is None:
        nd = 0 if abs(m) >= 100 else 1
    s = f"{m:.{nd}f}"
    if len(xs) > 1:
        s += "[" + " ".join(f"{x:.{nd}f}" for x in xs) + "]"
    return s

def med(xs):
    xs = [x for x in xs if x is not None]
    return st.median(xs) if xs else None

def chg(d):
    """speed change 300 -> 50 -> 300 (18-30 s at 50): per phase Mbit/s, back to 90% of the old rate"""
    f1 = d.get("fast1s") or d.get("up1s") or []
    o = {}
    if len(f1) > 41:
        o["pre"] = mean(f1[7:18]); o["low"] = mean(f1[19:30]); o["post"] = mean(f1[31:42])
        o["back90"] = next((i - 30 for i in range(30, len(f1)) if f1[i] >= 0.9 * o["pre"]), None)
        o["back90low"] = next((i - 18 for i in range(18, 30) if f1[i] <= 1.1 * 50), None)
        rt, si = d.get("trRt"), d.get("trSi")
        if rt and len(rt) > 30:
            dr, ds = rt[30] - rt[18], si[30] - si[18]
            o["rslow"] = 100 * dr / max(1, ds + dr)
            dr, ds = rt[-1] - rt[6], si[-1] - si[6]
            o["rsall"] = 100 * dr / max(1, ds + dr)
    return o

def wctrace(d, kind="B"):
    """what the window core did in a core run: states over time, events, B in BDP units"""
    rid = d["_id"]
    up = isup(d)
    side = "exit" if up else "edge"
    L = trace(rid, side)
    if not L:
        return None
    t0 = ts(d["t0"]) if d.get("t0") else L[0]["_t"]
    e = d["env"]
    mb = float(e["Z5_UP_MBIT" if up else "Z5_DN_MBIT"])
    rtt = float(e["Z5_RTT_MS"])
    bdp = mb * 1e6 / 8 * rtt / 1000
    seq = []
    for x in L:
        t = x["_t"] - t0
        if not seq or seq[-1][1] != x["state"]:
            seq.append((t, x["state"], x["b"] / bdp if x["b"] else 0))
    o = {"seq": seq, "bdp": bdp}
    ev = []
    for x in L:
        for s in x.get("events") or []:
            ev.append((x["_t"] - t0, s.split()[0], x))
    o["events"] = ev
    hold = [t for t, s, b in seq if s == "hold"]
    o["t_hold"] = hold[0] if hold else None
    st_ = [t for t, s, b in seq if s == "startup"]
    o["t_startup"] = st_[0] if st_ else None
    # B in HOLD (BDP units), over the measure window
    w0, w1 = window(d)
    hb = [x["b"] / bdp for x in L if x["state"] == "hold" and w0 <= x["_t"] - t0 <= w1 and x["b"] > 0]
    o["B_hold"] = med(hb)
    o["B_hold_max"] = max(hb) if hb else None
    allb = [x["b_pub"] / bdp for x in L if w0 <= x["_t"] - t0 <= w1]
    o["Bpub_med"] = med(allb)
    o["Bpub_max"] = max(allb) if allb else None
    # time per state in the window
    tin = {}
    for a, b in zip(L, L[1:]):
        ta, tb_ = a["_t"] - t0, b["_t"] - t0
        if tb_ <= w0 or ta >= w1:
            continue
        tin[a["state"]] = tin.get(a["state"], 0) + min(tb_, w1) - max(ta, w0)
    o["time_in"] = tin
    # OFF with fast streams: the "do no harm" rule (q_snd past target) or waiting
    offF = [x for x in L if x["state"] == "off" and x["n_f"] > 0]
    o["off_with_F"] = len(offF)
    o["off_qsnd"] = [round(x["q_snd_ms"], 1) for x in offF if x["q_snd_ok"]][:20]
    # drains: true queue at the drain (emulator, 100 ms samples) and drops just before
    qk, dk = ("qu", "dropu") if up else ("qd", "dropd")
    t2 = d.get("t2") or {}
    qv, dv = t2.get(qk) or [], t2.get(dk) or []
    drains = []
    for t, k, x in ev:
        if not k.startswith("drain"):
            continue
        i = int(t * 10)
        qwin = qv[max(0, i - 6):i + 1]
        dwin = dv[max(0, i - 10):i + 1]
        dd = (dwin[-1] - dwin[0]) if len(dwin) > 1 else None
        drains.append({"t": round(t, 1), "why": k, "qtrue_max": max(qwin) if qwin else None, "qtrue_mean": mean(qwin),
                       "drops1s": dd, "q_turn": round(x["q_turn_ms"], 1), "q_ref": round(x["q_ref_ms"], 1),
                       "q_snd": round(x["q_snd_ms"], 1) if x["q_snd_ok"] else None, "ooo": round(100 * x["ooo"], 2), "state": x["state"]})
    o["drains"] = drains
    o["false_drains"] = [x for x in drains if (x["qtrue_max"] or 0) < 10 and not x["drops1s"]]
    # probes: a probe evaluation followed by what
    probes = []
    for a, b in zip(L, L[1:]):
        if a["state"] == "probe" and b["state"] != "probe":
            probes.append((round(b["_t"] - t0, 1), b["state"]))
    o["probes"] = probes
    o["refresh"] = sum(1 for a, b in zip(L, L[1:]) if a["state"] == "hold" and b["state"] == "drain" and not (b.get("events")))
    return o

if __name__ == "__main__":
    R = load(*(sys.argv[1:] or ["B"]))
    by = {}
    for d in R:
        by.setdefault(d["scen"], {}).setdefault(d["ver"], []).append(d)
    order = ["dl1i@300-150", "ul1i@300-150", "dl1i@1000-100", "dl1i@100-100", "dl1i@300-150-l0.1", "dl1i@1000-100-l0.1", "dlchg@300-150", "dl4@300-150"]
    for sc in order + [s for s in by if s not in order]:
        if sc not in by:
            continue
        V = by[sc]
        print(f"== {sc}  (n base={len(V.get('base', []))} core={len(V.get('core', []))})")
        M = {}
        for v in ("base", "core", "off", "fix", "fixtr"):
            ds = sorted(V.get(v, []), key=lambda d: d["rep"])
            if not ds:
                continue
            ms = [basic(d) for d in ds]
            M[v] = {k: [m.get(k) for m in ms] for k in ["T", "rs", "rs3", "p50", "p99", "cpu", "hcpu", "drops"]}
            print(f"   {v:<5} " + "  ".join(f"{k}={cell(M[v][k], 2 if k in ('rs', 'rs3') else None)}" for k in M[v]))
            if sc.startswith("dlchg"):
                cs = [chg(d) for d in ds]
                print("         chg: " + "  ".join(f"{k}={cell([c.get(k) for c in cs])}" for k in ["pre", "low", "post", "back90", "back90low", "rslow", "rsall"]))
        if "base" in M and "core" in M:
            b = {k: med(v) for k, v in M["base"].items()}
            c = {k: med(v) for k, v in M["core"].items()}
            fl = []
            if sc in N4OLD and b["T"] and c["T"]:
                b0, n4 = N4OLD[sc]
                if n4 > 1.1 * b0:
                    g = (c["T"] - b["T"]) / (n4 - b0)
                    fl.append(f"G {100*g:.0f}% of N4's old gain")
                fl.append(f"S {c['T']/b['T']:.2f}x")
            if b["rs3"] is not None and c["rs3"] is not None:
                fl.append(f"R(rs3) {c['rs3']-b['rs3']:+.2f}pp {'ok' if c['rs3'] <= b['rs3'] + 3 else 'FAIL'}")
            if b["rs"] is not None and c["rs"] is not None:
                fl.append(f"rs {c['rs']-b['rs']:+.2f}pp {'ok' if c['rs'] <= b['rs'] + 0.5 else 'over+0.5'}")
            if b["p50"] and c["p50"] and b["p99"] and c["p99"]:
                r50, r99 = c["p50"] / b["p50"], c["p99"] / b["p99"]
                fl.append(f"L {r50:.2f}x/{r99:.2f}x {'ok' if max(r50, r99) <= 1.3 else 'FAIL'}")
            if b["cpu"] and c["cpu"]:
                fl.append(f"C {c['cpu']/b['cpu']:.2f}x {'ok' if c['cpu'] <= 1.1 * b['cpu'] else 'FAIL'}")
            print("   => " + "; ".join(fl))
        for d in sorted(V.get("core", []), key=lambda d: d["rep"]):
            o = wctrace(d)
            if not o:
                continue
            seq = " ".join(f"{s}@{t:.1f}" + (f"(B{b:.2f})" if b else "") for t, s, b in o["seq"][:40])
            print(f"   core r{d['rep']}: t_startup={o['t_startup'] and round(o['t_startup'],1)} t_hold={o['t_hold'] and round(o['t_hold'],1)} "
                  f"B_hold={o['B_hold'] and round(o['B_hold'],2)}BDP (max {o['B_hold_max'] and round(o['B_hold_max'],2)}) Bpub med/max={o['Bpub_med'] and round(o['Bpub_med'],2)}/{o['Bpub_max'] and round(o['Bpub_max'],2)} "
                  f"time_in={ {k: round(v,1) for k,v in o['time_in'].items()} } offF={o['off_with_F']} off_qsnd={o['off_qsnd'][:8]} probes={o['probes']} refresh={o['refresh']}")
            print(f"      seq: {seq}")
            for x in o["drains"]:
                print(f"      drain {x}")
            if d.get("wcLines"):
                for l in d["wcLines"][:12]:
                    print("      log: " + l)
