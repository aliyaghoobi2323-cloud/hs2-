# anaQ.py: Part A compact tables for the report (from res/A-cells.json, anaA.py's output):
# per (dir, path, queue, loss) a row per window; then the slopes per 0.1 BDP past the knee.
import json, os, sys
from z11lib import *
C = json.load(open(os.environ.get("ACELLS", os.path.join(S, "res", "A-cells.json"))))
def f(x, nd=1):
    return "-" if x is None else f"{x:.{nd}f}"
groups = {}
for m in C:
    dr, mb, rtt, q, loss, w = m["cell"]
    groups.setdefault((dr, mb, rtt, q, loss), []).append(m)
which = sys.argv[1] if len(sys.argv) > 1 else "main"
for g, ms in groups.items():
    dr, mb, rtt, q, loss = g
    print(f"\n### {dr} {mb}/{rtt} صف {q}ms اتلاف {loss:g}%  (n={ms[0]['n']})")
    if which == "main":
        print("| W (BDP) | T | rs% | p50/p99 | q واقعی (میانگین/p90) | q_turn (min) | چرخش p25/p50 | q_snd | q_own | q_ctrl (میانه) | ooo% | hit | F | τ_hold | PROBE_RTT |")
        print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
        for m in ms:
            w = m["cell"][5]
            bad = "*" if (m["ceil0"] or 0) > 0.1 * (m["n_eval"] or 1) else ""
            qs = m["qsnd"] if dr == "dl" else m["qsnd_dense"]
            qc = m["qctrl_p50"] if dr == "dl" else m["qctrl_dense"]
            print(f"| {w:g}{bad} | {f(m['T'],0)} | {f(m['rs'],2)} | {f(m['p50'],0)}/{f(m['p99'],0)} | {f(m['qtrue'])}/{f(m['qtrue90'])} | "
                  f"{f(m['qturn_med'])} | {f(m['ts_p25'])}/{f(m['ts_p50'])} | {f(qs)} | {f(m['qown'])} | {f(qc)} | "
                  f"{f(m['ooo'],0)} | {f(m['hit'],2)} | {f(m['inF'],2)} | {f(m['tau'],0)} | {f(m['probertt'],0)} |")
    else:
        print("| W | sd q_turn (دوره، مقاوم) | sd q_own | sd q_snd | sd p25 چرخش (دوره) | CV نرخ دوره | q_turn آرام/sd | q_own آرام/sd | سهم دوره‌های آرام |")
        print("|---|---|---|---|---|---|---|---|---|")
        for m in ms:
            w = m["cell"][5]
            print(f"| {w:g} | {f(m['qturn_rsd'])} ({f(m['qturn_sd'])}) | {f(m['qown_sd'])} | {f(m['qsnd_sd'])} | {f(m['ts_ip25_rsd'])} | {f(m['rf_cv'],2)} | "
                  f"{f(m['qturn_calm'])}/{f(m['qturn_calm_sd'])} | {f(m['qown_calm'])}/{f(m['qown_calm_sd'])} | {f(m['calm'],2)} |")
