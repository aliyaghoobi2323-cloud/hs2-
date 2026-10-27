#!/bin/bash
# tests.sh: γ2's tests of the branch (round 3), each step only once no
# quiet lock is held (a step that runs long may overlap a block that starts
# meanwhile: the rig's block scripts wait for no go test). Output to
# round3-g2/tests/.
set -u
W=/root/hs2-wt/r1-r3b
O=$W/docs/architecture/wip/r1/round3-g2/tests
G=/usr/local/go/bin/go
mkdir -p $O
free() { while [ -e /root/r1-quiet.lock ]; do sleep 20; done; }
cd $W/hs2-src
free; $G vet ./... > $O/vet.txt 2>&1; echo "vet exit $?" >> $O/vet.txt
/usr/local/go/bin/gofmt -l . > $O/gofmt.txt 2>&1
free; (cd third_party/smux && nice $G test -count=1 -race ./... > $O/race-smux.txt 2>&1; echo "exit $?" >> $O/race-smux.txt)
free; nice $G test -count=1 -race -timeout 20m ./engine -run 'Budget|OWD|Quiet|Shape|SendCap|Sched|MoveRule|Group|HostQ' > $O/race-engine.txt 2>&1; echo "exit $?" >> $O/race-engine.txt
free; nice $G test -run '^$' -fuzz FuzzOWDDecode -fuzztime 60s ./engine > $O/fuzz-owd.txt 2>&1; echo "exit $?" >> $O/fuzz-owd.txt
free; nice $G test -count=1 -timeout 30m ./... > $O/test-all.txt 2>&1; echo "exit $?" >> $O/test-all.txt
echo "$(date -u +%T) tests done at $(git -C $W rev-parse --short HEAD)" >> $O/done.txt
