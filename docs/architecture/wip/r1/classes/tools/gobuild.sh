#!/bin/bash
# waits for a gap with no quiet lock, then go vet / build / test of rigload (r1-mix)
cd /root/hs2-wt/r1-mix/docs/architecture/wip/rig-scale/rigload || exit 1
busy() { pgrep -f -- "go test|go build|go vet|\.test -test\.|/go-build[0-9]*/.*\.test" > /dev/null; }
for i in $(seq 1 1200); do
  if [ ! -e /root/r1-quiet.lock ] && ! busy; then
    echo "gap at $(date -u +%T)"
    export PATH=/usr/local/go/bin:$PATH GOFLAGS=-count=1
    gofmt -l . ; go vet . && CGO_ENABLED=0 go build -o /root/rmix/rigload . && echo BUILD-OK
    go test -count=1 . 2>&1 | tail -30
    echo "done at $(date -u +%T), lock now: $(cat /root/r1-quiet.lock 2>/dev/null || echo none)"
    exit 0
  fi
  sleep 2
done
echo "no gap in 40 min"
