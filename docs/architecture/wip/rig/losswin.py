# losswin.py FILE.jsonl: per run, the download loss fraction per 3 s window as
# judgeDownLoss counts it (exit retrans / (edge segs in + retrans)), and the
# longest run of windows over 12% (the LinkManager degrades at 3)
import json, sys
for f in sys.argv[1:]:
    for l in open(f):
        if not l.strip(): continue
        d = json.loads(l)
        r = d.get("r", d)
        rt, si = r.get("trRt"), r.get("trSi")
        if not rt: continue
        fr = []
        for i in range(3, len(rt), 3):
            dr, ds = rt[i]-rt[i-3], si[i]-si[i-3]
            fr.append(dr/(ds+dr) if ds+dr > 0 else 0)
        run = best = 0
        for x in fr:
            run = run+1 if x > 0.12 else 0
            best = max(best, run)
        tot = (rt[-1]-rt[2])/max(1,(si[-1]-si[2])+(rt[-1]-rt[2]))
        up = ""
        re, so = r.get("trRtE"), r.get("trSoE")
        if re and so and so[-1]-so[2] > 1000:
            uf = []
            for i in range(2, len(re), 2):
                dr, ds = re[i]-re[i-2], so[i]-so[i-2]
                uf.append(dr/ds if ds > 0 else 0)
            ur = ub = 0
            for x in uf:
                ur = ur+1 if x > 0.12 else 0
                ub = max(ub, ur)
            up = f" | up maxrun={ub} " + " ".join(f"{x*100:.0f}" for x in uf)
        print(f'{r["scen"]:<22} {r["ver"]:<8} rep{r["rep"]:<2} F={r["fastSum"]:6.1f} tot={tot*100:4.1f}% maxrun={best:2d}  ' + " ".join(f"{x*100:.0f}" for x in fr) + up)
