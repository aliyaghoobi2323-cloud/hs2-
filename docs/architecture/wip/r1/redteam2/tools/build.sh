#!/bin/bash
# build.sh: the red team (second pass) build under its own rig lock (block2.sh
# PRE): hs2 of the worktree and rigload into /root/rred2/raw-bins, the
# red-team tests of engine and smux, nothing else.
set -eu
R=/root/hs2-wt/r1-red2; B=/root/rred2/raw-bins; G=/usr/local/go/bin/go
cd $R/hs2-src
CGO_ENABLED=0 nice -n 5 $G build -o $B/hs2-r2.new ./cmd/hs2 && mv -f $B/hs2-r2.new $B/hs2-r2
echo "hs2-r2 $(git -C $R rev-parse --short HEAD)$(git -C $R diff --quiet -- hs2-src || echo +dirty) $(sha256sum $B/hs2-r2 | cut -c1-16)"
(cd $R/docs/architecture/wip/rig-scale/rigload && CGO_ENABLED=0 $G build -o $B/rigload.new . && mv -f $B/rigload.new $B/rigload)
echo "rigload $(sha256sum $B/rigload | cut -c1-16)"
$G test -count=1 -run "Red2" ./engine 2>&1 | tail -15
(cd third_party/smux && $G test -count=1 -run "Red2" . 2>&1 | tail -15)
