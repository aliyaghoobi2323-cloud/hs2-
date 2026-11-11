# s48 harness (rel48, docs/reality-architecture.md §11.7)

Lab 2 copy at /opt/hs2lab/r48 (lab source: a copy of smuxwin labsrc2 with
`../s45/patchrun.py` applied and `tcprelay.py` next to run.sh).
`drv.sh JOBS 3` runs lines `RUNNER NAME DIR CAR FARM VAR` through
/opt/hs2lab/labslot.sh. `run1.sh NAME DIR CAR FARM VAR`: DIR d (direct, kharej
listens) | r (reverse); CAR mtcp | l3mtcp | tls (the tls carrier has no lb:
one IP, its one link killed); FARM tls | reality | both on every listening
IP; VAR m (crowd 3000 + call + game + heavy down x8, 75 % of the links killed
at 12 s), s (one download connection), k (as m without the kill, cold dialer
restart at 16 s), sc (6 IPs 40/20/15/10/10/5, per-IP keys and two targets,
path c 30 % loss 12-32 s). `w.sh` wraps hs2 on both sides (fronts, target
servers, probe + doctor at 9 s, dialer status per second); `run2.sh`/`w2.sh`
add FARM r128 (a target that picks TLS_AES_128_GCM_SHA256). keys.txt (6 lines
"private public" from `hs2 reality keygen`) and t.pem/t.key (CN/SAN
www.lab-target.test) live in the lab dir.
