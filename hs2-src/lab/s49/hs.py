import json,sys,glob,os
for f in sorted(glob.glob(sys.argv[1]+"/*/*-heavy.json")):
    n=f.split('/')[-2]
    try: d=json.load(open(f))
    except Exception: print(n,"bad json"); continue
    iv=d.get("intervals",[])
    s=[round(i["sum"]["bits_per_second"]/1e6) for i in iv]
    post=s[13:] if len(s)>13 else []
    fz = len(post)>=5 and max(post[3:] or [0])<5
    print(f"{n:22s} {'FROZEN' if fz else 'ok    '} {s}")
