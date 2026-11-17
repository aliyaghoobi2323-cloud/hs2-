#!/bin/sh
# m4f251 bench (lab only): u32 filter count on the egress path vs per-packet
# cost of NON-tunnel traffic. netns m4a (10.77.0.1) --veth-- m4b (10.77.0.2).
# mode root: htb root + filters at parent 1fe1: (the exit, root mode)
# mode clsact: clsact egress + skbedit (mqchild/ifb steering)
# set old: 600 keys dst <16 IPs>/32 + dport <ephemeral>; new: 16 keys dst + proto 6 + sport 8443
set -u
B=${B:-./blast}; T=${T:-10}
setup() {
  ip netns del m4a 2>/dev/null; ip netns del m4b 2>/dev/null
  ip netns add m4a; ip netns add m4b
  ip link add va netns m4a type veth peer name vb netns m4b
  ip -n m4a addr add 10.77.0.1/24 dev va; ip -n m4b addr add 10.77.0.2/24 dev vb
  ip -n m4a link set va up; ip -n m4b link set vb up; ip -n m4a link set lo up; ip -n m4b link set lo up
  ip netns exec m4b iptables -A INPUT -p udp --dport 9 -j DROP 2>/dev/null || true
}
filters() { # $1 mode $2 set
  f=/tmp/m4f.batch; : > $f
  if [ "$1" = root ]; then
    echo "qdisc add dev va root handle 1fe1: htb default 1" >> $f
    echo "class add dev va parent 1fe1: classid 1fe1:1 htb rate 10gbit" >> $f
    echo "class add dev va parent 1fe1: classid 1fe1:10 htb rate 100mbit" >> $f
    tail="flowid 1fe1:10"; pre="filter add dev va parent 1fe1: protocol ip prio 16"
  else
    echo "qdisc add dev va clsact" >> $f
    tail="action skbedit priority 1fe1:10"; pre="filter add dev va egress protocol ip prio 16"
  fi
  if [ "$2" = old ]; then
    i=0; while [ $i -lt 600 ]; do echo "$pre u32 match ip dst 10.77.1.$((i%16+1))/32 match ip dport $((30000+i)) 0xffff $tail" >> $f; i=$((i+1)); done
  elif [ "$2" = new ]; then
    i=0; while [ $i -lt 16 ]; do echo "$pre u32 match ip dst 10.77.1.$((i+1))/32 match ip protocol 6 0xff match ip sport 8443 0xffff $tail" >> $f; i=$((i+1)); done
  fi
  ip netns exec m4a tc -batch $f
  echo "filters: $(ip netns exec m4a tc filter show dev va $( [ $1 = root ] && echo parent 1fe1: || echo egress) | grep -c 'match')"
}
cpu() { awk '/^cpu /{print $2+$3+$4+$5+$6+$7+$8, $4, $7}' /proc/stat; }
run() { # mode set
  setup; filters $1 $2
  # idle latency
  il=$(ip netns exec m4a ping -q -i 0.01 -c 300 10.77.0.2 | tail -1)
  c0=$(cpu)
  ip netns exec m4a ping -i 0.005 -c $((T*190)) 10.77.0.2 > /tmp/m4ping.txt &
  pp=$!
  r=$(ip netns exec m4a $B -t ${T}s -n 4)
  wait $pp
  c1=$(cpu)
  lat=$(grep -o 'time=[0-9.]*' /tmp/m4ping.txt | cut -d= -f2 | sort -n | awk '{a[NR]=$1} END{printf "n=%d p50=%.3f p99=%.3f p999=%.3f max=%.3f", NR, a[int(NR*.5)], a[int(NR*.99)], a[int(NR*.999)], a[NR]}')
  echo "$c0 $c1" | awk -v m=$1 -v s=$2 -v r="$r" -v nc=$(nproc) -v T=$T '{tot=$4-$1; sys=$5-$2; si=$6-$3; split(r,x," "); pps=x[2]; ns=(sys+si)/tot*nc*T*1e9/(pps*T); printf "RESULT mode=%s set=%s pps=%d sys=%.1f%% softirq=%.1f%% cpu_ns/pkt=%.0f ", m, s, pps, 100*sys/tot, 100*si/tot, ns}'
  echo "idle[$(echo $il | cut -d' ' -f4)] ping-under-load[$lat]"
}
for m in ${MODES:-root clsact}; do for s in ${SETS:-none old new}; do run $m $s; done; done
ip netns del m4a; ip netns del m4b
