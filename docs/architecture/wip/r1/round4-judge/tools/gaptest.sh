#!/bin/bash
# gaptest.sh BLOCK OUT: after block BLOCK ends, in the gap (no lock held),
# the TUN ping one_session subtest with the realtime share and with the
# latency lane by user, and the rigload loopback test 5 times; output to OUT
B=${1:?}; OUT=${2:?}
for i in $(seq 1 600); do grep -q "done after" /root/r4j/$B/orch.log 2>/dev/null && break; sleep 5; done
cd /root/hs2-wt/r1-r4j/hs2-src/engine || exit 1
cat > zz_tmp_judge_test.go <<'EOF'
package engine

import "testing"

func TestZZJudgeTunShare(t *testing.T) { RTShare.Set(8192); defer RTShare.Set(0); oneSessionTunPing(t) }
func TestZZJudgeTunShare4(t *testing.T) { RTShare.Set(4096); defer RTShare.Set(0); oneSessionTunPing(t) }
func TestZZJudgeTunRR(t *testing.T)    { LatRR.Set(1); defer LatRR.Set(0); oneSessionTunPing(t) }
func TestZZJudgeTunHold(t *testing.T)  { RTHold.Set(20); defer RTHold.Set(0); oneSessionTunPing(t) }
EOF
cd ..
export PATH=$PATH:/usr/local/go/bin
{
  date -u +%T
  if [ -e /root/r1-quiet.lock ]; then echo "lock held: $(cat /root/r1-quiet.lock)"; else
    timeout 150 nice go test -count=1 -run "TestZZJudge" ./engine -v 2>&1 | grep -E "ping one-way|^(ok|FAIL|---)"
    cd /root/hs2-wt/r1-r4j/docs/architecture/wip/rig-scale/rigload && timeout 120 nice go test -count=5 -run TestClassesLoopback . 2>&1 | tail -3
  fi
  date -u +%T
} > "$OUT" 2>&1
rm -f /root/hs2-wt/r1-r4j/hs2-src/engine/zz_tmp_judge_test.go
