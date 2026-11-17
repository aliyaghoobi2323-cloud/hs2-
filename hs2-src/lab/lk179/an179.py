import sys,re,os,statistics as S
K="/opt/hs2lab/lk179"
def kv(l): return dict(re.findall(r"(\w+)=([-\w.]+)",l))
for a in sys.argv[1:]:
    D=f"{K}/{a}"
    if not os.path.exists(D+"/gen.txt"): continue
    env=open(D+"/env.txt").read().split()[0:2]
    L=[(re.search(r"(storm|drop|c6) t=",l).group(1),kv(l)) for l in open(D+"/gen.txt") if re.search(r"(storm|drop|c6) t=",l)]
    act={l.split()[3]:int(l.split()[0]) for l in open(D+"/actions.txt") if "gen" in l}
    st=[d for t,d in L if t=="storm" and int(d["open_n"])>=1000]
    dr=[(int(float(d["t"])),d) for t,d in L if t=="drop"]
    def ev(lo,hi):
        w=[d for tt,d in dr if lo<tt<=hi]
        n=sum(int(d["open_n"]) for d in w); f=sum(int(d["open_fail"])+int(d["req_fail"]) for d in w)
        p99=max([float(d["open_p99"]) for d in w if int(d["open_n"])>0]+[-1]); mx=max([float(d["open_max"]) for d in w]+[-1])
        p50=max([float(d["open_p50"]) for d in w if int(d["open_n"])>100]+[-1])
        rq=max([float(d["req_p99"]) for d in w]+[-1])
        return f"opens={n} p50<={p50:.0f} p99<={p99:.0f} max={mx:.0f} fail={f} req_p99<={rq:.0f}"
    print(f"== {a} {env}")
    sw=[d for t,d in L if t=="storm" and int(d["open_n"])>0]
    if any(float(d["t"])<5 for d in sw):
        for lo in (0,50):
            w=[d for d in sw if lo<float(d["t"])<=lo+12]
            if w: print("  storm burst@%d: n=%d p50(win max)=%.0f p99(win max)=%.0f max=%.0f fail=%d"%(lo,sum(int(d["open_n"]) for d in w),max(float(d["open_p50"]) for d in w if int(d["open_n"])>100),max(float(d["open_p99"]) for d in w),max(float(d["open_max"]) for d in w),sum(int(d["open_fail"]) for d in w)))
    else:
        for d in st: print("  storm burst: n=%s p50=%s p99=%s max=%s fail=%s" % (d["open_n"],d["open_p50"],d["open_p99"],d["open_max"],d["open_fail"]))
    print("  drop1 (t95-103):",ev(94,103)); print("  drop2 (t145-153):",ev(144,153))
    stdy=[d for tt,d in dr if 20<tt<=60] or [d for t,d in L if t=="c6" and 10<float(d["t"])<=60]
    print("  steady (drop crowd t20-60, no storm): req_p50 med=%.1f req_p99 med=%.1f max=%.1f fails=%d"%(S.median(float(d["req_p50"]) for d in stdy),S.median(float(d["req_p99"]) for d in stdy),max(float(d["req_p99"]) for d in stdy),sum(int(d["req_fail"]) for d in stdy)))
    try:
        mins=[l.split() for l in open(D+"/min.txt")]
        print("  probe crowd per-min [min n p50 p99 fails call99 game99]:"," | ".join(" ".join(m) for m in mins))
    except Exception as e: pass
    c={"ir":[], "kh":[]}; cs={"ir":[], "kh":[]}
    for l in open(D+"/proc.txt"):
        p=l.split()
        if p[2] in("hs2","hs2kh"):
            t=int(p[0]); v=float(p[4].split("=")[1]); s=p[1]
            if 30<=t<=85: cs[s].append(v)
            if 88<=t<=175: c[s].append(v)
    print("  cpu%% steady edge=%.0f exit=%.0f | storm/drop phase edge=%.0f (max %.0f) exit=%.0f (max %.0f)"%(S.mean(cs["ir"]),S.mean(cs["kh"]),S.mean(c["ir"]),max(c["ir"]),S.mean(c["kh"]),max(c["kh"])))
    ld=[float(l.split()[1]) for l in open(D+"/load.txt") if 88<=int(l.split()[0])<=175]
    print("  load1 storm phase max=%.1f; panics ir=%s kh=%s"%(max(ld),open(D+"/ir.panics").read().strip() if os.path.exists(D+"/ir.panics") else "?",open(D+"/kh.panics").read().strip() if os.path.exists(D+"/kh.panics") else "?"))
