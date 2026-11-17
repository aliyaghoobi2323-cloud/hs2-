#!/bin/bash
# lk179: arms sequentially on the whole lab (under labslot --all)
set -u
K=/opt/hs2lab/lk179
for a in ${ARMS:?}; do
  [ -e $K/STOPALL2 ] && break
  case $a in
    R*) SB=$K/hs2-rel66b; IRX=""; KHX=""; PROF=0;;
    Q*) SB=$K/hs2-rel66b; IRX="HS2_PPROF_LOCKS=100"; KHX="HS2_PPROF_LOCKS=100"; PROF=1;;
    L*) SB=$K/hs2-lk179a; IRX="HS2_LM_FASTPICK=1"; KHX=""; PROF=0;;
    P*) SB=$K/hs2-lk179a; IRX="HS2_LM_FASTPICK=1 HS2_PPROF_LOCKS=100"; KHX="HS2_PPROF_LOCKS=100"; PROF=1;;
    F*) SB=$K/hs2-lk179b; IRX="HS2_LM_FASTPICK=1 ${FIX_IR:-}"; KHX="${FIX_KH:-}"; PROF=0;;
    G*) SB=$K/hs2-lk179b; IRX="HS2_LM_FASTPICK=1 ${FIX_IR:-} HS2_PPROF_LOCKS=100"; KHX="${FIX_KH:-} HS2_PPROF_LOCKS=100"; PROF=1;;
  esac
  [ -e $K/$a ] && { echo "$a exists, skipped" >> $K/seq.log; continue; }
  mkdir -p $K/$a
  echo "$(date -u +%T) start $a" >> $K/seq.log
  O=$K/$a SB=$SB IRX="$IRX" KHX="$KHX" PROF=$PROF bash $K/sc179d.sh > $K/$a.sc.out 2>&1
  echo "$(date -u +%T) end $a rc=$?" >> $K/seq.log
  sleep 15
done
