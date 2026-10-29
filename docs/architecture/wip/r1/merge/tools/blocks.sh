#!/bin/bash
# blocks.sh WHICH: the bodies of the R1 merge's locked test blocks (step 5),
# run by lockrun.sh in /root/hs2-wt/r1-merge. Summaries to /root/rmerge/suite,
# go test -json streams to /root/rmerge/raw-suite (both named by head + time).
set -u
export PATH=$PATH:/usr/local/go/bin
W=/root/hs2-wt/r1-merge; S=$W/hs2-src
cd "$S" || exit 1
H=$(git rev-parse --short HEAD); T=$(date -u +%Y%m%dT%H%M%SZ)
SU=/root/rmerge/suite; RAW=/root/rmerge/raw-suite
SJ=/root/rmerge/tools/sumjson.py
echo "== $1 on $H ($(git log -1 --format=%s | cut -c1-90)) $(date -u +%FT%TZ)"
git status --short | sed "s/^/== dirty: /"
go version
# gt NAME DIR ARGS...: go test -json in DIR, summed to SU/NAME-H-T.txt
gt() {
  local n=$1 d=$2; shift 2
  local t0=$(date +%s)
  (cd "$d" && go test -json "$@") > "$RAW/$n-$H-$T.json" 2> "$RAW/$n-$H-$T.err"
  local r=$?
  { echo "== go test $* (in ${d#$W/}): rc $r, $(( $(date +%s) - t0 )) s"; python3 "$SJ" "$RAW/$n-$H-$T.json" "$n"; cat "$RAW/$n-$H-$T.err"; } > "$SU/$n-$H-$T.txt" 2>&1
  echo "-- $n rc $r $(( $(date +%s) - t0 )) s: $(grep "^TOTAL" "$SU/$n-$H-$T.txt")"
  return $r
}
RACE_RUN='Hint|MoveBack|MoveRule|QuietCounts|QuietMove|QuietTunPlacement|OWD|Budget|Panel|LinkSndbuf|Moved|UDPLate|LocalTarget|Group|Coalesce|Events|QuietRecords|HostQ|Lowat|Gate|HostWait'
JUDGE_RACE='Quiet|Follow|RTHold|RTShare|LatRR|RTLane|SchedRT|Sched|Budget|HostGate|HostWait|Kill|ByUser|HoldCap|Rescue|TwoWay|Place|Hint|Red|AngleTunPing'
case "$1" in
build)
  # the rig binaries from this head (BUILD.md release flags) and rigload
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /root/rmerge/raw-bins/hs2-m-$H ./cmd/hs2 && echo "-- built /root/rmerge/raw-bins/hs2-m-$H"
  (cd "$W/docs/architecture/wip/rig-scale/rigload" && CGO_ENABLED=0 go build -o /root/rmerge/raw-bins/rigload-$H .) && echo "-- built rigload-$H"
  (cd /root/rmerge/raw-bins && sha256sum hs2-m-$H rigload-$H)
  ;;
udp)
  # TestUDPUserPortRepliesFromTheAddressUsed (CI red on r1-merge): before
  # (3fcc588) and after the freePort fix, -count=20 each, alone and while
  # udphold.py holds UDP on 127.0.0.1 for every 4th port of the ephemeral
  # range (another program's UDP sockets, as xray's on this host)
  P=/root/rmerge/wt-pre
  [ -d "$P" ] || git -C "$W" worktree add -q --detach "$P" 3fcc588
  (cd "$P/hs2-src" && go test -c -o /root/rmerge/raw-bins/engine-pre.test ./engine) || exit 1
  go test -c -o /root/rmerge/raw-bins/engine-post-$H.test ./engine || exit 1
  RX='^TestUDPUserPortRepliesFromTheAddressUsed$'
  RX3='^(TestUDPUserPortRepliesFromTheAddressUsed|TestStreamTCPAndUDP|TestReverseStreamTCPAndUDP)$'
  ab() { # ab NAME BIN RUN COUNT HOLD
    local o=$SU/udp-$1-$H-$T.txt t0=$(date +%s) hp=""
    if [ "$5" = 1 ]; then python3 /root/rmerge/tools/udphold.py 4 1 600 > $SU/udp-$1-hold-$H-$T.txt 2>&1 & hp=$!; sleep 3; cat $SU/udp-$1-hold-$H-$T.txt; fi
    (cd "$S/engine" && "$2" -test.run "$3" -test.count=$4 -test.v -test.timeout 15m) > "$o" 2>&1
    [ -n "$hp" ] && kill $hp
    echo "-- udp $1: pass $(grep -c "^--- PASS" "$o"), fail $(grep -c "^--- FAIL" "$o"), $(( $(date +%s) - t0 )) s; $(grep -m1 "got 0 of 10\|never answered" "$o")"
  }
  ab pre-alone /root/rmerge/raw-bins/engine-pre.test "$RX" 20 0
  ab post-alone /root/rmerge/raw-bins/engine-post-$H.test "$RX" 20 0
  ab pre-held /root/rmerge/raw-bins/engine-pre.test "$RX" 20 1
  ab post-held /root/rmerge/raw-bins/engine-post-$H.test "$RX" 20 1
  ab post-held-three /root/rmerge/raw-bins/engine-post-$H.test "$RX3" 5 1
  ;;
