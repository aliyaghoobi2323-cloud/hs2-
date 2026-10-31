#!/bin/bash
# three pieces, 2 + 2 + 1, a pause between them for whoever waits for the test lock
T=/root/rmerge/tools
flock /root/r1-tests.lock "$T/load.sh" 1 2
sleep 120
flock /root/r1-tests.lock "$T/load.sh" 2 2
sleep 120
flock /root/r1-tests.lock "$T/load.sh" 3 1
echo "end $(date -u +%FT%TZ)" >> /root/rmerge/load.log
