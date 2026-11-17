import json,sys,os,statistics as st,glob,subprocess
def pct(v,p):
    v=sorted(v); return v[min(len(v)-1,int(p*len(v)))] if v else 0
for d in sys.argv[1:]:
    dn=[];up=[];low=0;stalls=0;stl=[];slowstart=0
    for l in open(d+"/tests.txt"):
        f=l.split(); t=f[0]; dirn=f[3].split("=")[1]
        try: j=json.load(open("%s/tests/t%s.json"%(d,t))); iv=[round(i["sum"]["bits_per_second"]/1e6) for i in j["intervals"]]; tot=j["end"]["sum_received"]["bits_per_second"]/1e6
        except Exception: continue
        if dirn=="u": up.append(tot); continue
        dn.append(tot); low+=sum(1 for x in iv[1:] if x<100)
        if max(iv[:3])<80: slowstart+=1
        best=0
        for k,x in enumerate(iv):
            if k>0 and best>0 and x<0.3*best and x<200 and k<len(iv)-1:
                stalls+=1; stl.append("t%s:%s"%(t,",".join(map(str,iv[max(0,k-2):k+2])))); break
            best=max(best,x)
    print("%s down n=%d mean %.0f p10 %.0f min %.0f | sec<100 after1st %d | stalls %d %s | slow-start(first3<80) %d | up n=%d mean %.0f p10 %.0f"%(os.path.basename(d),len(dn),st.mean(dn),pct(dn,0.1),min(dn),low,stalls," ".join(stl),slowstart,len(up),st.mean(up) if up else 0,pct(up,0.1)))
