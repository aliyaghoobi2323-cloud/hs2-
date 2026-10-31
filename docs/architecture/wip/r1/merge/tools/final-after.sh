#!/bin/bash
while kill -0 1929278 2>/dev/null; do sleep 10; done
exec /root/rmerge/tools/final.sh
