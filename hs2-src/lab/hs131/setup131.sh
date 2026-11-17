#!/bin/bash
set -u
D=/opt/hs2lab/hs131; S=/opt/hs2lab/hs130
mkdir -p $D/res
sed 's#/opt/hs2lab/hs130#/opt/hs2lab/hs131#g; s#d130-#d131-#g' $S/d130.sh > $D/d131.sh
sed 's#/opt/hs2lab/hs130#/opt/hs2lab/hs131#g; s#d130.sh#d131.sh#g' $S/seq130.sh > $D/seq131.sh
ln -sfn $S/bin $D/bin
cp $S/ana130.py $S/callep130.py $S/stat130.py $D/ 2>/dev/null
V=HEAVY_CTL=direct,HEAVY_P=8
: > $D/j2.txt
for i in 1 2 3 4 5 6; do
  case $((i%4)) in 1) o="A B C D";; 2) o="B D A C";; 3) o="C A D B";; 0) o="D C B A";; esac
  for a in $o; do
    case $a in A) v=$V;; B) v="EX_KH=HS2_MCONN_STRIPE_KEEPSTALL=1,$V";; C) v="EX_IR=HS2_MCONN_STRIPE_KEEPSTALL=1,$V";; D) v=$V;; esac
    e=GODEBUG=gctrace=1; [ $a = D ] && e=GODEBUG=gctrace=1,HS2_MCONN_STRIPE_KEEPSTALL=1
    echo "ko$a$i Q hs2-k128 hs2-k128 $e $v" >> $D/j2.txt
  done
done
cat > $D/go131.sh <<'G'
#!/bin/bash
set -u
D=/opt/hs2lab/hs131
while [ -e /proc/3514254 ]; do sleep 10; done
echo "$(date +%T) hs130runs done, start" >> $D/res/seq.log
for w in 0 1 2; do bash $D/seq131.sh $w 3 $D/j2.txt & sleep 20; done
wait
echo "$(date +%T) RUNSDONE" >> $D/res/seq.log
G
chmod +x $D/*.sh
cat $D/j2.txt
grep -n EX_ $D/d131.sh
systemctl is-active hs131runs 2>/dev/null || systemd-run --unit hs131runs /usr/bin/bash $D/go131.sh
