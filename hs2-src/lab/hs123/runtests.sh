#!/bin/bash
# hs123: once the j2 workers are done, run tests123.sh on the idle host
set -u
while systemctl is-active --quiet hs123v0 || systemctl is-active --quiet hs123v1 || systemctl is-active --quiet hs123v2; do sleep 5; done
bash /opt/hs2lab/hs123/tests123.sh
