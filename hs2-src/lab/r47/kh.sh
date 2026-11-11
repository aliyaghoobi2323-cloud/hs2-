#!/bin/bash
# kharej (reverse dialer): no front config at all — every path front comes from the Iran announcement.
F=$3; D=/opt/hs2lab/r47
# rel47: every run starts cold (no remembered fronts from an earlier run in this slot)
[ -n "${KEEPPF:-}" ] || rm -f "${F%.json}.peer-fronts.json"
for p in /proc/$BASHPID/fd/*; do n=${p##*/}; [ "$n" -gt 2 ] && eval "exec $n>&-" 2>/dev/null; done
ME=$$
SF=/run/hs2/$(echo "$F" | sed "s|^/||; s|/|-|g").status.json
OUT=${P3_OUT:-/tmp/p3}; mkdir -p $OUT
( t0=$(date +%s); while kill -0 $ME 2>/dev/null; do t=$(( $(date +%s)-t0 )); python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(json.dumps({\"t\":int(sys.argv[2]),\"front_paths\":d.get(\"front_paths\"),\"lb\":[(p.get(\"path\"),p.get(\"links\"),p.get(\"state\",\"ok\")) for p in d.get(\"lb\",[])]}))" $SF $t >> $OUT/kh-status.jsonl 2>/dev/null; sleep 1; done ) </dev/null >/dev/null 2>&1 &
exec $D/${HS2BIN:-hs2-r45} "$@"
