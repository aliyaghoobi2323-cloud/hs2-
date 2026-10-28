#!/bin/bash
# fuzzall.sh — the red team's fuzz runs, one target at a time, each only
# while no rig lock is held (redrun.sh).
R=/root/hs2-wt/r1-red/hs2-src
G=/usr/local/go/bin/go
run() { name=$1; dir=$2; target=$3; secs=$4
	/root/rred/redrun.sh fz-$name $dir $G test -run '^$' -fuzz "^$target\$" -fuzztime ${secs}s -parallel 4 .
}
run session  $R/third_party/smux FuzzRedSessionFrames 120
run reasm    $R/third_party/smux FuzzGroupReassembly 60
run auth     $R/tlscarrier       FuzzParseClientAuth 45
run tundesc  $R/tlscarrier       FuzzParseTunDesc 30
run proxy    $R/engine           FuzzProxyHeader 45
run owd      $R/engine           FuzzOWDDecode 30
run hint     $R/engine           FuzzHintDecode 20
run flowkey  $R/engine           FuzzFlowKeyRev 30
run link     $R/cmd/hs2          FuzzDecodeLink 45
echo done > /root/rred/fuzzall.done
