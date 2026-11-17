#!/bin/bash
# dr259: dl259 PATHQ default-on decision. Same binary hs2C both arms (key off P / on Q), stripe log on.
# Light 2+2 (mixed up/down tests) then heavy 2+2 (CROWD=3000, 6 bg slots incl. unlimited downloads, path F 3 % loss).
R=/opt/hs2lab/dr257
log(){ echo "$(date -u +%FT%TZ) $*" >> $R/seq.log; }
run(){ s=$1 X=$2; mkdir -p $R/$s; log "start $s bin=$(sha256sum $R/bin/hs2C|cut -c1-8) X=$X $3 TT=$4"
  env $3 O=$R/$s SB=$R/bin/hs2C SK=$R/bin/hs2C XENV="HS2_MCONN_RESCUE_RST=1 HS2_MCONN_STRIPE_LOG=1 $X" TT=$4 TI=16 bash /opt/hs2lab/labslot.sh --all bash $R/dlab.sh > $R/$s.out 2>&1
  log "end $s rc=$?"; sleep 3; }
for s in P3 Q3 Q4 P4; do [ -e $R/STOP3 ] && exit
  case $s in P*) X="";; Q*) X="HS2_DL259_PATHQ=1";; esac; run $s "$X" "DNONLY=0" 180; done
for s in HP1 HQ1 HQ2 HP2; do [ -e $R/STOP3 ] && exit
  case $s in HP*) X="";; HQ*) X="HS2_DL259_PATHQ=1";; esac
  run $s "$X" "CROWD=3000 NBG=6 DNONLY=1 LOSSB=0.1_0.3_0.2_0.5_0.1_3_0.2_0.8_0.4_0.2 BGRS=50_80_0_0" 180; done
log "chain3 done"
