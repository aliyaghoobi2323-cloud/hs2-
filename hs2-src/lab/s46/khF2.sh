#!/bin/bash
F=$3; D=/opt/hs2lab/s46
# pure reality: the dialer pins the key (a cold dialer never hears an announcement over a reality-only listener)
{ [ "${FARM:-tls}" = reality ] || [ "${FARM:-tls}" = r70 ] || [ "${FARM:-tls}" = rpy ] || [ "${FARM:-tls}" = r128 ]; } && python3 - "$F" <<"PY"
import json,sys
p=sys.argv[1]; c=json.load(open(p))
sid={"192.168.50.1":"61bbbd5ea44185c5","192.168.51.1":"0c0c0c0c","192.168.52.1":"0a0a0a0a"}
for e in c["lb"]["iran"]:
    e["front"]={"kind":"reality","server_names":["www.lab-target.test"],"public_key":"_4_19atU_LwDBCAymAsqUmcIX2ySEPH8ysZ4vQEy1hA","short_ids":[sid[e["ip"]]]}
json.dump(c,open(p,"w"))
PY
for p in /proc/$BASHPID/fd/*; do n=${p##*/}; [ "$n" -gt 2 ] && eval "exec $n>&-" 2>/dev/null; done
ME=$$
SF=/run/hs2/$(echo "$F" | sed "s|^/||; s|/|-|g").status.json
OUT=${P3_OUT:-/tmp/p3}; mkdir -p $OUT
( t0=$(date +%s); while kill -0 $ME 2>/dev/null; do t=$(( $(date +%s)-t0 )); python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(json.dumps({\"t\":int(sys.argv[2]),\"front_paths\":d.get(\"front_paths\"),\"lb\":[(p.get(\"path\"),p.get(\"links\"),p.get(\"state\",\"ok\")) for p in d.get(\"lb\",[])]}))" $SF $t >> $OUT/kh-status.jsonl 2>/dev/null; sleep 1; done ) </dev/null >/dev/null 2>&1 &
# s46: ss -ti of the link sockets every second (kharej = the c0/c1 sender): n, sum cwnd, mean rtt, sum retrans, sum delivery_rate
( while kill -0 $ME 2>/dev/null; do echo "$(date +%s) $(ss -tinH state established '( sport = :2096 or dport = :2096 )' 2>/dev/null | awk '{for(i=1;i<=NF;i++){if($i~/^cwnd:/){split($i,a,":");cw+=a[2];n++} if($i~/^rtt:/){split($i,a,"[:/]");rt+=a[2]} if($i~/^retrans:/){split($i,a,"/");rx+=a[2]} if($i~/^delivery_rate/){v=$(i+1);m=1;if(v~/Gbps/)m=1000;else if(v~/Kbps/)m=0.001;else if(v~/bps/&&v!~/Mbps/)m=0.000001;sub(/[A-Za-z]+/,"",v);dr+=v*m}}}END{printf "n=%d cwnd=%d rtt=%.1f retr=%d rate=%.0f",n,cw,(n?rt/n:0),rx,dr}')" >> $OUT/kh-ss.txt; sleep 1; done ) </dev/null >/dev/null 2>&1 &
exec ${HS2BIN:-$D/hs2-cur} "$@"
