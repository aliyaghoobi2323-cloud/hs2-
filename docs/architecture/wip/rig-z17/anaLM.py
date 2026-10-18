# anaLM.py SET[,SET...] [-v]: TestZ5LM runs (real LinkManager + RunKharej): per run the mean Mbit/s,
# whether the transfer was cut, the link verdicts (degraded / stuck / closed / lossy), the core's
# state changes and events (with -v), and for the multi-link cell (Z5_LM_INTS) where each new
# interactive user landed (on the download's link or not) and its latency.
import re, sys, json, statistics as st
sys.path.insert(0, "/tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z12")
from z11lib import load

sets = sys.argv[1].split(",")
verbose = "-v" in sys.argv
R = load(*sets)
by = {}
for d in R:
    by.setdefault(d["scen"], {}).setdefault(d["ver"], []).append(d)
for sc in sorted(by):
    print("==", sc)
    for v in sorted(by[sc]):
        ds = sorted(by[sc][v], key=lambda d: d["rep"])
        means, cuts = [], 0
        for d in ds:
            cells = []
            for u in d["users"]:
                m = u["mbit1s"]
                mean_ = sum(m) / max(1, len(m))
                means.append(mean_)
                cut = u["endedAtS"] >= 0
                cuts += cut
                cells.append(f"{mean_:.1f}" + (f" CUT@{u['endedAtS']:.0f}s ({u.get('err','')})" if cut else "") +
                             f" [0-30s {sum(m[:30])/max(1,len(m[:30])):.0f}, 30s- {sum(m[30:])/max(1,len(m[30:])):.0f}]")
            verd = {}
            wc = []
            for x in d["lines"]:
                for w, pat in (("degraded", r"link \d+ degraded \("), ("stuck", r"link \d+ stuck:"), ("poor", r"poor"),
                               ("link down (idle links reaped to Z5_LM_MIN)", r"link down"), ("lossy", r"lossy")):
                    if re.search(pat, x):
                        verd[w] = verd.get(w, 0) + 1
                if "window" in x or "fill the path" in x or "queue grew" in x or "loss" in x:
                    wc.append(x)
            print(f"   {v} r{d['rep']}: {'; '.join(cells)}  links={d['linksAtEnd']} verdicts={verd or 'none'} core lines={len(wc)}")
            for it in d.get("ints") or []:
                lat = it.get("lat") or {}
                print(f"        int at {it['atS']:.0f}s: on the download's link {it['sameDl']}, links {it['links']}, "
                      f"download link pressed {it['dlPressed']}, lat p50 {lat.get('p50')} p99 {lat.get('p99')} n {lat.get('n')}")
            if verbose:
                for x in wc[:20]:
                    print("        " + x[:240])
        if means:
            print(f"   {v}: mean {st.median(means):.1f} Mbit/s (median of {len(means)}), cuts {cuts}")
