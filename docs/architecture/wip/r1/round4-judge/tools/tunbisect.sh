#!/bin/bash
# tunbisect.sh: after block S2 ends, in the gap (no lock held by anyone),
# run TestAngleTunPingAheadOfBulk/one_session with round 4's switches off one
# by one (the CI's red on r1-r4j); output to /root/r4j/tunbisect.txt
OUT=/root/r4j/tunbisect.txt
for i in $(seq 1 400); do grep -q "done after" /root/r4j/S2/orch.log 2>/dev/null && break; sleep 5; done
cd /root/hs2-wt/r1-r4j/hs2-src/engine || exit 1
cat > zz_tmp_judge_test.go <<'EOF'
package engine

import "testing"

func TestZZJudgeTunBase(t *testing.T) { oneSessionTunPing(t) }
func TestZZJudgeTunRT0(t *testing.T)  { TuneRT.Set(0); defer TuneRT.Set(1); oneSessionTunPing(t) }
func TestZZJudgeTunTW0(t *testing.T)  { TwoWay.Set(0); defer TwoWay.Set(1); oneSessionTunPing(t) }
func TestZZJudgeTunPU0(t *testing.T)  { PlaceUsers.Set(0); defer PlaceUsers.Set(2); oneSessionTunPing(t) }
EOF
cd ..
export PATH=$PATH:/usr/local/go/bin
{
  date -u +%T
  if [ -e /root/r1-quiet.lock ]; then echo "lock held: $(cat /root/r1-quiet.lock)"; else
    timeout 150 nice go test -count=1 -run "TestZZJudge" ./engine -v 2>&1 | grep -E "ping one-way|^(ok|FAIL|---)"
  fi
  date -u +%T
} > "$OUT" 2>&1
rm -f /root/hs2-wt/r1-r4j/hs2-src/engine/zz_tmp_judge_test.go
