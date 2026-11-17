# mk125.py: builds /opt/hs2lab/ec125/ec125.sh from ec124d.sh (lab3).
s = open("/opt/hs2lab/ec124/ec124d.sh").read()
lines = s.split("\n")
hdr = """#!/bin/bash
# ec125.sh E|R TAG (id125 F4/F3): R = rel61 both sides, E = hs2E125 both sides with HS2_RETIRE_KEEP_IDLE=1.
# T=1500, crowd 300 (light p99), heavy at 100/700/1300. t=20 600 conns idle until t=1340 (22 min idle);
# t=60 20k storm (stormgo, burst at 400, then closed -> pool shrinks); t=430 300 conns idle until t=1240 (13 min).
# pprof cpu 8 s on both sides at t=62 (storm) and t=401 (burst)."""
i = lines.index("set -u")
s = "\n".join([hdr] + lines[i:])


def rep(a, b):
    global s
    assert a in s, a
    s = s.replace(a, b)


rep('R=/opt/hs2lab/ec124; O=$R/res/$TAG', 'R=/opt/hs2lab/ec125; R4=/opt/hs2lab/ec124; O=$R/res/$TAG')
rep('[ $1 = D ] && { SB=$R/hs2E124; KX="HS2_LB_SILENT_PROBE_TLS=1"; }', '[ $1 = E ] && { SB=$R/hs2E125; KX="HS2_RETIRE_KEEP_IDLE=1"; }')
rep('WD=/tmp/ec124-', 'WD=/tmp/ec125-')
rep('if [ $SC = A ]; then TT=960; CR=1500; CA=2mbit; GA=1; elif [ $SC = C ] || [ $SC = D ]; then TT=330; CR=1500; CA=2mbit; GA=1; else TT=780; CR=0; CA=""; GA=0; fi',
    'TT=1500; CR=300; CA=""; GA=0')
a = s.index("  if [ $SC = C ] || [ $SC = D ]; then")
b = s.index("  fi\n", s.index("    at 400; ev burst20k; at 700; ev idle300 request")) + 5
new = """  prof(){ for s in ir kh; do ip netns exec $s$ID timeout 15 curl -s -o $O/$s-cpu-$1.pb.gz "http://127.0.0.1:6060/debug/pprof/profile?seconds=8" & done; }
  ( for t in 100 700 1300; do at $t; [ -e $O/STOP ] && break; heavy; done ) &
  at 20; ev idle600 open; ip netns exec ir$ID bash -c "ulimit -n 200000; $R4/stormgo 127.0.0.1:8443 600 5 1320 $O/storm-idle600.jsonl" &
  at 60; ev storm20k start; ip netns exec ir$ID bash -c "ulimit -n 200000; $R4/stormgo 127.0.0.1:8443 20000 10 340 $O/storm-20k.jsonl" &
  at 62; prof storm
  at 401; ev burst20k; prof burst
  at 430; ev idle300 open; ip netns exec ir$ID bash -c "ulimit -n 200000; $R4/stormgo 127.0.0.1:8443 300 5 810 $O/storm-idle300.jsonl" &
  at 1240; ev idle300 request; at 1340; ev idle600 request; wait
"""
s = s[:a] + new + s[b:]
rep('IR_ENV="HS2_PPROF=127.0.0.1:6060" KH_ENV="HS2_PPROF=127.0.0.1:6060 $KX"', 'IR_ENV="HS2_PPROF=127.0.0.1:6060 $KX" KH_ENV="HS2_PPROF=127.0.0.1:6060 $KX"')
rep("RUNTAG=ec124$TAG", "RUNTAG=ec125$TAG")
rep("python3 $R/win.py", "python3 $R4/win.py")
open("/opt/hs2lab/ec125/ec125.sh", "w").write(s)
