#!/bin/bash
busy() { pgrep -f -- "go test|go build|\.test -test\.|/go-build[0-9]*/.*\.test" > /dev/null; }
until [ ! -e /root/r1-quiet.lock ] && ! busy; do sleep 2; done
cd /root/hs2-wt/r1-r3a/hs2-src && nice /usr/local/go/bin/go test -count=1 -run "TestPeerInfo" -v ./engine > /root/r3a/tests/peerinfo-rerun.txt 2>&1; echo "exit $?" >> /root/r3a/tests/peerinfo-rerun.txt
