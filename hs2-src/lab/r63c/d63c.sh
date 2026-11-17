#!/bin/bash
# r63d: RS=/opt/hs2lab/r63c/res63d selects another result dir (default res).
# d63c.sh TAG ARM SC (lab1, via seq63c.sh + labslot): rel63c A/B.
#   ARM (edge/exit): N rel63c/rel63c   P rel62k3/rel61h2 (production now)   M rel62k3/rel63c (mixed, exit first)
#                    E rel63c/rel61h2 (mixed, edge first; r63d)
#                    O rel63c/rel63c with HS2_MCONN_STRIPE_KEEPSTALL=0 both sides (u1 attribution)
#                    K rel63c without mt135 (5b1bfd38 = rel62k3 + keepstall only) both sides (u1 attribution)
#   SC quiet d1|d8|u1|u8: d139 lab (lbv81 run.sh, 10 paths 45 ms loss 0.2-2 %, crowd 3000 + call + game, T 16 s)
#   SC dk: d133 dark download (fl106 run117.sh, 5 paths, path 3 blackholed 30..90 s, T 150 s, HEAVY down P1 direct ctl)
# Every 5 s: RSS (MB) of both hs2 processes -> rss.txt. Results /opt/hs2lab/r63c/res/TAG.
set -u
QX=
TAG=$1; ARM=$2; SC=$3; R=/opt/hs2lab/r63c; B=$R/bin; RS=${RS:-$R/res}
export P3_OUT=$RS/$TAG; rm -rf $P3_OUT; mkdir -p $P3_OUT
case $ARM in
  N) BIN=$B/hs2R63C; BK=$B/hs2R63C;;
  P) BIN=$B/hs2R62K3; BK=$B/hs2R61H2;;
  M) BIN=$B/hs2R62K3; BK=$B/hs2R63C;;
  E) BIN=$B/hs2R63C; BK=$B/hs2R61H2;;
  L) BIN=$B/hs2R63C; BK=$B/hs2R63C; QX=HS2_MCONN_STRIPE_LOG=1;;  # r63d: N with keepstall counters (ir/kh.keep.txt)
  W) BIN=$B/hs2R62K3; BK=$B/hs2R63C; QX=HS2_MCONN_STRIPE_LOG=1;; # r63d: M with keepstall counters
  K) BIN=$B/hs2R63K; BK=$B/hs2R63K;;
  O) BIN=$B/hs2R63C; BK=$B/hs2R63C; QX=HS2_MCONN_STRIPE_KEEPSTALL=0;;
  *) echo "unknown ARM $ARM" >&2; exit 2;;
esac
WD=/tmp/r63c-$TAG; W=$WD${LAB_ID:-}
echo "ARM=$ARM SC=$SC BIN=$(sha256sum $BIN|cut -c1-8) BIN_KH=$(sha256sum $BK|cut -c1-8) LAB_ID=${LAB_ID:-} QX=$QX" > $P3_OUT/env.txt
( for i in $(seq 1 90); do sleep 5; [ -d $P3_OUT ] && [ ! -e $P3_OUT/end ] || break
    for ns in ir kh; do r=0; for p in $(ip netns pids $ns${LAB_ID:-} 2>/dev/null); do
        case "$(readlink /proc/$p/exe 2>/dev/null)" in $W/hs2*) r=$(awk '/VmRSS/{print int($2/1024)}' /proc/$p/status 2>/dev/null);; esac; done
      echo "$((i*5)) $ns ${r:-0}"; done >> $P3_OUT/rss.txt; done ) & RSP=$!
# r63d: the quiet runs' heavy uses --cport 41000(+i) (run.sh): reserve 41000-41063 in this slot's Iran netns as soon as
# it exists, so the crowd's ephemeral ports cannot take them ("unable to connect stream: Address already in use").
( for i in $(seq 1 300); do ip netns exec ir${LAB_ID:-} sysctl -qw net.ipv4.ip_local_reserved_ports=41000-41063 2>/dev/null && break; sleep 0.2; done ) &
export HOME=/root GOPATH=/root/go PATH=$PATH:/usr/local/go/bin GOPROXY=off GOFLAGS=-mod=mod MODE=l3mtcp EMU=kernel RATE=1000mbit DELAY=45ms QUEUE=100ms
export BULK=0 CALL=2mbit GAME=1 MIN_LINKS=100 MAX_LINKS=150 CROWD=3000 REVERSE=1 NOWARM=1 WORKDIR=$WD LB_MODE=auto LOSS=0 KILL_AT=0 PPROF=0 HEAVY_CTL=direct
export KHAREJ_EXTRA="\"max_links\":150,\"min_links\":100,"
export HEAVY_CPORT=$(( 41000 + 64 * (RANDOM % 300) ))
if [ $SC = dk ]; then
  L=/opt/hs2lab/fl106/labsrc/lab; X="HS2_LB_REC_DEBUG=1 HS2_MCONN_STRIPE_LOG=1"
  export T=150s KH2=1 PAIR_EVENTS="1:1:0.025 30:3:1 90:3:1d" LOSS_EVENTS="" PATHS=5
  export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 0.3%" PATH_E_NETEM="loss random 0.8%"
  export SPTBIN=/opt/hs2lab/cg86/spt PROBE_EXTRA="-crowdlife 40s -samplelog $P3_OUT/samples.txt"
  env BIN=$BIN BIN_KH=$BK IR_ENV="$X" KH_ENV="$X" HEAVY=down HEAVY_P=1 LOGDIR=$P3_OUT RUNTAG=$TAG timeout 600 bash $L/run117.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
  cp $W/lbtime.txt $P3_OUT/ 2>/dev/null
else
  L=/opt/hs2lab/lbv81/labsrc/lab; HP=1; HV=down; case $SC in d8|u8) HP=8;; esac; case $SC in u1|u8) HV=up;; esac
  export T=16s HEAVY_SECS=12 HEAVY_OMIT=0 PATHS=10
  export PATH_A_NETEM="loss random 0.2%" PATH_B_NETEM="loss random 0.5%" PATH_C_NETEM="loss random 1%" PATH_D_NETEM="loss random 2%" PATH_E_NETEM="loss random 0.3%" PATH_F_NETEM="loss random 0.8%" PATH_G_NETEM="loss random 1.5%" PATH_H_NETEM="loss random 0.2%" PATH_I_NETEM="loss random 0.6%" PATH_J_NETEM="loss random 1.2%"
  env BIN=$BIN BIN_KH=$BK IR_ENV="$QX" KH_ENV="$QX" HEAVY=$HV HEAVY_P=$HP LOGDIR=$P3_OUT RUNTAG=$TAG timeout 600 bash $L/run.sh > $P3_OUT/run.out 2> $P3_OUT/run.err
fi
touch $P3_OUT/end; kill $RSP 2>/dev/null
cp $W/heavy.json $P3_OUT/ 2>/dev/null
for s in ir kh; do echo "$s panics $(grep -a -c -E 'panic|fatal error' $W/$s.log 2>/dev/null)"; grep -a -E 'keepstall|ack-stall' $W/$s.log 2>/dev/null | tail -50 > $P3_OUT/$s.keep.txt; done > $P3_OUT/panics.txt
tail -1 $P3_OUT/run.out > $P3_OUT/result.json
rm -rf $W
