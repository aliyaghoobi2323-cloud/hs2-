#!/bin/bash
# w.sh run -c CFG — wraps hs2 on either side (s48). FARM=tls|reality|both is every
# listener IP's front (lb.<listening side>[].front; the tunnel's "front" when there
# is no lb, i.e. the tls carrier). The dialer pins the key for pure reality only.
# FARM=r128: reality with an AES-128 target (127.0.0.1:9445). SCALE=1: per-IP keys (k0..k5) and two targets. CK=1 on the dialer: cold restart at 16 s.
F=$3; D=/opt/hs2lab/r48; B=${HS2BIN:-$D/hs2}
# the heavy test's data streams bind --cport 41000+i: keep the dialer's link sockets off them (direct mode)
sysctl -qw net.ipv4.ip_local_reserved_ports=41000-41063 2>/dev/null
OUT=${P3_OUT:-/tmp/p3}; mkdir -p $OUT
ROLE=$(python3 - "$F" "${FARM:-tls}" "${SCALE:-0}" <<"PY"
import json,sys
p,farm,scale=sys.argv[1:]; c=json.load(open(p))
listen=(c["mode"]=="listen")!=bool(c.get("reverse"))
keys=[l.split() for l in open("/opt/hs2lab/r48/keys.txt")]
sids=["61bbbd5ea44185c5","0c0c0c0c","0a0a0a0a","0d0d0d0d","0e0e0e0e","0f0f0f0f"]
def fr(i):
    k=keys[i if scale=="1" else 0]
    tgt="127.0.0.1:%d"%(9445 if farm=="r128" else 9443+(i%2 if scale=="1" else 0))
    if listen:
        if farm=="tls": return None
        return {"kind":"reality" if farm=="r128" else farm,"target":tgt,"server_names":["www.lab-target.test"],"private_key":k[0],"short_ids":[sids[i]]}
    if farm not in ("reality","r128"): return None
    return {"kind":"reality","server_names":["www.lab-target.test"],"public_key":k[1],"short_ids":[sids[i]]}
lb=c.get("lb") or {}
side="iran" if c.get("reverse") else "kharej"
if lb.get("enabled"):
    for i,e in enumerate(lb[side]):
        f=fr(i)
        if f: e["front"]=f
else:
    f=fr(0)
    if f: c["front"]=f
json.dump(c,open(p,"w"))
print("L" if listen else "D")
PY
)
for p in /proc/$BASHPID/fd/*; do n=${p##*/}; [ "$n" -gt 2 ] && eval "exec $n>&-" 2>/dev/null; done
SF=/run/hs2/$(echo "$F" | sed "s|^/||; s|/|-|g").status.json
DIRM=d; grep -q '"reverse": true' "$F" && DIRM=r
ME=$BASHPID
if [ "$ROLE" = L ]; then
  for port in 9443 9444; do
    setsid timeout 300 openssl s_server -accept 127.0.0.1:$port -cert $D/t.pem -key $D/t.key -www -quiet </dev/null >/dev/null 2>&1 &
    echo $! >> $OUT/tpids
  done
  # r128: a target that picks TLS_AES_128_GCM_SHA256 (suite parity follows it)
  setsid timeout 300 openssl s_server -accept 127.0.0.1:9445 -ciphersuites TLS_AES_128_GCM_SHA256 -cert $D/t.pem -key $D/t.key -www -quiet </dev/null >/dev/null 2>&1 &
  echo $! >> $OUT/tpids
  # checks: the non-auth probe (from the dialer's namespace, no hs2) and doctor on both configs
  ( sleep 9
    if [ $DIRM = r ]; then DNS=kh${LAB_ID:-30}; LIP=192.168.50.1; DCF=$(dirname $F)/kh.json; else DNS=ir${LAB_ID:-30}; LIP=192.168.50.2; DCF=$(dirname $F)/ir.json; fi
    for sn in www.lab-target.test lab.example.com; do
      echo "$sn $(ip netns exec $DNS timeout 5 openssl s_client -connect $LIP:2096 -servername $sn </dev/null 2>/dev/null | openssl x509 -noout -subject 2>&1)" >> $OUT/probe.txt
    done
    timeout 20 $B doctor -c $F > $OUT/doctor-L.txt 2>&1
    ip netns exec $DNS timeout 20 $B doctor -c $DCF > $OUT/doctor-D.txt 2>&1
  ) </dev/null >/dev/null 2>&1 &
  exec $B "$@"
fi
# dialer: per-second lb status
( t0=$(date +%s); while kill -0 $ME 2>/dev/null; do python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(json.dumps({\"t\":int(sys.argv[2]),\"links\":d.get(\"links\"),\"lb\":[(p.get(\"path\"),p.get(\"links\"),p.get(\"target\"),p.get(\"state\",\"ok\")) for p in d.get(\"lb\",[])]}))" $SF $(( $(date +%s)-t0 )) >> $OUT/d-status.jsonl 2>/dev/null; sleep 1; done ) </dev/null >/dev/null 2>&1 &
if [ "${CK:-0}" != 1 ]; then exec $B "$@"; fi
# CK: cold dialer restart at 16 s (exact PID), warm file removed, time to links>0
timeout 200 $B "$@" & C=$!
trap 'kill $C 2>/dev/null; exit 0' TERM INT
sleep 16 & wait $!
L0=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1])).get('links',0))" $SF 2>/dev/null)
kill $C; wait $C 2>/dev/null
rm -f /run/hs2/$(echo "$F" | sed "s|^/||; s|/|-|g").warm "$SF"
t0=$(date +%s.%N)
timeout 200 $B "$@" & C=$!
up=""; for i in $(seq 1 200); do
  l=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1])).get('links',0))" $SF 2>/dev/null || echo 0)
  if [ -z "$up" ] && [ "${l:-0}" -gt 0 ]; then up=$(python3 -c "import time;print(round(time.time()-$t0,2))"); fi
  [ -n "$up" ] && [ "${l:-0}" -ge $(( ${L0:-1} * 9 / 10 )) ] && { echo "{\"links_before\":${L0:-0},\"first_link_s\":$up,\"links90_s\":$(python3 -c "import time;print(round(time.time()-$t0,2))")}" > $OUT/restart.json; break; }
  sleep 0.1
done
[ -f $OUT/restart.json ] || echo "{\"links_before\":${L0:-0},\"first_link_s\":\"${up:-none}\",\"links90_s\":null}" > $OUT/restart.json
wait $C
