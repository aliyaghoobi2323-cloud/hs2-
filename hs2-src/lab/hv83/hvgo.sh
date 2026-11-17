# hv83: many simultaneous heavy users with churn. $HV_USERS file, one user per line:
#   ID KIND STREAMS START DUR RATE   KIND f=fast download (spt from Iran netns), l=LTE download
#   (own netns, netem RATE, deep buffer), u=upload (iperf3 -P STREAMS to its own kharej port)
hv_go(){
  mkdir -p $W/hv; H83P=""
  ip netns exec $IR sysctl -qw net.ipv4.conf.all.route_localnet=1
  while read -r id kind st start dur rate; do
    [ -z "$id" ] && continue
    case $kind in
    f) ( sleep $start; ip netns exec $IR $SPTBIN -addr 127.0.0.1:8444 -n $st -t ${dur}s -pre 1s -post 1s > $W/hv/u$id.json 2>$W/hv/u$id.err ) & H83P="$H83P $!";;
    l) LT=lt${LAB_ID}k$id; LA=a${LAB_ID}k$id; LB=b${LAB_ID}k$id
       ip netns del $LT 2>/dev/null; ip netns add $LT; echo $LT >> $W/hvns
       ip link add $LA netns $IR type veth peer name $LB netns $LT
       ip -n $IR addr add 10.198.$id.1/24 dev $LA; ip -n $LT addr add 10.198.$id.2/24 dev $LB
       ip -n $IR link set $LA up; ip -n $LT link set $LB up; ip -n $LT link set lo up
       ip netns exec $IR ethtool -K $LA tso off gso off gro off >/dev/null 2>&1
       ip netns exec $LT ethtool -K $LB tso off gso off gro off >/dev/null 2>&1
       ip netns exec $IR tc qdisc replace dev $LA root netem delay 20ms rate $rate limit 2750
       ip netns exec $LT tc qdisc replace dev $LB root netem delay 20ms rate 30mbit limit 1500
       ip netns exec $LT sysctl -qw net.ipv4.tcp_rmem="4096 131072 8388608" net.ipv4.tcp_congestion_control=cubic
       ip netns exec $IR sysctl -qw net.ipv4.conf.$LA.route_localnet=1
       ip netns exec $IR iptables -t nat -A PREROUTING -i $LA -p tcp --dport 8444 -j DNAT --to-destination 127.0.0.1:8444
       ( sleep $start; ip netns exec $LT $SPTBIN -addr 10.198.$id.1:8444 -n $st -t ${dur}s -pre 1s -post 1s > $W/hv/u$id.json 2>$W/hv/u$id.err ) & H83P="$H83P $!";;
    u) ( sleep $start; ip netns exec $IR iperf3 -c 127.0.0.1 -p $((8460+id)) -P $st -t $dur -J > $W/hv/u$id.json 2>$W/hv/u$id.err ) & H83P="$H83P $!";;
    esac
  done < $HV_USERS
  # 1 s samples: both status.json, cpu ticks + rss of both daemons
  ( for i in $(seq 1 $(( ${T%s} + 6 ))); do sleep 1
      cp "$SF" $W/hv/si_$i.json 2>/dev/null; cp "$SFK" $W/hv/sk_$i.json 2>/dev/null
      echo "$(date +%s.%N) $(awk '{print $14+$15}' /proc/$IRPID/stat) $(awk '{print $14+$15}' /proc/$KHPID/stat) $(awk '/VmRSS/{print $2}' /proc/$IRPID/status) $(awk '/VmRSS/{print $2}' /proc/$KHPID/status)" >> $W/hv/proc.txt
    done ) & H83S=$!
}
hv_end(){
  for p in $H83P; do wait $p 2>/dev/null; done; kill $H83S 2>/dev/null
  cp $HV_USERS $W/hv/users.txt; cp -r $W/hv $LOGDIR/ 2>/dev/null
  for n in $(cat $W/hvns 2>/dev/null); do ip netns del $n 2>/dev/null; done
}
