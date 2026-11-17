#!/bin/bash
# dk63d.sh RESDIR: per dark run (dk*): ana133 line + how the stream got through the dark path: subs of the heavy
# stripe at the dark moment, exit stripe cuts, ack-stall closes, resend chunk counts per side, edge hole age
# (longest run of identical "rx next" ticks), sub deaths with cause.
R=${1:-/opt/hs2lab/r63c/res63d}
python3 /opt/hs2lab/hs133/ana133.py $R/dk* 2>/dev/null
for d in $R/dk*; do t=$(basename $d); [ -e $d/$t-ir.log ] || { echo "$t no logs"; continue; }
  dk=$(sed -n 2p $d/ev.txt | cut -c1-8)
  id=$(grep -a "download stripe at" $d/$t-ir.log | head -1 | awk '{print $3}' | tr -d :)
  sa=$(grep -a "$id: striped over" $d/$t-ir.log | awk -v t=$dk '$1<t{x=$6} END{print x+0}')
  smax=$(grep -a -oE "$id: striped over [0-9]+" $d/$t-ir.log | awk '{print $4}' | sort -n | tail -1)
  cuts=$(grep -a -c "stripe cut $id" $d/$t-kh.log)
  hole=$(grep -a "$id: tick iran=true" $d/$t-ir.log | awk '{print $7}' | uniq -c | sort -n | tail -1 | awk '{print $1}')
  echo "$t id=$id dark=$dk subsAtDark=$sa subsMax=$smax exitCuts=$cuts maxFrozenTicks=$hole"
  echo "  kh: $(grep -a -ohE "resend [0-9]+ chunks|cause=[a-z:A-Z ]+(\(dark path\))?" $d/$t-kh.log | grep -a -v "resend 0" | sort | uniq -c | tr -s ' ' | tr '\n' ';')"
  echo "  ir: $(grep -a -ohE "resend [0-9]+ chunks|cause=[a-z:A-Z ]+" $d/$t-ir.log | grep -a -v "resend 0" | sort | uniq -c | tr -s ' ' | tr '\n' ';')"
  echo "  last kh keepstall: $(grep -a 'keepstall:' $d/$t-kh.log | tail -1 | grep -oE 'ack_stalls=[0-9]+|resend_chunks=[0-9]+|room_old_peer=[0-9]+|stall_cand=[0-9]+|stall_veto_tcp=[0-9]+' | tr '\n' ' ')"
done
