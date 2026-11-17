#!/bin/bash
# chain127.sh: pair 1 (OFF slot A / ON slot B) + PR together, then pair 2 swapped, then tests.
set -u
R=/opt/hs2lab/rel62k; S=/opt/hs2lab/labslot.sh; mkdir -p $R/res
echo "$(date -u +%FT%TZ) chain start" >> $R/chain.log
$S bash $R/rk127.sh OFF P1off & sleep 20
$S bash $R/rk127.sh ON P1on & sleep 20
$S bash $R/rk127.sh PR PR1 & wait
echo "$(date -u +%FT%TZ) pair1+PR done" >> $R/chain.log
$S bash $R/rk127.sh ON P2on & sleep 20
$S bash $R/rk127.sh OFF P2off & wait
echo "$(date -u +%FT%TZ) pair2 done" >> $R/chain.log
cd $R/src/hs2-src && export PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod HOME=/root
( go test ./... -count=1 -timeout 30m > $R/test.txt 2>&1; echo "rc=$?" >> $R/test.txt
  go test -race ./engine -count=1 -timeout 30m > $R/race.txt 2>&1; echo "rc=$?" >> $R/race.txt )
go clean -cache; echo "$(date -u +%FT%TZ) chain done" >> $R/chain.log
