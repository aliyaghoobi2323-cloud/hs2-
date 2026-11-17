#!/bin/bash
# keep a copy of the soak hs0 ping.txt (the soak rm -rf its workdir at the end)
while ! systemctl is-active -q r66soak; do sleep 10; done
while systemctl is-active -q r66soak; do cp /tmp/r66soakw30/ping.txt /opt/hs2lab/r66/soak/ping.copy.txt 2>/dev/null; sleep 30; done
