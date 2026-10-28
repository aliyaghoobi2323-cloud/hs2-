#!/bin/bash
# build.sh TAG: fast-forward the judge worktree to r1-r3j-pc, then (only while no
# rig lock is held) gofmt the changed Go files, go vet, and build /root/r3j/bins/hs2-TAG.
set -u
TAG=${1:?}
cd /root/hs2-wt/r1-r3j || exit 1
[ -e /root/r1-quiet.lock ] && { echo "lock held: $(cat /root/r1-quiet.lock)"; exit 2; }
git merge -q --ff-only r1-r3j-pc || exit 1
git log --oneline -1
cd hs2-src
f=$(git diff --name-only HEAD~1 -- . | grep "\.go$" | sed "s|^hs2-src/||")
[ -n "$f" ] && /usr/local/go/bin/gofmt -l $f
/usr/local/go/bin/go vet ./engine ./third_party/smux/... ./tlscarrier ./cmd/... 2>&1 | tail -20
CGO_ENABLED=0 /usr/local/go/bin/go build -o /root/r3j/bins/hs2-$TAG ./cmd/hs2 && sha256sum /root/r3j/bins/hs2-$TAG && /root/r3j/bins/hs2-$TAG version
