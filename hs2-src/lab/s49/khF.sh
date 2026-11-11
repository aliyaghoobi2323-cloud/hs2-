#!/bin/bash
F=$3; D=/opt/hs2lab/s49
# pure reality: the dialer pins the key (a cold dialer never hears an announcement over a reality-only listener)
{ [ "${FARM:-tls}" = reality ] || [ "${FARM:-tls}" = r70 ] || [ "${FARM:-tls}" = rpy ]; } && python3 - "$F" <<"PY"
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
exec ${HS2BIN:-$D/hs2-cur} "$@"
