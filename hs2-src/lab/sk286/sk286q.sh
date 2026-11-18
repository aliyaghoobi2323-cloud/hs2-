#!/bin/bash
R=/opt/hs2lab/dc254
while systemctl is-active -q sk286p; do sleep 10; done
SEGS="SB11:b286 SB12:b286 SB13:b286" XENV="" bash $R/h2/sk280e.sh
echo "$(date -u +%FT%TZ) sk286q done" >> $R/seq.log
