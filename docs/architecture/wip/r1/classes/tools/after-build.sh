#!/bin/bash
# after-build.sh BLOCK...: once gobuild.sh is done and green, a fresh frozen rig copy, then the blocks (mchain.sh)
cd /root/rmix || exit 1
until grep -q "^done at\|no gap" build.log; do sleep 5; done
grep -q BUILD-OK build.log && grep -q "^ok" build.log || { echo "$(date -u +%T) build not green" >> chain.log; exit 1; }
rm -rf rig && cp -a /root/hs2-wt/r1-mix/docs/architecture/wip/rig-scale rig && touch rigload
echo "$(date -u +%T) rig frozen, blocks $*" >> chain.log
exec tools/mchain.sh "$@"
