#!/bin/bash
S=$(cd "$(dirname "$0")" && pwd)
date -u +%T; for f in E C3 L F; do echo "$f: $(grep -c . $S/res/$f.jsonl 2>/dev/null) done, $(grep -c FAIL $S/res/$f.jsonl.log 2>/dev/null) FAIL; last: $(tail -n1 $S/res/$f.jsonl.log 2>/dev/null | cut -c1-70)"; done; cat $S/res/chain.out 2>/dev/null | tr '\n' ' '; echo
