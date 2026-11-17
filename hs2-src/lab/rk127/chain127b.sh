#!/bin/bash
# chain127b.sh: after rk127chain: mixed run (rel62k-on edge + rel61 exit), final build.
set -u
R=/opt/hs2lab/rel62k
while systemctl is-active -q rk127chain; do sleep 30; done
echo "$(date -u +%FT%TZ) mixed start" >> $R/chain.log
/opt/hs2lab/labslot.sh bash $R/mx127.sh MX MX1
cd $R/src/hs2-src && export PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod HOME=/root
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $R/hs2-linux-amd64 ./cmd/hs2 && (cd $R && sha256sum hs2-linux-amd64 > hs2-linux-amd64.sha256)
go clean -cache; echo "$(date -u +%FT%TZ) chain b done" >> $R/chain.log
