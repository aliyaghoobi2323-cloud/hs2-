# anaLMZ.py SET[,SET...] [-v]: z13 TestZ5LM runs (real LinkManager + RunKharej) per cell and version:
# each user's mean Mbit/s (over the first min(dur, 120) s: the rig's bins hold 120 s) and whether
# its connection was cut; every verdict line counted (poor, on par, degraded, stuck, lossy, nothing
# received, closed, retired, exit-side link down, rescue moves) with the link it named; the
# multi-link cell's interactive users (on the download's link or not, latency); the gap-2 cells
# (the black-holed link, the downloads before and after, the short users' time to first byte, where
# they landed, failures) and the gap-3 cell (the link made worse, where the download and the
# interactive users landed, which links were judged poor).
import re, sys, os, json, statistics as st
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from z11lib import load, pct

PAT = [("poor", r"link (\d+): .*new connections go to the other links"),
       ("onpar", r"link (\d+): on par with the other links"),
       ("degraded", r"link (\d+) degraded"),
       ("stuck", r"link (\d+) stuck:"),
       ("lossy", r"lossy"),
       ("nothing", r"link (\d+): nothing received"),
       ("closed", r"closed (\d+) connection"),
       ("retired", r"retired (\d+) drained"),
       ("exitdown", r"exit: link down from"),
       ("moved", r"(\d+) new connection\(s\) got no answer"),
       ("core", r"window|fill the path|queue grew|packets (were lost|arrived)")]

def verdicts(lines):
    v = {}
    for x in lines or []:
        for k, p in PAT:
            m = re.search(p, x)
            if m:
                e = v.setdefault(k, [])
                e.append(m.group(1) if m.groups() and m.group(1) else "")
    return v

def umean(u, a=0, b=None):
    m = u["mbit1s"][:120]
    b = len(m) if b is None else min(b, len(m))
    seg = m[a:b]
    return sum(seg) / len(seg) if seg else 0

def run_line(d, verbose=False):
    out = []
    dur = float(d["env"].get("Z5_LM_DUR_S", 130))
    us = []
    for u in d["users"]:
        s = f"{umean(u):.1f}"
        if u.get("link") is not None:
            s += f"@L{u['link']}"
        if u["endedAtS"] >= 0:
            s += f" CUT@{u['endedAtS']:.1f}s({u.get('err','')[:30]})"
        us.append(s)
    v = verdicts(d["lines"])
    vs = " ".join(f"{k}:{len(x)}" + (f"(L{','.join(sorted(set(i for i in x if i)))})" if k in ("poor", "onpar", "degraded", "stuck", "nothing") and any(x) else "")
                  for k, x in v.items() if k != "core")
    out.append(f"r{d['rep']}: users {'; '.join(us)} | links end {d['linksAtEnd']} | verdicts {vs or 'none'} | core lines {len(v.get('core', []))}")
    return out, v

def exitlines(d):
    """a split run's exit-side lines (res/raw/<id>.exit.txt)"""
    f = os.path.join(os.path.dirname(os.path.abspath(__file__)), "res", "raw", d["_id"] + ".exit.txt")
    if not os.path.exists(f):
        return []
    for l in open(f):
        if l.startswith("Z5EXIT "):
            return json.loads(l[7:]).get("exitLines") or []
    return []

def summarize(sets, verbose=False):
    R = load(*sets)
    for d in R:
        d["lines"] = (d.get("lines") or []) + exitlines(d)
    by = {}
    for d in R:
        by.setdefault(d["scen"], {}).setdefault(d["ver"], []).append(d)
    out = []
    for sc in sorted(by):
        out.append(f"== {sc}")
        for ver in sorted(by[sc]):
            ds = sorted(by[sc][ver], key=lambda d: d["rep"])
            means, cuts, allv = [], 0, {}
            for d in ds:
                ls, v = run_line(d, verbose)
                for k, x in v.items():
                    allv[k] = allv.get(k, 0) + len(x)
                for u in d["users"]:
                    means.append(umean(u))
                    cuts += u["endedAtS"] >= 0
                out += [f"   {ver:10} " + l for l in ls]
                kind = sc.split("@")[0]
                if d.get("ints"):
                    for it in d["ints"]:
                        lat = it.get("lat") or {}
                        out.append(f"        int at {it['atS']:.0f}s: link L{it.get('link')} sameDl {it['sameDl']} bad {it.get('bad', False)} links {it['links']} "
                                   f"dlPressed {it['dlPressed']} lat p50 {lat.get('p50')} p99 {lat.get('p99')} n {lat.get('n')}")
                if kind in ("lmcut", "lmcuti"):
                    ca = d.get("cutAtS") or 0
                    sh = d.get("shorts") or []
                    aft = [s for s in sh if s["atS"] >= ca]
                    bef = [s for s in sh if s["atS"] < ca]
                    fails = [s for s in sh if s["ttfbMs"] < 0]
                    oncut = [s for s in aft if s.get("cut")]
                    tt = [s["ttfbMs"] for s in aft if s["ttfbMs"] >= 0]
                    tb = [s["ttfbMs"] for s in bef if s["ttfbMs"] >= 0]
                    ul = []
                    for u in d["users"]:
                        ul.append(f"L{u.get('link')} before {umean(u, max(0, int(ca) - 10), int(ca)):.0f} after {umean(u, int(ca)+1, int(ca)+20):.0f}"
                                  + (f" ended {u['endedAtS']:.1f}s" if u['endedAtS'] >= 0 else " to the end"))
                    out.append(f"        cut L{d.get('cutId')} at {ca}s, drops up/down {d.get('cutDrops')}; downloads: {'; '.join(ul)}")
                    out.append(f"        short users: before {len(bef)} (ttfb p50 {pct(tb, .5) and round(pct(tb, .5))}), after {len(aft)}: fails {len(fails)}, "
                               f"on the cut link {len(oncut)} (ttfb {[s['ttfbMs'] for s in oncut]}), ttfb p50 {pct(tt, .5) and round(pct(tt, .5))} "
                               f"p99 {pct(tt, .99) and round(pct(tt, .99))} max {max(tt) if tt else None}; links after {sorted(set(s['link'] for s in aft))}")
                    if fails:
                        out.append(f"        failed: {[(s['atS'], s['link'], s.get('err','')[:40]) for s in fails][:8]}")
                if kind == "lmbad":
                    dl = d["users"][0] if d["users"] else None
                    poor = v.get("poor", [])
                    bad = str(d.get("badId"))
                    good = [str(l["id"]) for l in d.get("pool0") or [] if str(l["id"]) != bad]
                    out.append(f"        bad link L{bad} (+{d['env'].get('Z5_LM_BAD_MS')} ms); download on L{dl and dl.get('link')} "
                               f"({'the bad one' if dl and str(dl.get('link')) == bad else 'a good one'}); poor verdicts: bad {poor.count(bad)} good {sum(poor.count(g) for g in good)}; "
                               f"on par: {v.get('onpar', [])}")
            if verbose:
                for d in ds:
                    for x in d["lines"]:
                        out.append("        " + x[:220])
            out.append(f"   {ver:10} => mean {st.median(means):.1f} Mbit/s (median of {len(means)} users), cuts {cuts}, verdict lines {allv}")
    return out

if __name__ == "__main__":
    print("\n".join(summarize(sys.argv[1].split(","), "-v" in sys.argv)))
