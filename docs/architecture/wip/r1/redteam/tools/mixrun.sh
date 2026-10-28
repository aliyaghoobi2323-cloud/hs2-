#!/bin/bash
# mixrun.sh NAME EDGE_BIN EXIT_BIN [EXIT_BIN2] — the R1 red team's mixed-version
# check on two network namespaces of their own (rr-e, rr-x; no netem): the
# edge (Iran, dial) and the exit (Kharej, listen) with the binaries given;
# a 50 MB download through user port 8443 after 8 s; with EXIT_BIN2, the exit
# is then stopped and started again with it (an upgrade of the second server)
# and the download repeated after 20 s. Logs and results in /root/rred/mix/NAME.
# Aborts (and cleans up) the moment the shared rig lock appears.
set -u
NAME=$1; EB=$2; XB=$3; XB2=${4:-}
W=/root/rred/mix/$NAME; mkdir -p $W; rm -f $W/*.log $W/result.txt
C=/root/rred/mix
E=rr-e; X=rr-x
L=/root/r1-quiet.lock
cleanup() {
	for p in $(cat $W/pids 2>/dev/null); do kill $p 2>/dev/null; done
	sleep 0.5
	for p in $(cat $W/pids 2>/dev/null); do kill -9 $p 2>/dev/null; done
	rm -f $W/pids
	ip netns del $E 2>/dev/null; ip netns del $X 2>/dev/null
}
chk() { if [ -e $L ]; then echo "ABORTED: rig lock taken $(date -u +%T)" >> $W/result.txt; cleanup; exit 3; fi; }
trap cleanup EXIT
cleanup
chk
[ -f $C/cert.pem ] || openssl req -x509 -newkey rsa:2048 -nodes -keyout $C/key.pem \
	-out $C/cert.pem -days 30 -subj /CN=lab.example.com 2>/dev/null
[ -f $C/www/big ] || { mkdir -p $C/www; head -c 50000000 /dev/urandom > $C/www/big; }
ip netns add $E; ip netns add $X
ip link add rre0 netns $E type veth peer name rrx0 netns $X
ip -n $E addr add 192.168.77.1/24 dev rre0
ip -n $X addr add 192.168.77.2/24 dev rrx0
for n in $E $X; do ip -n $n link set lo up; done
ip -n $E link set rre0 up; ip -n $X link set rrx0 up
KEY=$(printf 'cd%.0s' {1..32})
cat > $W/kh.json <<J
{"mode":"listen","carrier":"mtcp","addr":"192.168.77.2:2096","iface":"rr0",
 "local_cidr":"10.79.0.2/30","peer_ip":"10.79.0.1","mtu":1380,
 "backend_addr":"builtin","shared_key":"$KEY","cert_file":"$C/cert.pem",
 "key_file":"$C/key.pem","expose":"127.0.0.1:5299"}
J
cat > $W/ir.json <<J
{"mode":"dial","carrier":"mtcp","addr":"192.168.77.2:2096","sni":"lab.example.com",
 "iface":"rr0","local_cidr":"10.79.0.1/30","peer_ip":"10.79.0.2","mtu":1380,
 "shared_key":"$KEY","forward_ports":"8443","min_links":2,"max_links":4}
J
( cd $C/www && exec ip netns exec $X python3 -m http.server 5299 --bind 127.0.0.1 ) > $W/www.log 2>&1 &
echo $! >> $W/pids
ip netns exec $X env HS2_NO_TUNE=1 $XB run -c $W/kh.json > $W/kh1.log 2>&1 &
echo $! > $W/xpid; cat $W/xpid >> $W/pids
sleep 1
ip netns exec $E env HS2_NO_TUNE=1 $EB run -c $W/ir.json > $W/ir.log 2>&1 &
echo $! >> $W/pids
dl() { # one download: its rate (bytes/s) and the curl exit code
	ip netns exec $E curl -s -m 20 -o /dev/null -w '%{speed_download} %{http_code}' http://127.0.0.1:8443/big; echo " rc=$?"
}
for i in $(seq 1 8); do sleep 1; chk; done
echo "phase1 $(basename $EB) <- $(basename $XB): $(dl)" >> $W/result.txt
chk
if [ -n "$XB2" ]; then
	kill $(cat $W/xpid); sleep 1; kill -9 $(cat $W/xpid) 2>/dev/null
	ip netns exec $X env HS2_NO_TUNE=1 $XB2 run -c $W/kh.json > $W/kh2.log 2>&1 &
	echo $! >> $W/pids
	t0=$(date +%s)
	for i in $(seq 1 20); do sleep 1; chk; done
	echo "phase2 $(basename $EB) <- $(basename $XB2): $(dl)" >> $W/result.txt
	echo "phase2 started $t0" >> $W/result.txt
fi
sleep 1
cp /run/hs2/root-rred-mix-$NAME-ir.json.status.json $W/ir.status.json 2>/dev/null
echo done >> $W/result.txt
