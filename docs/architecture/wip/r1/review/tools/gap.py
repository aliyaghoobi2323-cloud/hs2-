import sys, gzip, glob, os, datetime
def ep(hms):  # HH:MM:SS.mmm on 2026-10-29 UTC
    h, m, s = hms.split(":")
    return datetime.datetime(2026,10,29,int(h),int(m),tzinfo=datetime.timezone.utc).timestamp()+float(s)
for d in sorted(glob.glob(sys.argv[1]+"/*/")):
    t0 = float(open(d+"events.txt").readline().split()[0])
    rs = []
    for u in ("hs2-uge","hs2-ugx"):
        for l in open(d+"events-%s.txt"%u):
            p = l.split()
            if p[1] == "start" and ep(p[0]) >= t0 - 1:
                rs.append((ep(p[0]), u[-1]))
    rs.sort()
    first, second = rs[0], rs[1]
    pr = [l.split() for l in gzip.open(d+"probe.log.gz","rt") if l.strip()]
    pr = [(float(a), b) for a, b, *_ in pr]
    comp = None
    for i, (t, s) in enumerate(pr):
        if t >= second[0] and s == "ok" and i + 1 < len(pr):
            comp = pr[i+1][0]; break
    gap = second[0]-first[0]
    print("%-12s restarts %s then %s %+.2f s; service gap %.2f s (first restart to the first new connection served after both)" % (os.path.basename(d.rstrip("/")), first[1], second[1], gap, comp-first[0] if comp else -1))
