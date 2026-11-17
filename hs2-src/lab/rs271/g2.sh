#!/bin/bash
# rs271 g2.sh NAME FB : Q1 stripe hot-fallback A/B under heavy crowd (dlab.sh, no fault, download-only single-stream tests).
# Same binary hs2P273 both arms; FB = HS2_DL273_HOTFB on Iran (0 = dlp217 as shipped).
L=/opt/hs2lab/rs271; n=$1; FB=$2; P=$L/bin/hs2P273
mkdir -p $L/$n; echo "$(date -u +%FT%TZ) start $n FB=$FB" >> $L/seq.log
env FZ=0 UPONLY=0 DNONLY=1 SSMON=0 O=$L/$n SB=$P SK=$P NBG=${NBG:-4} CROWD=${CROWD:-4000} XENV="HS2_MCONN_RESCUE_RST=1" IRX="HS2_DL273_HOTFB=$FB" KHX="" TT=${TT:-200} TI=14 bash /opt/hs2lab/labslot.sh bash /opt/hs2lab/ab267/dlab.sh > $L/$n.out 2>&1
echo "$(date -u +%FT%TZ) end $n rc=$?" >> $L/seq.log
