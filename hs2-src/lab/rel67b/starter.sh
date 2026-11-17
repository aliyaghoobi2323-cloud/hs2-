#!/bin/bash
# starter.sh: wait for r67soak (another soak) to end on its own, then start the rel67b 1 h soak
while systemctl is-active -q r67soak; do sleep 15; done
echo "r67soak inactive $(date -u +%FT%TZ)" > /opt/hs2lab/r67b/starter.log
sleep 20
systemd-run --unit r67bsoak --setenv=TT=3600 /opt/hs2lab/labslot.sh --all bash /opt/hs2lab/r67b/soak67b.sh >> /opt/hs2lab/r67b/starter.log 2>&1
systemd-run --unit r67bping bash /opt/hs2lab/r67b/pingkeep.sh >> /opt/hs2lab/r67b/starter.log 2>&1
systemd-run --unit r67bwatch bash -c "while ! test -e /opt/hs2lab/r67b/soak/t0; do sleep 10; done; exec bash /opt/hs2lab/r67b/watch.sh" >> /opt/hs2lab/r67b/starter.log 2>&1
echo "started $(date -u +%FT%TZ)" >> /opt/hs2lab/r67b/starter.log
