#!/bin/bash
# go47.sh: PLAN="sc:arm:i ..." PLANTAG=q — runs each spec through labslot (rel47 matrix)
for p in /proc/$BASHPID/fd/*; do n=${p##*/}; [ "$n" -gt 2 ] && [ "$n" != 255 ] && eval "exec $n>&-" 2>/dev/null; done
D=/opt/hs2lab/r47; cd $D
r() { echo "$(date +%T) start $* [$PLANTAG]" >> $D/drv.log; /opt/hs2lab/labslot.sh "$@" >> $D/slot.$PLANTAG.log 2>&1; echo "$(date +%T) done $1 $2" >> $D/drv.log; }
for spec in $PLAN; do
  [ -f $D/stop ] && exit 0
  sc=${spec%%:*}; rest=${spec#*:}; a=${rest%%:*}; i=${rest#*:}
  case $sc:$a in
    b:mix) T=45s r $D/run1.sh b-mix-$i "";;
    b:tls) PLAIN=1 T=45s r $D/run1.sh b-tls-$i "";;
    c:mix) T=60s r $D/run1.sh c-mix-$i "15:bbad";;
    c:mixr) BTLS=1 T=60s r $D/run1.sh c-mixr-$i "15:bbad";;
    c:mixtr) XENV=HS2_MCONN_TRACE=1 T=60s r $D/run1.sh c-mixtr-$i "15:bbad";;
    c:tlstr) PLAIN=1 XENV=HS2_MCONN_TRACE=1 T=60s r $D/run1.sh c-tlstr-$i "15:bbad";;
    st:old) HS2BIN=hs2-r45 T=30s r $D/run1.sh st-old-$i "";;
    st:new) HS2BIN=hs2-r47 T=30s r $D/run1.sh st-new-$i "";;
    c:mixN) HS2BIN=hs2-r47 T=60s r $D/run1.sh c-mixN-$i "15:bbad";;
    c:tlsN) PLAIN=1 HS2BIN=hs2-r47 T=60s r $D/run1.sh c-tlsN-$i "15:bbad";;
    p:old) BTLS=1 HS2BIN=hs2-r45 T=40s r $D/run1.sh p-old-$i "";;
    p:new) BTLS=1 HS2BIN=hs2-r47b T=40s r $D/run1.sh p-new-$i "";;
    c:mixB) HS2BIN=hs2-r47b T=60s r $D/run1.sh c-mixB-$i "15:bbad";;
    c:tls) PLAIN=1 T=60s r $D/run1.sh c-tls-$i "15:bbad";;
    t:ctlL) KILL_AT=12 T=75s r $D/run1.sh t-ctlL-$i "";;
    t:rejL|t:blkL|t:slowL) KILL_AT=12 T=75s r $D/run1.sh t-$a-$i "8:t${a%L} 25:tok";;
    t:ctl) KILL_AT=12 T=50s r $D/run1.sh t-ctl-$i "";;
    t:blk|t:rej|t:slow) KILL_AT=12 T=50s r $D/run1.sh t-$a-$i "8:t$a 32:tok";;
    s:sw) T=45s r $D/run1.sh s-sw-$i "10:a2both 16:a2real 24:a2both 30:a2tls";;
  esac
done
touch $D/plan.done.$PLANTAG
