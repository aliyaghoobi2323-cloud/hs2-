#!/bin/bash
# Iran: A 192.168.50.1 tls, B 51.1 reality (target 10.199.X.2:9443 in netns tgtX, tgt.py), C 52.1 both (target lo s_server).
# P3_EV="SEC:ACTION ..." : bbad (path B +80ms, 3% corrupt) | tblk (B target blackholed) | trej (B target refuses: RST)
#   | tslow (B target +2.5 s each way) | tok (B target healthy) | a2both a2real a2tls (live front of A, SIGHUP)
F=$3; D=/opt/hs2lab/r47; X=${LAB_ID:-30}; TNS=tgt$X
PK=PrCGs2qJvvkEWoWFzk0xIzf0ulkunIGCxlIVLmKqQbc
ip netns del $TNS 2>/dev/null; ip link del vt$X-a 2>/dev/null; ip netns add $TNS
ip link add vt$X-a type veth peer name vt$X-b; ip link set vt$X-b netns $TNS
ip addr add 10.199.$X.1/30 dev vt$X-a; ip link set vt$X-a up
ip netns exec $TNS ip addr add 10.199.$X.2/30 dev vt$X-b; ip netns exec $TNS ip link set vt$X-b up; ip netns exec $TNS ip link set lo up
tc qdisc add dev vt$X-a root netem delay 1ms limit 10000
setfront(){ python3 - "$F" "$PK" "$X" "$1" <<"PY"
import json,sys
p,pk,x,a=sys.argv[1:]; c=json.load(open(p)); import os; btls=os.environ.get("BTLS")=="1"
R=lambda k,s,t:{"kind":k,"target":t,"server_names":["www.lab-target.test"],"private_key":pk,"short_ids":[s]}
for e in c["lb"]["iran"]:
    if a=="init":
        if e["ip"]=="192.168.51.1" and not btls: e["front"]=R("reality","61bbbd5ea44185c5","10.199.%s.2:9443"%x)
        if e["ip"]=="192.168.50.1" and btls: e["front"]=R("reality","0a0a0a0a","127.0.0.1:9443")
        if e["ip"]=="192.168.52.1": e["front"]=R("both","0c0c0c0c","127.0.0.1:9443")
    elif e["ip"]=="192.168.50.1":
        if a=="tls": e.pop("front",None)
        else: e["front"]=R(a,"0a0a0a0a","127.0.0.1:9443")
json.dump(c,open(p,"w"))
PY
}
[ -n "${PLAIN:-}" ] || setfront init
for p in /proc/$BASHPID/fd/*; do n=${p##*/}; [ "$n" -gt 2 ] && eval "exec $n>&-" 2>/dev/null; done
setsid timeout 150 openssl s_server -accept 127.0.0.1:9443 -cert /opt/hs2lab/r35/t.pem -key /opt/hs2lab/r35/t.key -www -quiet </dev/null >/dev/null 2>&1 &
setsid timeout 150 ip netns exec $TNS python3 $D/tgt.py 10.199.$X.2 9443 /opt/hs2lab/r35/t.pem /opt/hs2lab/r35/t.key </dev/null >/dev/null 2>&1 &
ME=$$
SF=/run/hs2/$(echo "$F" | sed "s|^/||; s|/|-|g").status.json
OUT=${P3_OUT:-/tmp/p3}; mkdir -p $OUT
( t0=$(date +%s); while kill -0 $ME 2>/dev/null; do t=$(( $(date +%s)-t0 )); python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(json.dumps({\"t\":int(sys.argv[2]),\"fronts\":d.get(\"fronts\"),\"reality\":d.get(\"reality\"),\"links\":d.get(\"links\")}))" $SF $t >> $OUT/ir-status.jsonl 2>/dev/null; sleep 1; done
  ip netns del $TNS 2>/dev/null; ip link del vt$X-a 2>/dev/null ) </dev/null >/dev/null 2>&1 &
( t0=$(date +%s)
  for ev in ${P3_EV:-}; do at=${ev%%:*}; act=${ev#*:}
    while [ $(( $(date +%s)-t0 )) -lt $at ]; do sleep 0.2; done
    case $act in
      bbad) WD=/tmp/r47w${LAB_ID:-30}; MD=mid${LAB_ID:-30}
        for dv in $(cut -d" " -f1,2 $WD/emu-1); do
          cur=$(ip netns exec $MD tc qdisc show dev $dv | grep -m1 netem)
          args=$(echo "$cur" | sed -E "s/.*root refcnt [0-9]+ //; s/ seed [0-9]+//")
          d=$(echo "$args" | sed -nE "s/.*delay ([0-9.]+)ms.*/\1/p")
          nd=$(python3 -c "print(int(float('$d'))+80)")
          args=$(echo "$args" | sed -E "s/delay [0-9.]+ms/delay ${nd}ms/")
          ip netns exec $MD tc qdisc change dev $dv root netem $args corrupt 3% 2>>$OUT/events.txt
        done;;
      tblk) ip netns exec $TNS iptables -I INPUT -p tcp --dport 9443 -j DROP;;
      trej) ip netns exec $TNS iptables -I INPUT -p tcp --dport 9443 -j REJECT --reject-with tcp-reset;;
      tslow) tc qdisc change dev vt$X-a root netem delay 2500ms limit 10000;;
      tok) ip netns exec $TNS iptables -F INPUT; tc qdisc change dev vt$X-a root netem delay 1ms limit 10000;;
      a2both) setfront both; kill -HUP $ME;;
      a2real) setfront reality; kill -HUP $ME;;
      a2tls) setfront tls; kill -HUP $ME;;
    esac
    echo "$(date +%T) t=$at $act" >> $OUT/events.txt
  done ) </dev/null >/dev/null 2>&1 &
exec $D/${HS2BIN:-hs2-r45} "$@"
