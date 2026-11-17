#!/usr/bin/env python3
# dr258 forced-stuck upload: find the upload's raw first sub (an ir-side tunnel link that was idle
# for the second before the test and then sends >= FZB bytes) and drop every data packet
# (IP length >= 128) Iran->kharej on that link's path for FZ seconds (ACKs/keepalives still pass).
# usage: fz.py LAB_ID OUTLOG FZB FZSEC [N]   (start ~1 s before the iperf3 test)
# N > 0: drop 1 in N data packets at random instead of all (heavy loss, not a blackout)
import subprocess, sys, time, re
ID, LOG, FZB, FZS = sys.argv[1], sys.argv[2], int(sys.argv[3]), float(sys.argv[4])
FZN = int(sys.argv[5]) if len(sys.argv) > 5 else 0
ACT = ['action','gact','pass','random','netrand','drop',str(FZN)] if FZN > 0 else ['action','drop']
def snap():
    o = subprocess.run(['ip','netns','exec','ir'+ID,'ss','-tinH','state','established'],capture_output=True,text=True).stdout
    d = {}; key = None
    for l in o.splitlines():
        if not l.startswith((' ','\t')):
            f = l.split()
            key = (f[2], f[3]) if len(f) >= 4 else None
            if key and ('127.0.0.1' in key[0] or '[::1]' in key[0]): key = None
        elif key:
            m = re.search(r'bytes_sent:(\d+)', l)
            if m: d[key] = int(m.group(1))
    return d
def path(local):
    if local.startswith('[fd00:51::1]'): return 1, True
    m = re.match(r'192\.168\.5(\d)\.1:', local)
    return (int(m.group(1)), False) if m else (None, False)
t0 = time.time(); pre = snap(); time.sleep(1.0); base = snap()
idle = {k for k, v in base.items() if v - pre.get(k, v) < 200000}
out = open(LOG, 'a'); hit = None
while time.time() - t0 < 8:
    s = snap()
    for k, v in s.items():
        if (k in idle or k not in base) and v - base.get(k, 0) >= FZB:
            hit = (k, v - base[k]); break
    if hit: break
    time.sleep(0.05)
if not hit:
    print(f"{t0:.3f} nohit idle={len(idle)}", file=out); sys.exit(0)
(k, dl), (p, v6) = hit, path(hit[0][0])
sx = '' if p == 0 else chr(97 + p)
dev = f'xa{ID}{sx}'
if v6: m = ['protocol','ipv6','basic','match','cmp(u16 at 4 layer network gt 87)']
else:  m = ['protocol','ip','basic','match','cmp(u16 at 2 layer network gt 127)']
r = subprocess.run(['ip','netns','exec','mid'+ID,'tc','filter','add','dev',dev,'ingress','prio','1']+m+ACT,capture_output=True,text=True)
tf = time.time()
print(f"{t0:.3f} hit at +{tf-t0-1.0:.2f}s (after iperf start) link={k[0]}->{k[1]} sent={dl} path={p} dev={dev} drop1in={FZN} rc={r.returncode} {r.stderr.strip()}", file=out); out.flush()
time.sleep(FZS)
subprocess.run(['ip','netns','exec','mid'+ID,'tc','filter','del','dev',dev,'ingress','prio','1'])
print(f"{t0:.3f} unfreeze +{time.time()-t0-1.0:.2f}s", file=out)
