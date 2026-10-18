# mkZ.py NAME REPS VARIANTS SCENS...: a z13 plan (Phase Z5 full matrix: base vs n4 vs core).
# Interleaved: each repetition runs every scenario across all variants back to back, the
# variants' order rotated by repetition; a core run gets its own window core trace file.
# Every run has Z5_TXQ=10000. RTT 500 cells get 10 s warm-up + 30 s measure.
# scen syntax: kind[+ENV=V...]@dn-rtt[-l<loss%>][-up<mbit>][-q<ms>]
# variants: base, n4, core; split variants for TestZ5LM: E<edge>X<exit> e.g. EcoreXbase
import sys, re
S = "/tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z13"
VENV = {"base": "", "n4": "Z5_POOL=128", "core": "Z5_POOL=128"}

def var(v):
    if v in VENV:
        return v, VENV[v]
    m = re.fullmatch(r"E(\w+?)X(\w+)", v)
    if m:
        e, x = m.group(1), m.group(2)
        return f"lm{e}:lm{x}", "Z5_POOL=128"
    raise SystemExit("unknown variant " + v)

COMMON = "Z5_TRACE=1 Z5_TRACE2=1"
W = "Z5_WARM_S=6 Z5_DUR_S=24"
WL = "Z5_WARM_S=10 Z5_DUR_S=30"
KIND = {
 "dl1i": f"Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 {W} {COMMON}",
 "ul1i": f"Z5_RELAY=1 Z5_UPL=1 Z5_INT=1 {W} {COMMON}",
 "dl4": f"Z5_RELAY=1 Z5_FAST=4 Z5_INT=1 {W} {COMMON}",
 "dl8": f"Z5_RELAY=1 Z5_FAST=8 Z5_INT=1 {W} {COMMON}",
 "dl16": f"Z5_RELAY=1 Z5_FAST=16 Z5_INT=1 {W} {COMMON}",
 "dlul": f"Z5_RELAY=1 Z5_FAST=1 Z5_UPL=1 Z5_INT=1 {W} {COMMON}",
 "dlchg": f"Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=36 Z5_CHG_MBIT=50 Z5_CHG_AT=18 Z5_CHG_S=12 {COMMON}",
 "ulchg": f"Z5_RELAY=1 Z5_UPL=1 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=36 Z5_CHG_MBIT=50 Z5_CHG_AT=18 Z5_CHG_S=12 {COMMON}",
 # stall matrix of phases Z/Z2 (as in the earlier rounds' mkplan.py), with the 1 s tracer
 "slow2": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_SLOW=2 Z5_SLOW_MBIT=2 Z5_WARM_S=8 Z5_DUR_S=15 Z5_TRACE=1",
 "slow4": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_SLOW=4 Z5_SLOW_MBIT=2 Z5_WARM_S=8 Z5_DUR_S=15 Z5_TRACE=1",
 "slow8": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_SLOW=8 Z5_SLOW_MBIT=2 Z5_WARM_S=8 Z5_DUR_S=15 Z5_TRACE=1",
 "slow16": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_SLOW=16 Z5_SLOW_MBIT=2 Z5_WARM_S=8 Z5_DUR_S=15 Z5_TRACE=1",
 "frz32": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=32 Z5_FREEZE_KB=16 Z5_WARM_S=8 Z5_DUR_S=15 Z5_TRACE=1",
 "ftf4": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=4 Z5_FREEZE_KB=32768 Z5_WARM_S=4 Z5_DUR_S=20 Z5_TRACE=1",
 "ftf8": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=8 Z5_FREEZE_KB=32768 Z5_WARM_S=4 Z5_DUR_S=20 Z5_TRACE=1",
 "z3f8s": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=3 Z5_FREEZE_KB=8192 Z5_SLOW=8 Z5_SLOW_MBIT=8 Z5_WARM_S=8 Z5_DUR_S=15 Z5_TRACE=1",
 "fts12": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FTS=12 Z5_FTS_S=3 Z5_FTS_KBS=32 Z5_F_AT=0 Z5_BG_AT=2 Z5_WARM_S=4 Z5_DUR_S=20 Z5_TRACE=1",
 "seqf6": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=6 Z5_FREEZE_KB=65536 Z5_FREEZE_SEQ_S=1 Z5_WARM_S=4 Z5_DUR_S=30 Z5_TRACE=1",
 "seqf12": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=12 Z5_FREEZE_KB=32768 Z5_FREEZE_SEQ_S=1 Z5_WARM_S=4 Z5_DUR_S=36 Z5_TRACE=1",
 # full edge stack (TestZ5LM)
 "lm1": "Z5_TEST=TestZ5LM Z5_LM_DUR_S=130",
 "lm1u": "Z5_TEST=TestZ5LM Z5_LM_UP=1 Z5_LM_DUR_S=130",
 # multi-link (as z12): one big download from the start, 4 interactive users at 15/30/45/60 s
 "lmi": "Z5_TEST=TestZ5LM Z5_LM_DUR_S=90 Z5_LM_INTS=4 Z5_LM_INT_AT=15 Z5_LM_INT_EVERY=15",
 # gap 2: 4 links, 2 big downloads; the first one's link black-holed (both ways, left open) at 40 s;
 # a short user (200 B request, 2 KiB reply, close) every 250 ms from 35 s to 60 s
 "lmcut": "Z5_TEST=TestZ5LM Z5_LM_DUR_S=90 Z5_LM_MIN=4 Z5_LM_MAX=6 Z5_LM_USERS=2 Z5_LM_CUT_AT=40 "
          "Z5_LM_SHORT_FROM=35 Z5_LM_SHORT_TO=60 Z5_LM_SHORT_MS=250",
 # gap 2, the other way: the serving link of the lowest id that carries neither download is black-holed
 "lmcuti": "Z5_TEST=TestZ5LM Z5_LM_DUR_S=90 Z5_LM_MIN=4 Z5_LM_MAX=6 Z5_LM_USERS=2 Z5_LM_CUT_AT=40 Z5_LM_CUT_IDLE=1 "
          "Z5_LM_SHORT_FROM=35 Z5_LM_SHORT_TO=60 Z5_LM_SHORT_MS=250",
 # gap 3: 2 links, the newest one 300 ms worse (RTT) once the pool is up; 30 s with a short user every
 # 250 ms to be judged on, then a big download (it should land on the good link); 3 interactive users
 # at 30/50/70 s of the download
 "lmbad": "Z5_TEST=TestZ5LM Z5_LM_DUR_S=100 Z5_LM_MIN=2 Z5_LM_MAX=2 Z5_LM_BAD_MS=300 Z5_LM_SETTLE_S=30 Z5_LM_PRE_MS=250 "
          "Z5_LM_INTS=3 Z5_LM_INT_AT=30 Z5_LM_INT_EVERY=20",
}
LONG = {"dl1i": "dl1i", "ul1i": "ul1i"}

