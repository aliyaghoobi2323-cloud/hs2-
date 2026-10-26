#!/bin/bash
# rig_test.sh [OUT]: the rig's own test (R1 task 2, step 5), on BIN=old ($OLD_BIN).
#  1. "N=1 RATE=50 RTT=72 LOSS=0 MODE=fqcodel USERS=1 MIX=lone16 BIN=old" twice
#     (REPS=2): two ok lines; in each the lone user's download >= 0.85 x RATE;
#     the two within 10 % of each other.
#  2. "N=$SCALE_N RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=s5 BIN=old"
#     once (SCALE_N, default 300): an ok line, all N links up at the start and
#     at the end, every user connected without dial errors, >= 99 % of the
#     probes and TUN pings answered.
# Exit status 0 when both hold. run.sh tears every run down (namespaces,
# processes, hs2 status files, keys), so nothing is left behind.
set -u
D=$(cd "$(dirname "$0")" && pwd)
OUT=${1:-/tmp/rig-scale-test}
SCALE_N=${SCALE_N:-300}
rm -rf "$OUT"; mkdir -p "$OUT"
echo "N=1 RATE=50 RTT=72 LOSS=0 MODE=fqcodel USERS=1 MIX=lone16 BIN=old" > "$OUT/plan1.txt"
echo "N=$SCALE_N RATE=100 RTT=72 LOSS=0 MODE=fqcodel USERS=200 MIX=s5 BIN=old" > "$OUT/plan2.txt"
REPS=2 "$D/run.sh" "$OUT/plan1.txt" "$OUT/t1"
REPS=1 "$D/run.sh" "$OUT/plan2.txt" "$OUT/t2"
python3 "$D/ana.py" "$OUT/t1/res.jsonl"
python3 "$D/ana.py" "$OUT/t2/res.jsonl"
python3 - "$OUT" "$SCALE_N" <<'EOF'
import json, sys
out, n = sys.argv[1], int(sys.argv[2])
bad = []
def lines(p):
    try:
        return [json.loads(l) for l in open(p) if l.strip()]
    except OSError:
        return []
r1 = [r for r in lines(out + "/t1/res.jsonl") if r.get("ok")]
v = [r["dn_sum_mbit"]["main"] for r in r1]
print("test 1: %d ok lines, lone user download %s Mbit/s (>= %.1f)" % (len(r1), v, 0.85 * 50))
if len(r1) != 2:
    bad.append("test 1: %d ok lines, want 2" % len(r1))
for x in v:
    if x < 0.85 * 50:
        bad.append("test 1: %.2f Mbit/s < 0.85 x RATE" % x)
if len(v) == 2:
    d = abs(v[0] - v[1]) / max(v)
    print("test 1: repeats differ by %.1f %% (< 10 %%)" % (100 * d))
    if d >= 0.10:
        bad.append("test 1: repeats differ by %.1f %%" % (100 * d))
r2 = lines(out + "/t2/res.jsonl")
ok2 = [r for r in r2 if r.get("ok")]
if len(ok2) != 1:
    bad.append("test 2: %d ok lines, want 1 (%s)" % (len(ok2), [r.get("errs") for r in r2]))
for r in ok2:
    lk = r["links"]
    de = sum(u.get("dial_errs", 0) for u in r["users"])
    pa, ta = r["probe"]["all"], (r.get("ping_tun") or {}).get("all") or {}
    print("test 2: N=%d links at start %s, at end %s (edge status %s), up in %.1f s; dn %.1f up %.1f Mbit/s; "
          "probes %d/%d, TUN pings %s/%s answered; dial errors %d; CPU edge %s exit %s %%; host busy %s %%" % (
          n, lk["at_start"], lk["end"], lk["edge_status"], lk["wait_s"], r["dn_sum_mbit"]["main"], r["up_sum_mbit"]["main"],
          pa["n"], pa["sent"], ta.get("n"), ta.get("sent"), de, r["cpu"].get("edge", {}).get("mean"),
          r["cpu"].get("exit", {}).get("mean"), r.get("host_busy_pct")))
    if lk["at_start"] < n or lk["end"] < n:
        bad.append("test 2: links %s/%s of %d" % (lk["at_start"], lk["end"], n))
    if de:
        bad.append("test 2: %d dial errors" % de)
    if pa["n"] < 0.99 * pa["sent"] or (ta and ta["n"] < 0.99 * ta["sent"]):
        bad.append("test 2: probes or pings unanswered")
for b in bad:
    print("FAIL " + b)
print("rig_test: " + ("PASS" if not bad else "FAIL"))
sys.exit(1 if bad else 0)
EOF
