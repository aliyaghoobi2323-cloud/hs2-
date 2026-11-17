import sys,re,datetime,gzip,collections
# dlan.py RUNDIR : per DL test, exit gate width sequence of the busiest gated conn + subs-with-data
R=sys.argv[1]
kh=gzip.open(R+'/logs/kh.full.log.gz','rt',errors='replace').read().splitlines()
tests=[l.split() for l in open(R+'/tests.txt') if 'dir=-R' in l]
def ts(s): h,m,x=s.split(':'); return int(h)*3600+int(m)*60+float(x)
g=collections.defaultdict(list); cut=collections.Counter()
for l in kh:
    m=re.match(r'(\S+) stripe gate (\w+): q=(\S+) base=(\S+) w=(\d+) live=(\d+)',l)
    if m: g[m.group(2)].append((ts(m.group(1)),m.group(3),int(m.group(5)),int(m.group(6))))
    m=re.match(r'(\S+) stripe cut (\w+):',l)
    if m: cut[(m.group(2),int(ts(m.group(1))))]+=1
for t in tests:
    st=float(t[2].split('=')[1]); d=datetime.datetime.utcfromtimestamp(st); s0=d.hour*3600+d.minute*60+d.second+d.microsecond/1e6
    c=[(k,[x for x in v if s0<=x[0]<=s0+13]) for k,v in g.items()]
    c=[x for x in c if x[1]]
    if not c: print(t[0],'no gate', ' '.join(t[4:])[:90]); continue
    k,v=max(c,key=lambda x:len(x[1]))
    ws=' '.join('%d/%d'%(x[2],x[3]) for x in v)
    nc=sum(n for (kk,s),n in cut.items() if kk==k and s0<=s<=s0+13)
    print(t[0],' '.join(t[4:])[9:100],'| w/live:',ws,'| cuts',nc)
