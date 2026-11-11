#!/bin/bash
# go52.sh: PLAN="sc:arm:i ..." PLANTAG=x — rel52 arms (docs §11.11). Binaries: hs2-r52 (rel52),
# hs2-r52pre (cb7c95ee: without the whole-peer-outage fix), hs2-r52a (cb7c95ee^: without the call-stuck change).
# hs2-r52g: rel52 + HS2_MCONN_TRACE_GAME (game-like trace band).
# hs2-r51: fa50b7d9 (rel51, before the ee51 merge), for the c-scenario A/B.
for p in /proc/$BASHPID/fd/*; do n=${p##*/}; [ "$n" -gt 2 ] && [ "$n" != 255 ] && eval "exec $n>&-" 2>/dev/null; done
D=/opt/hs2lab/r47; cd $D
r() { echo "$(date +%T) start $* [$PLANTAG]" >> $D/drv.log; /opt/hs2lab/labslot.sh "$@" >> $D/slot.$PLANTAG.log 2>&1; echo "$(date +%T) done $1 $2" >> $D/drv.log; }
for spec in $PLAN; do
  [ -f $D/stop ] && exit 0
  sc=${spec%%:*}; rest=${spec#*:}; a=${rest%%:*}; i=${rest#*:}
  case $sc:$a in
    o:52) HS2BIN=hs2-r52 T=60s r $D/run1.sh o-52-$i "30:irstop";;
    o:52p) HS2BIN=hs2-r52pre T=60s r $D/run1.sh o-52p-$i "30:irstop";;
    o:52L) IRSTOP_S=12 HS2BIN=hs2-r52 T=60s r $D/run1.sh o-52L-$i "25:irstop";;
    o:52pL) IRSTOP_S=12 HS2BIN=hs2-r52pre T=60s r $D/run1.sh o-52pL-$i "25:irstop";;
    o:52D) HS2BIN=hs2-r52 T=60s r $D/run1.sh o-52D-$i "30:irdown";;
    o:52pD) HS2BIN=hs2-r52pre T=60s r $D/run1.sh o-52pD-$i "30:irdown";;
    c:52tr) HS2BIN=hs2-r52 XENV=HS2_MCONN_TRACE=1 T=60s r $D/run1.sh c-52tr-$i "15:bbad";;
    c:52) HS2BIN=hs2-r52 T=60s r $D/run1.sh c-52-$i "15:bbad";;
    c:51) HS2BIN=hs2-r51 T=60s r $D/run1.sh c-51-$i "15:bbad";;
    c:52g) HS2BIN=hs2-r52g XENV="HS2_MCONN_TRACE=1 HS2_MCONN_TRACE_GAME=1" T=60s r $D/run1.sh c-52g-$i "15:bbad";;
    s:52) HS2BIN=hs2-r52 T=45s r $D/run1.sh s-52-$i "10:a2both 16:a2real 24:a2both 30:a2tls";;
    s:52a) HS2BIN=hs2-r52a T=45s r $D/run1.sh s-52a-$i "10:a2both 16:a2real 24:a2both 30:a2tls";;
  esac
done
touch $D/plan.done.$PLANTAG
