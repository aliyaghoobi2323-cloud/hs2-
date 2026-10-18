# z11lib.py: loading and per-run measures for the window core rig (z11)
import json, os, statistics as st, math
from datetime import datetime

S = os.path.dirname(os.path.abspath(__file__))

def load(*names):
    out = []
    for n in names:
        f = os.path.join(S, "res", n + ".jsonl")
        if not os.path.exists(f):
            continue
        for l in open(f):
            if l.strip():
                d = json.loads(l)
                d["r"]["_id"] = d["id"]
                out.append(d["r"])
    return out

def pct(v, p):
    v = sorted(x for x in v if x is not None)
    if not v:
        return None
    x = p * (len(v) - 1)
    i = int(x)
    if i + 1 >= len(v):
        return v[-1]
    return v[i] + (x - i) * (v[i + 1] - v[i])

def mean(v):
    v = [x for x in v if x is not None]
    return sum(v) / len(v) if v else None

def sd(v):
    v = [x for x in v if x is not None]
    return st.pstdev(v) if len(v) > 1 else None

def ts(s):
    # RFC3339Nano UTC -> seconds
    s = s.rstrip("Z")
    if "." in s:
        a, b = s.split(".")
        s = a + "." + b[:6]
    return datetime.fromisoformat(s).timestamp()

def trace(rid, side=None):
    f = os.path.join(S, "res", "tr", rid + ".jsonl")
    if not os.path.exists(f):
        return []
    L = []
    for l in open(f):
        try:
            d = json.loads(l)
        except Exception:
            continue
        if side and d["side"] != side:
            continue
        d["_t"] = ts(d["t"])
        L.append(d)
    return L

def env(d, k, dflt=None):
    v = d["env"].get(k)
    return v if v is not None else dflt

def window(d):
    w0 = float(env(d, "Z5_WARM_S", 6))
    return w0, w0 + float(env(d, "Z5_DUR_S", 15))

def isup(d):
    return (d.get("upSum") or 0) > 0 and not d.get("fastSum")

def basic(d):
    """throughput, resend %, rs3, interactive, cpu, drops of the bulk direction"""
    o = {}
    up = isup(d)
    F, U = d.get("fastSum", 0) or 0, d.get("upSum", 0) or 0
    o["T"] = F + U
    i = 0 if up else 1
    if d.get("segsOut") and d["segsOut"][i] > 0:
        o["rs"] = 100 * d["retrans"][i] / d["segsOut"][i]
    # largest resend share of a 3 s window (as the loss rule counts)
    w0 = int(window(d)[0])
    if up:
        rt, so = d.get("trRtE"), d.get("trSoE")
    else:
        rt, so = d.get("trRt"), d.get("trSi")
    if rt:
        best = 0
        for k in range(w0 + 3, len(rt), 3):
            dr = rt[k] - rt[k - 3]
            ds = so[k] - so[k - 3]
            den = ds if up else ds + dr
            if den > 1000:
                best = max(best, dr / den)
        o["rs3"] = 100 * best
    if d["int"]["n"] > 0:
        o["p50"], o["p99"] = d["int"]["p50"], d["int"]["p99"]
    dur = float(env(d, "Z5_DUR_S", 15))
    mb = o["T"] * dur / 8
    if mb > 0:
        o["cpu"] = 1000 * d["cpuS"] / mb
        if "emuCpuS" in d:
            o["hcpu"] = 1000 * (d["cpuS"] - d["emuCpuS"]) / mb
    o["drops"] = d["drops"][0 if up else 1]
    return o

def t2win(d, key):
    t2 = d.get("t2") or {}
    v = t2.get(key)
    if not v:
        return []
    a, b = window(d)
    return v[int(a * 10):int(b * 10)]
