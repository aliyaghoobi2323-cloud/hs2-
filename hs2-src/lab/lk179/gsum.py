import sys,re,collections
# gsum.py FILE [filter-regex]: goroutine debug=1 groups; prints top groups (count, top frames) matching filter
f=sys.argv[1]; flt=re.compile(sys.argv[2]) if len(sys.argv)>2 else None
txt=open(f,errors='replace').read()
hdr=txt.split('\n',1)[0]
groups=txt.split('\n\n')
agg=collections.Counter()
for g in groups:
    ls=g.strip().split('\n')
    m=re.match(r'(\d+) @',ls[0]) if ls else None
    if not m: continue
    n=int(m.group(1))
    fr=[]
    for l in ls[1:]:
        mm=re.match(r'#\s+0x[0-9a-f]+\s+(\S+)\+0x[0-9a-f]+\s+(\S+):(\d+)',l)
        if mm: fr.append((mm.group(1).split('/')[-1], mm.group(2).split('/')[-1]+':'+mm.group(3)))
    s=' '.join(a for a,_ in fr)
    if flt and not flt.search(s): continue
    # skip runtime frames at top
    i=0
    while i<len(fr)-1 and (fr[i][0].startswith('runtime.') or fr[i][0].startswith('internal/') or fr[i][0].startswith('sync.') ): i+=1
    key=' < '.join(f'{a}@{b}' for a,b in fr[max(0,i-1):i+4])
    agg[key]+=n
print(hdr, 'matched', sum(agg.values()))
for k,v in agg.most_common(int(sys.argv[3]) if len(sys.argv)>3 else 15): print(f'{v:7d} {k}')
