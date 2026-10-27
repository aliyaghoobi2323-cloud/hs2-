#!/bin/bash
# chain6.sh: after chain5 (the flock): build the final binary b2o (version 5
# with the jittered beat), the DPI captures again (capture.sh waits for the
# device to be up now), their post-processing, then the tests of the branch
# (no rig block runs then).
set -u
LOG=/root/r2j/chain.log
T=/root/r2j/tools
exec 9> /root/r2j/chain.flock
flock 9
echo "$(date +%T) chain6 pid $$ start" >> $LOG
cd /root/hs2-wt/r1-r2j/hs2-src
CGO_ENABLED=0 nice /usr/local/go/bin/go build -o /root/r2j/bins/hs2-b2o ./cmd/hs2 && sha256sum /root/r2j/bins/hs2-b2o >> /root/r2j/bins-sha256.txt
grep -q "^b2ox " /root/r2j/bins.txt || printf "b2ox /root/r2j/bins/hs2-b2o HS2_TUNE_QUIET_EXTRA=1\nb2oxr /root/r2j/bins/hs2-b2o HS2_TUNE_QUIET_EXTRA=1 HS2_TUNE_QUIET_REC=1\n" >> /root/r2j/bins.txt
echo "$(date +%T) b2o built" >> $LOG
sleep 180
R=/root/r2j/raw-dpi2
mkdir -p $R
$T/capture.sh $R "N=1 RATE=25 RTT=72 LOSS=0 MODE=fqcodel USERS=7 MIX=s5 DUR=20" 0 b2ox b2bbrq; echo "$(date +%T) capture A exit $?" >> $LOG
$T/capture.sh $R "N=8 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=7 MIX=s5 DUR=20" 96 old b1 b2 b2ox; echo "$(date +%T) capture B exit $?" >> $LOG
$T/capture.sh $R "N=1 RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=one_vs_16 DUR=20" 0 b2ox; echo "$(date +%T) capture C exit $?" >> $LOG
O=/root/r2j/dpi2
mkdir -p $O
cd $R
for p in cap-*.pcap; do
  t=${p#cap-}; t=${t%.pcap}
  cell=$(cat plan-$t.txt)
  case "$cell" in
    *"RATE=100"*"USERS=7"*) nice python3 $T/dpiconn.py $p --md "$t: $cell" > $O/conn-$t.md 2>&1 ;;
    *) nice python3 $T/tlsrec.py extract $p $O/rec-$t.json >> $O/extract.log 2>&1
       nice python3 $T/dpiconn.py $O/rec-$t.json --md "$t: $cell" --sizes $O/sizes-$t > $O/conn-$t.md 2>&1 ;;
  esac
done
ls -la $R/cap-*.pcap > $O/pcaps.txt
cat $R/tcpdump-*.txt > $O/tcpdump.txt
rm -f $R/cap-*.pcap
echo "$(date +%T) dpi2 done" >> $LOG
# the tests (no rig block now)
TO=/root/r2j/tests
mkdir -p $TO
cd /root/hs2-wt/r1-r2j/hs2-src
/usr/local/go/bin/go vet ./... > $TO/vet.txt 2>&1; echo "vet exit $?" >> $TO/vet.txt
/usr/local/go/bin/gofmt -l . > $TO/gofmt.txt 2>&1
nice /usr/local/go/bin/go test -count=1 -timeout 30m ./... > $TO/test-all.txt 2>&1; echo "exit $?" >> $TO/test-all.txt
echo "$(date +%T) go test ./... done" >> $LOG
nice /usr/local/go/bin/go test -race -count=1 -timeout 20m ./engine -run 'OWD|Alpha|Quiet|Group|Sched|SendCap|HostQ|Angle' > $TO/race-engine.txt 2>&1; echo "exit $?" >> $TO/race-engine.txt
(cd third_party/smux && nice /usr/local/go/bin/go test -race -count=1 ./... > $TO/race-smux.txt 2>&1; echo "exit $?" >> $TO/race-smux.txt)
nice /usr/local/go/bin/go test -race -count=1 ./obfs ./tlscarrier ./cmd/... > $TO/race-other.txt 2>&1; echo "exit $?" >> $TO/race-other.txt
echo "$(date +%T) chain6 done" >> $LOG
