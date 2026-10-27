#!/bin/bash
# builds the base (r1-judge) and the β1 (r1-r2a HEAD) binaries in detached worktrees
set -e
cd /root/hs2-repo
git worktree add --detach /root/hs2-wt/r2a-bin r1-r2a >/dev/null 2>&1 || (cd /root/hs2-wt/r2a-bin && git checkout -q --detach r1-r2a)
(cd /root/hs2-wt/r2a-base/hs2-src && CGO_ENABLED=0 /usr/local/go/bin/go build -o /root/r2a/bins/hs2-base ./cmd/hs2)
(cd /root/hs2-wt/r2a-bin/hs2-src && CGO_ENABLED=0 /usr/local/go/bin/go build -o /root/r2a/bins/hs2-b1 ./cmd/hs2)
cd /root/hs2-wt/r2a-bin && git log --oneline -1
sha256sum /root/r2a/bins/* | cut -c1-16
