#!/bin/bash
cd /root/hs2-wt/r1-red/hs2-src
/usr/local/go/bin/go test -c -o /root/rred/bin/engine-red.test ./engine || exit 1
/usr/local/go/bin/go test -c -o /root/rred/bin/smux-red.test ./third_party/smux 2>/dev/null || (cd third_party/smux && /usr/local/go/bin/go test -c -o /root/rred/bin/smux-red.test .)
cd /root/hs2-wt/r1-red/hs2-src/engine && /usr/bin/time -v /root/rred/bin/engine-red.test -test.run "^TestRed(QuietBackAfterPeerUpgrade|HintTickNeverWaits|MoveBeforeMainSyn|OWDClockSlew)\$" -test.count=1 2>&1 | grep -E "^(ok|PASS|FAIL|---)|Maximum resident|Elapsed"
cd /root/hs2-wt/r1-red/hs2-src/third_party/smux && /usr/bin/time -v /root/rred/bin/smux-red.test -test.run "^(TestRed(GroupSeqWrap|GroupLinkDiesAroundMove|BadSynFINsWait)|FuzzRedSessionFrames)\$" -test.count=1 2>&1 | grep -E "^(ok|PASS|FAIL|---)|Maximum resident|Elapsed"
