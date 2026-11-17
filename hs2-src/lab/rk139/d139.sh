#!/bin/bash
# d139.sh TAG ARM MODE(d8|d1|u1|u8): rel62k2 edge A/B (copy of hv138/d138.sh) on the d61 lab (10 paths
# 45 ms, loss 0.2-2 %, crowd 3000 + call + game, one iperf3). ARM (edge / exit):
#   H rel61h2/rel61h2 (production now)   A rel62k2 edge + rel61h2 exit (the planned deploy)
# Lab = /opt/hs2lab/lbv81/labsrc/lab (run.sh with NETEM_EVENTS). Run via seq139.sh (labslot --all, lab2).
set -u
TAG=$1; ARM=$2; MD=$3; R=/opt/hs2lab/rk139; L=/opt/hs2lab/lbv81/labsrc/lab
export P3_OUT=$R/res/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
WD=/tmp/h139-$TAG; W=$WD$LAB_ID
case $ARM in
  H) BIN=$R/hs2R61H2; BK=$R/hs2R61H2;;
  A) BIN=$R/hs2K2; BK=$R/hs2R61H2;;
  *) echo "unknown ARM $ARM" >&2; exit 2;;
esac
echo "ARM=$ARM BIN=$(sha256sum $BIN|cut -c1-8) BIN_KH=$(sha256sum $BK|cut -c1-8)" > $P3_OUT/env.txt
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
HP=1; HV=down; case $MD in d8|u8) HP=8;; esac; case $MD in u1|u8) HV=up;; esac
export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 2%" PATH_E_NETEM="loss random 0.3%" PATH_F_NETEM="loss random 0.8%" PATH_G_NETEM="loss random 1.5%" PATH_H_NETEM="loss random 0.2%" PATH_I_NETEM="loss random 0.6%" PATH_J_NETEM="loss random 1.2%"
export T=16s HEAVY_SECS=12 HEAVY_OMIT=0 BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD PATHS=10 LB_MODE=auto LOSS=0 KILL_AT=0
export PPROF=0 HEAVY_CTL=direct
env BIN=$BIN BIN_KH=$BK HEAVY=$HV HEAVY_P=$HP LOGDIR=$P3_OUT RUNTAG=$TAG timeout 600 bash $L/run.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
cp $W/heavy.json $P3_OUT/ 2>/dev/null
for s in ir kh; do echo "$s panics $(grep -a -c -E 'panic|fatal error' $W/$s.log 2>/dev/null)"; done > $P3_OUT/panics.txt
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
echo "LAB_ID=${LAB_ID:-} ARM=$ARM MD=$MD" > $P3_OUT/meta.txt
rm -rf $W