def plan(name, reps, vs, scens):
    out = []
    for rep in range(1, reps + 1):
        for sc in scens:
            kindx, path = sc.split("@")
            kparts = kindx.split("+")
            kind, extra = kparts[0], " ".join(kparts[1:])
            p = path.split("-")
            dn, rtt = p[0], p[1]
            loss, up, q = "0", dn, "50"
            for x in p[2:]:
                if x.startswith("l"): loss = str(float(x[1:]) / 100)
                if x.startswith("up"): up = x[2:]
                if x.startswith("q"): q = x[1:]
            kenv = KIND[kind]
            if int(rtt) >= 500 and W in kenv:
                kenv = kenv.replace(W, WL)
            k = (rep - 1) % len(vs)
            for v in vs[k:] + vs[:k]:
                b, env = var(v)
                sid = sc.replace("@", "-").replace("+", "_").replace("=", "")
                rid = f"{name}-{sid}-{v}-r{rep}"
                if (b == "core" or b.startswith("lmcore")) and not kind.startswith("lm"):
                    env += f" Z5_WC_TRACE={S}/res/tr/{rid}.jsonl"
                if b in ("base", "n4", "core") and kind.startswith("lm"):
                    b = "lm" + b
                out.append(f"{rid} {b} Z5_SCEN={sc} Z5_VER={v} Z5_REP={rep} {env} "
                           f"Z5_DN_MBIT={dn} Z5_UP_MBIT={up} Z5_RTT_MS={rtt} Z5_LOSS={loss} Z5_QMS={q} Z5_MTU=1500 Z5_TXQ=10000 {kenv} {extra}".replace("  ", " "))
    return out

if __name__ == "__main__":
    name, reps, vs = sys.argv[1], int(sys.argv[2]), sys.argv[3].split(",")
    print("\n".join(plan(name, reps, vs, sys.argv[4:])))
