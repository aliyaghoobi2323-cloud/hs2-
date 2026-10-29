#!/bin/bash
# gapbuild.sh: after block G5, in the gap: build hs2-j2 (the QUIET pack
# switch) and run its tests, then block S10
for i in $(seq 1 600); do grep -q "done after" /root/r4j/G5/orch.log 2>/dev/null && break; sleep 5; done
export PATH=$PATH:/usr/local/go/bin
cd /root/hs2-wt/r1-r4j/hs2-src || exit 1
{
  date -u +%T
  CGO_ENABLED=0 go build -o /root/r4j/bins/hs2-j2 ./cmd/hs2 && sha256sum /root/r4j/bins/hs2-j2
  go vet ./engine ./cmd/... && echo vet-ok
  timeout 120 go test -count=1 -run "TestQuietPack|TestQuietFewCount|TestQuietCountsExtra|TestQuietByUserSticky|TestQuietKillShare|TestQuietDefaultsRound3" ./engine -v 2>&1 | grep -E "^(---|ok|FAIL)|pack [01]:"
  date -u +%T
} > /root/r4j/gapbuild.txt 2>&1
/root/r4j/tools/chain.sh "S10 2"
