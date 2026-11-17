#!/bin/bash
# keep a copy of the soak hs0 ping.txt (the soak rm -rf its workdir at the end)
while ! systemctl is-active -q r66bsoak; do sleep 10; done
while systemctl is-active -q r66bsoak; do cp /tmp/r66bsoakw30/ping.txt /opt/hs2lab/r66b/soak/ping.copy.txt 2>/dev/null; sleep 30; done
