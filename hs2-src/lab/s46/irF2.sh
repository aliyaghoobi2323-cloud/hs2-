#!/bin/bash
# Iran (reverse listener, 3 lb IPs) with every IP on one front: FARM=tls|reality|both|r70|an
#   r70: reality with the target in its own netns behind a 35 ms+35 ms netem veth (70 ms RTT)
#   an:  starts tls, switched live to both at 8 s and to pure reality at 14 s (SIGHUP)
F=$3; D=/opt/hs2lab/s46
PK=PrCGs2qJvvkEWoWFzk0xIzf0ulkunIGCxlIVLmKqQbc
TGT=127.0.0.1:9443; TNS=""
if [ "${FARM:-tls}" = r70 ]; then
  TNS=tgt${LAB_ID:-30}; X=${LAB_ID:-30}; TGT=10.199.$X.2:9443
  ip netns del $TNS 2>/dev/null; ip netns add $TNS
  ip link add vt$X-a type veth peer name vt$X-b
  ip link set vt$X-b netns $TNS
  ip addr add 10.199.$X.1/30 dev vt$X-a; ip link set vt$X-a up
  ip netns exec $TNS ip addr add 10.199.$X.2/30 dev vt$X-b
  ip netns exec $TNS ip link set vt$X-b up; ip netns exec $TNS ip link set lo up
  tc qdisc add dev vt$X-a root netem delay 35ms limit 10000
  ip netns exec $TNS tc qdisc add dev vt$X-b root netem delay 35ms limit 10000
fi
mk(){ python3 - "$F" "$PK" "$1" "$TGT" <<"PY"
import json,sys
p,pk,k,tgt=sys.argv[1:]; c=json.load(open(p))
sid={"192.168.50.1":"61bbbd5ea44185c5","192.168.51.1":"0c0c0c0c","192.168.52.1":"0a0a0a0a"}
for e in c["lb"]["iran"]:
    e["front"]={"kind":k,"target":tgt,"server_names":["www.lab-target.test"],"private_key":pk,"short_ids":[sid.get(e["ip"],"0b0b0b0b")]}
json.dump(c,open(p,"w"))
PY
}
case "${FARM:-tls}" in tls|an) ;; r70|rpy|r128) mk reality;; *) mk $FARM;; esac
for p in /proc/$BASHPID/fd/*; do n=${p##*/}; [ "$n" -gt 2 ] && eval "exec $n>&-" 2>/dev/null; done
if [ -n "$TNS" ]; then
  setsid timeout 150 ip netns exec $TNS python3 $D/tgt.py 10.199.$X.2 9443 /opt/hs2lab/r35/t.pem /opt/hs2lab/r35/t.key </dev/null >/dev/null 2>&1 &
elif [ "${FARM:-tls}" = rpy ]; then
  setsid timeout 150 python3 $D/tgt.py 127.0.0.1 9443 /opt/hs2lab/r35/t.pem /opt/hs2lab/r35/t.key </dev/null >/dev/null 2>&1 &
elif [ "${FARM:-tls}" = r128 ]; then
  setsid timeout 150 openssl s_server -accept 127.0.0.1:9443 -cert /opt/hs2lab/r35/t.pem -key /opt/hs2lab/r35/t.key -www -quiet -ciphersuites TLS_AES_128_GCM_SHA256 </dev/null >/dev/null 2>&1 &
else
  setsid timeout 150 openssl s_server -accept 127.0.0.1:9443 -cert /opt/hs2lab/r35/t.pem -key /opt/hs2lab/r35/t.key -www -quiet </dev/null >/dev/null 2>&1 &
fi
ME=$$
SF=/run/hs2/$(echo "$F" | sed "s|^/||; s|/|-|g").status.json
OUT=${P3_OUT:-/tmp/p3}; mkdir -p $OUT
( t0=$(date +%s.%N); while kill -0 $ME 2>/dev/null; do t=$(python3 -c "import time;print(round(time.time()-$t0,2))"); python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(json.dumps({\"t\":float(sys.argv[2]),\"fronts\":d.get(\"fronts\"),\"links\":d.get(\"links\")}))" $SF $t >> $OUT/ir-status.jsonl 2>/dev/null; sleep 0.25; done
  [ -n "$TNS" ] && ip netns del $TNS 2>/dev/null ) </dev/null >/dev/null 2>&1 &
[ "${FARM:-tls}" = an ] && ( sleep 8; mk both; kill -HUP $ME; echo "$(date +%s.%N) both" >> $OUT/an.txt; sleep 6; mk reality; kill -HUP $ME; echo "$(date +%s.%N) reality" >> $OUT/an.txt ;
    pr(){ echo "$(date +%s.%N) $1 $(ip netns exec kh${LAB_ID:-30} timeout 5 openssl s_client -connect 192.168.50.1:2096 -servername lab.example.com </dev/null 2>/dev/null | openssl x509 -noout -subject 2>&1)" >> $OUT/an.txt; }
    sleep 1; pr probe+1s; sleep 31; pr probe+32s ) </dev/null >/dev/null 2>&1 &
exec ${HS2BIN:-$D/hs2-cur} "$@"
