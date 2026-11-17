#!/bin/bash
# soakview.sh: rel59 soak at a glance (first/last proc samples per side, heavy, links)
set -u
O=${O:-/opt/hs2lab/rel59soak}
cat $O/env.txt; [ -e $O/DONE ] && cat $O/DONE
for s in ir kh; do echo "== $s"; grep " $s " $O/proc.txt | head -2; echo ...; grep " $s " $O/proc.txt | tail -3; done
echo "== missing: $(grep -c MISSING $O/proc.txt)"
echo "== heavy (last 5 of $(wc -l < $O/heavy.txt))"; tail -5 $O/heavy.txt
echo "== links (t ir kh)"; tail -3 $O/st.jsonl | python3 -c 'import sys,json
for l in sys.stdin:
  d=json.loads(l); print(d["t"], d["ir"].get("links"), d["kh"].get("links"))'
