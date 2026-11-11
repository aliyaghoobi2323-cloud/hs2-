import json,sys,glob,os
# per run: per stream, seconds from the kill (interval 12) until it carries >5 Mbit/s
# again and whether it is alive in the last 5 s; frozen = all streams 0 in the last 5 s
K=12
for f in sorted(glob.glob(sys.argv[1]+"/u8-*/*-heavy.json")):
    n=os.path.basename(os.path.dirname(f))
    try: j=json.load(open(f)); iv=j["intervals"]
    except Exception: print(n,"bad json"); continue
    ss=list(zip(*[x["streams"] for x in iv]))
    ttr=[];dead=0;stall=0
    for s in ss:
        b=[x["bits_per_second"]/1e6 for x in s]
        r=next((i-K for i in range(K,len(b)) if b[i]>5),None)
        ttr.append(r)
        if max(b[-5:])<1: dead+=1
        # a stall: >=3 s at 0 after it resumed, then alive again or not
        if r is not None:
            z=0;mz=0
            for v in b[K+r:]:
                z=z+1 if v<1 else 0; mz=max(mz,z)
            if mz>=3: stall+=1
    tot=[round(i["sum"]["bits_per_second"]/1e6) for i in iv]
    post=tot[K+1:]
    fz=len(post)>=5 and max(post[-5:])<5
    print(f"{n:16s} {'FROZEN' if fz else 'ok    '} dead_at_end={dead}/8 stalls>=3s={stall} ttr={ttr} post_mean={sum(post)/max(1,len(post)):.0f}")