release)
  # make release from this head (BUILD.md: CGO_ENABLED=0, -trimpath, -s -w),
  # amd64, 386 and arm64, into /root/rmerge/raw-dist-H (new each head), then the
  # stamps; nothing is installed anywhere
  make release DIST=/root/rmerge/raw-dist-$H || exit 1
  (cd /root/rmerge/raw-dist-$H && ls -l hs2-linux-* && for b in hs2-linux-*; do echo "== $b"; go version -m "$b" | grep -E "vcs.revision|vcs.modified|CGO_ENABLED|-trimpath|GOARCH"; done; ./hs2-linux-amd64 version; file hs2-linux-* 2>/dev/null)
  ;;
check)
  # make check: vet, fmt, test, smux (the same go test commands, with -json
  # for the counts); and the rig's own module rigload and ana_test.py
  { echo "== make vet"; make vet; echo "rc $?"; echo "== make fmt (gofmt -l .)"; make fmt; echo "rc $?"; } > "$SU/vetfmt-$H-$T.txt" 2>&1
  (cd "$W/docs/architecture/wip/rig-scale/rigload" && go vet ./... && echo "rigload vet rc 0") >> "$SU/vetfmt-$H-$T.txt" 2>&1
  echo "-- vet/fmt: $(grep -c "^rc 0" "$SU/vetfmt-$H-$T.txt") of 2 make targets rc 0; $(tail -1 "$SU/vetfmt-$H-$T.txt")"
  gt test "$S" -count=1 -timeout 25m ./...
  gt smux "$S/third_party/smux" -count=1 -timeout 25m ./...
  gt rigload "$W/docs/architecture/wip/rig-scale/rigload" -count=1 -timeout 5m ./...
  (cd "$W/docs/architecture/wip/rig-scale" && timeout 120 python3 ana_test.py) > "$SU/ana_test-$H-$T.txt" 2>&1
  echo "-- ana_test.py rc $?: $(tail -1 "$SU/ana_test-$H-$T.txt")"
  ;;
race386)
  # first the UDP test A/B (the CI red on r1-merge), then -race and 386
  bash "$0" udp
  # make race (RACE_RUN, tlscarrier, cmd/..., smux SMUX_RACE) widened to the
  # round-4 judge's -race set and all of smux; rigload -race; then
  # GOARCH=386 -short (make test386's packages, and core and smux)
  gt race-engine "$S" -race -count=1 -timeout 30m -run "$RACE_RUN|$JUDGE_RACE" ./engine
  gt race-pkgs "$S" -race -count=1 -timeout 30m ./tlscarrier ./cmd/... ./core
  gt race-smux "$S/third_party/smux" -race -count=1 -timeout 30m ./...
  gt race-rigload "$W/docs/architecture/wip/rig-scale/rigload" -race -count=1 -timeout 10m ./...
  GOARCH=386 gt t386 "$S" -short -count=1 -timeout 25m ./engine ./tlscarrier ./cmd/hs2 ./core
  GOARCH=386 gt t386-smux "$S/third_party/smux" -short -count=1 -timeout 25m ./...
  ;;
fuzzkernel)
  # the UDP test A/B again: the first one held even ports, but Linux gives
  # bind(0) odd ports (connect() the even ones), so it never collided
  bash "$0" udp
  STEP5_IGNORE_LOCK=1 STEP5_OUT=/root/rmerge/step5 "$W/docs/architecture/wip/r1/merge-prep/step5/fuzz.sh"; echo "-- fuzz.sh rc $?"
  STEP5_IGNORE_LOCK=1 STEP5_OUT=/root/rmerge/step5 "$W/docs/architecture/wip/r1/merge-prep/step5/kernel.sh"; echo "-- kernel.sh rc $?"
  ;;
arm7)
  STEP5_IGNORE_LOCK=1 STEP5_OUT=/root/rmerge/step5 "$W/docs/architecture/wip/r1/merge-prep/step5/arm7.sh"; echo "-- arm7.sh rc $?"
  # the binaries (tens of MB) go to the raw directory: /root/rmerge keeps files under 5 MB
  mkdir -p /root/rmerge/raw-step5 && mv /root/rmerge/step5/*-bin /root/rmerge/step5/*-bundle /root/rmerge/raw-step5/ 2>/dev/null
  # the one test flaky.fa.md still has open, repeated
  gt flaky-red2pool "$S" -count=30 -timeout 15m -run '^TestRed2DeadLinkKeepsBorrowedPoolUntilRelaysLetGo$' ./engine
  ;;
engine)
  gt engine "$S" -count=1 -timeout 25m ./engine
  ;;
*) echo "unknown block $1"; exit 2 ;;
esac
echo "== done $1 $(date -u +%FT%TZ)"
