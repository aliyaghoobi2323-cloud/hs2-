#!/bin/bash
set -u
D=/opt/hs2lab/hs131; J=$D/j3.txt; K=HS2_MCONN_STRIPE_KEEPSTALL=1
{
echo "xdfx1 bS hs2-k131 hs2-k131 $K HEAVY_CTL=direct"
echo "xufx1 bS hs2-k131 hs2-k131 $K HEAVY_CTL=direct,HEAVY=up"
echo "xdfx2 bS hs2-k131 hs2-k131 $K HEAVY_CTL=direct"
echo "xufx2 bS hs2-k131 hs2-k131 $K HEAVY_CTL=direct,HEAVY=up"
for i in 1 2 3 4 5 6 7 8; do
  a="qd8fx$i Q hs2-k131 hs2-k131 $K HEAVY_CTL=direct,HEAVY_P=8"; b="qd8fo$i Q hs2-r63 hs2-r63 - HEAVY_CTL=direct,HEAVY_P=8"
  if [ $((i%2)) = 1 ]; then echo "$a"; echo "$b"; else echo "$b"; echo "$a"; fi
done
for i in 1 2 3 4; do echo "qd1fx$i Q hs2-k131 hs2-k131 $K HEAVY_CTL=direct,HEAVY_P=1"; echo "qd1fo$i Q hs2-r63 hs2-r63 - HEAVY_CTL=direct,HEAVY_P=1"; done
for i in 1 2 3; do echo "qu8fo$i Q hs2-r63 hs2-r63 - HEAVY_CTL=direct,HEAVY=up,HEAVY_P=8"; echo "qu8fx$i Q hs2-k131 hs2-k131 $K HEAVY_CTL=direct,HEAVY=up,HEAVY_P=8"; done
} > $J
cat > $D/go131b.sh <<'G'
#!/bin/bash
set -u
D=/opt/hs2lab/hs131
while systemctl is-active --quiet hs131runs; do sleep 10; done
echo "$(date +%T) knock-out done, start j3 (fixed k131 vs r63 off)" >> $D/res/seq.log
for w in 0 1 2; do bash $D/seq131.sh $w 3 $D/j3.txt & sleep 20; done
wait
echo "$(date +%T) RUNSDONE j3" >> $D/res/seq.log
G
chmod +x $D/go131b.sh
wc -l $J
systemctl is-active hs131retest 2>/dev/null || systemd-run --unit hs131retest /usr/bin/bash $D/go131b.sh
