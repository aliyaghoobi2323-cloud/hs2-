import sys,re,datetime
# upan.py IRLOG TESTS.txt : for each upload test, the traced conn with most bytes; its events in first 8 s
log=open(sys.argv[1],errors='replace').read().splitlines()
tests=[l.split() for l in open(sys.argv[2]) if 'dir=up' in l]
def ts(s): h,m,x=s.split(':'); return int(h)*3600+int(m)*60+float(x)
ev={}
for l in log:
    m=re.match(r'(\S+) mtrace iran id=(\d+) \+\s*([\d.]+)s (.*)',l)
    if m: ev.setdefault(m.group(2),[]).append((ts(m.group(1)),float(m.group(3)),m.group(4)))
for t in tests:
    st=float(t[2].split('=')[1]); d=datetime.datetime.utcfromtimestamp(st); s0=d.hour*3600+d.minute*60+d.second+d.microsecond/1e6
    print('== test',t[0],' '.join(t[4:])[:120])
    best=None
    for k,v in ev.items():
        if not (s0-1<=v[0][0]-v[0][1]<=s0+2): continue
        mx=max([int(x) for e in v for x in re.findall(r'sent=(\d+)',e[2])]+[0])
        if best is None or mx>best[0]: best=(mx,k)
    if not best: print(' no trace'); continue
    for a,r,e in ev[best[1]]:
        if r>9: break
        if 'subs:' in e:
            e=' | '.join(re.sub(r'\S+>\S+ path=192.168.(\d+).1>\S+ first=\S+ dup=\S+ lag=\S+',r'p\1',x) for x in e.split(' | '))
        print(' %6.3f %s'%(r,e[:330]))
