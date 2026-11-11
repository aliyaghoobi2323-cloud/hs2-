import json,sys,glob,os
for f in sorted(glob.glob(sys.argv[1]+"/u8-*/*-heavy.json")):
    n=os.path.basename(os.path.dirname(f))
    try: iv=json.load(open(f))["intervals"]
    except Exception: continue
    if not iv: continue
    ss=list(zip(*[x["streams"] for x in iv])); mp=[]
    for s in ss:
        b=[x["bits_per_second"]/1e6 for x in s][12:]
        z=m=0
        for v in b:
            z=z+1 if v<1 else 0; m=max(m,z)
        mp.append(m if max(b[-5:])>=1 else 99)
    print(n, "maxpause_s per flow (99=dead at end):", mp)
