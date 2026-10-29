#!/bin/bash
# vmmix.sh NAME N EDGE_BIN EXIT_BIN ["EDGE_ENV"] ["EXIT_ENV"]: item 4 of the R1
# round-4 judge. Two private namespaces (vm-e: the edge, reverse listener;
# vm-x: the exit, reverse dialer), a veth without netem, carrier l3mtcp as the
# rig, min_links = max_links = N; after 15 s the status of both ends, then a
# 30 MB download through user port 8443 (edge) to a file server behind the
# exit (127.0.0.1:5299), then the status again. Keys and cert are made per
# run in the work dir and removed after; logs are scrubbed of the key.
# Out: /root/r4j/vm/mix/NAME/ (result.txt, *.log, status-*.txt).
set -u
NAME=$1; N=$2; EB=$3; XB=$4; EENV=${5:-}; XENV=${6:-}
W=/root/r4j/vmfin/$NAME-$(date +%s); C=/root/r4j/vm/mix
mkdir -p "${W:?}"
E=vm-e; X=vm-x
cleanup() {
	for p in $(cat $W/pids 2>/dev/null); do kill $p 2>/dev/null; done
	sleep 0.5
	for p in $(cat $W/pids 2>/dev/null); do kill -9 $p 2>/dev/null; done
	rm -f "${W:?}/pids" "${W:?}/key.pem" "${W:?}/cert.pem" "${W:?}/edge.json" "${W:?}/exit.json"
	ip netns del $E 2>/dev/null; ip netns del $X 2>/dev/null
}
trap cleanup EXIT
cleanup
[ -f $C/www/big ] || { mkdir -p $C/www; head -c 30000000 /dev/urandom > $C/www/big; }
ip netns add $E; ip netns add $X
ip link add vme0 netns $E type veth peer name vmx0 netns $X
ip -n $E addr add 192.168.79.1/24 dev vme0
ip -n $X addr add 192.168.79.2/24 dev vmx0
for n in $E $X; do ip -n $n link set lo up; done
ip -n $E link set vme0 up; ip -n $X link set vmx0 up
KEY=$(openssl rand -hex 32)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -keyout $W/key.pem -out $W/cert.pem -days 2 -subj /CN=lab.example.com 2>/dev/null
cat > $W/edge.json <<J
{"mode":"dial","reverse":true,"carrier":"l3mtcp","addr":"192.168.79.1:2096",
 "iface":"vm0","local_cidr":"10.79.9.1/30","peer_ip":"10.79.9.2","mtu":1320,
 "shared_key":"$KEY","cert_file":"$W/cert.pem","key_file":"$W/key.pem","backend_addr":"builtin",
 "forward_ports":"8443","min_links":$N,"max_links":$N,"per_link":8}
J
cat > $W/exit.json <<J
{"mode":"listen","reverse":true,"carrier":"l3mtcp","addr":"192.168.79.1:2096","sni":"lab.example.com",
 "iface":"vm0","local_cidr":"10.79.9.2/30","peer_ip":"10.79.9.1","mtu":1320,
 "shared_key":"$KEY","expose":"127.0.0.1:5299","min_links":$N,"max_links":$N,"per_link":8}
J
chmod 600 $W/*.json $W/key.pem
( cd $C/www && exec ip netns exec $X python3 -m http.server 5299 --bind 127.0.0.1 ) > /dev/null 2>&1 &
echo $! >> $W/pids
ip netns exec $E env HS2_NO_TUNE=1 $EENV $EB run -c $W/edge.json > $W/edge.log 2>&1 &
echo $! >> $W/pids
sleep 1
ip netns exec $X env HS2_NO_TUNE=1 $XENV $XB run -c $W/exit.json > $W/exit.log 2>&1 &
echo $! >> $W/pids
st() { echo "== $1 $(date +%T)" >> $W/status-edge.txt; timeout 10 $EB status -c $W/edge.json >> $W/status-edge.txt 2>&1; echo "== $1 $(date +%T)" >> $W/status-exit.txt; timeout 10 $XB status -c $W/exit.json >> $W/status-exit.txt 2>&1; }
sleep 15; st t15
dl=$(ip netns exec $E curl -s -m 30 -o /dev/null -w "%{size_download} %{speed_download} %{http_code}" http://127.0.0.1:8443/big; echo " rc=$?")
sleep 2; st after
{
echo "cell $NAME N=$N edge=$(basename $EB) [$EENV] exit=$(basename $XB) [$XENV]"
echo "download (bytes, B/s, http, curl rc): $dl"
echo "edge place: $(grep -h "place:" $W/status-edge.txt | tail -1 | sed "s/^ *//" | cut -c1-200)"
echo "edge links: $(grep -hiE "^ *links|serving" $W/status-edge.txt | tail -2 | tr -s " " | tr "\n" "|" | cut -c1-200)"
echo "exit links: $(grep -hiE "^ *links|pool|following" $W/status-exit.txt | tail -3 | tr -s " " | tr "\n" "|" | cut -c1-250)"
echo "edge QUIET up lines: $(grep -c "QUIET link q[0-9]* up" $W/edge.log), QUIET closed (follow): $(grep -c "QUIET link q[0-9]* closed" $W/edge.log)"
echo "edge follow lines: $(grep -h "HS2_TUNE_QUIET_FOLLOW\|leaves room" $W/edge.log | sed "s/^[0-9\/: ]*//" | head -2 | tr "\n" "|")"
echo "refusal lines: edge: $(grep -hiE "refus|upgrade|v2|plain website|older" $W/edge.log | sed "s/^[0-9\/: ]*//" | sort | uniq -c | sort -rn | head -2 | tr -s " " | tr "\n" "|" | cut -c1-300)"
echo "               exit: $(grep -hiE "refus|upgrade|v2|plain website|older" $W/exit.log | sed "s/^[0-9\/: ]*//" | sort | uniq -c | sort -rn | head -2 | tr -s " " | tr "\n" "|" | cut -c1-300)"
} > $W/result.txt
for f in $W/*.log $W/status-*.txt; do sed -i "s/$KEY/<key>/g; s#hs2://[^ \"]*#<hs2-link>#g" $f; done
cat $W/result.txt
