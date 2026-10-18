# anaL.py: full edge stack (TestZ5LM, real LinkManager + RunKharej), lone transfer 130 s:
# mean Mbit/s per run, cuts, verdict lines, and the window core's lines and trace per core run.
import re
from z11lib import *

R = load("L")
by = {}
for d in R:
    by.setdefault(d["scen"], {}).setdefault(d["ver"], []).append(d)
for sc in sorted(by):
    print("==", sc)
    for v in ("base", "core"):
        ds = sorted(by[sc].get(v, []), key=lambda d: d["rep"])
        for d in ds:
            cells = []
            for u in d["users"]:
                m = u["mbit1s"]
                mean_ = sum(m) / max(1, len(m))
                cut = u["endedAtS"] >= 0
                cells.append(f"{mean_:.1f}" + (f" CUT@{u['endedAtS']:.0f}s ({u.get('err','')})" if cut else "") +
                             f" [0-30s {sum(m[:30])/30:.0f}, 30-130s {sum(m[30:])/max(1,len(m[30:])):.0f}]")
            verd = {}
            wc = []
            for x in d["lines"]:
                for w, pat in (("degraded", r"link \d+ degraded \("), ("stuck", r"link \d+ stuck:"), ("poor", r"poor"),
                               ("closed", r"closed"), ("lossy", r"lossy")):
                    if re.search(pat, x):
                        verd[w] = verd.get(w, 0) + 1
                if "window" in x or "fill the path" in x:
                    wc.append(x)
            print(f"   {v} r{d['rep']}: {'; '.join(cells)}  links={d['linksAtEnd']} verdicts={verd or 'none'}")
            for x in wc[:14]:
                print("        " + x[:260])
            other = [x for x in d["lines"] if x not in wc]
            for x in other[:8]:
                print("      | " + x[:220])
            if v == "core":
                for side in ("edge", "exit"):
                    L = trace(d["_id"], side)
                    if not L:
                        continue
                    t0 = ts(d["t0"]) if d.get("t0") else L[0]["_t"]
                    seq = []
                    for x in L:
                        if x["n_f"] == 0 and x["state"] == "off":
                            continue
                        if not seq or seq[-1][1] != x["state"] or seq[-1][3] != x["link"]:
                            seq.append((round(x["_t"] - t0, 1), x["state"], round(x["b"] / 2**20, 1), x["link"]))
                    ev = [(round(x["_t"] - t0, 1), e.split()[0]) for x in L for e in (x.get("events") or [])]
                    print(f"      trace {side}: {len(seq)} state changes; first: {seq[:16]}")
                    print(f"      events {side}: {len(ev)}: {ev[:30]}")
