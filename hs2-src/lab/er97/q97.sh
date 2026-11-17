#!/bin/bash
# q97.sh (er97: hs2X = er91 + records rescue) TAG...: er92 arms in one slot. A* off, B* HS2_MCONN_RESCUE=1 both sides (blackhole plan),
# L* lossy plan with the key on (false triggers), M* mixed: edge hs2R key on + exit rel58 (blackhole plan).
set -u
J="1500us_25%_distribution_normal"
BH="40:1:delay_45ms_${J}_loss_random_100% 80:1:delay_45ms_${J}_loss_random_0.2% 160:2:delay_45ms_${J}_loss_random_100% 200:2:delay_45ms_${J}_loss_random_0.3%"
LO="40:0:delay_45ms_${J}_loss_random_3% 120:0:delay_45ms_${J}_loss_random_0.1% 130:1:delay_45ms_${J}_loss_random_10% 210:1:delay_45ms_${J}_loss_random_0.2%"
for t in "$@"; do
  case $t in
    A*) A=""; E="$BH"; X=/opt/hs2lab/er92/hs2X;;
    B*) A="HS2_MCONN_RESCUE=1 HS2_MCONN_RESCUE_TRACE=1"; E="$BH"; X=/opt/hs2lab/er92/hs2X;;
    L*) A="HS2_MCONN_RESCUE=1 HS2_MCONN_RESCUE_TRACE=1"; E="$LO"; X=/opt/hs2lab/er92/hs2X;;
    M*) A="HS2_MCONN_RESCUE=1 HS2_MCONN_RESCUE_TRACE=1"; E="$BH"; X=/opt/hs2lab/er92/rel58;;
  esac
  ARM="$A" EVENTS="$E" HS2X=$X TT=250s HVAT=212 HS2E=/opt/hs2lab/er92/hs2X bash /opt/hs2lab/er92/er92.sh $t
done
