#!/bin/bash
# block.sh PLAN OUT [SMOKE_OUT]: one locked rig block of step 2; with
# SMOKE_OUT, first a short smoke run (plans/smoke.txt, DUR=8) whose two runs
# must have game samples and a download, else the block stops there
D=$(cd "$(dirname "$0")" && pwd)
PLAN=$1; OUT=$2; SMOKE=${3:-}
if [ -n "$SMOKE" ]; then
  DUR=8 "$D/xrun.sh" "$D/plans/smoke.txt" "$SMOKE" || exit 1
  python3 - "$SMOKE/res.jsonl" <<'P' || { echo "smoke failed: block not run"; exit 1; }
import json, sys
rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
ok = len(rows) >= 2 and all((r.get("game") or {}).get("dl_p50") is not None and (r.get("dl_mbit") or 0) > 1 for r in rows[-2:])
print("smoke", "ok" if ok else "FAILED", [((r.get("game") or {}).get("dl_p50"), r.get("dl_mbit"), r.get("place")) for r in rows[-2:]])
sys.exit(0 if ok else 1)
P
fi
"$D/xrun.sh" "$PLAN" "$OUT"
