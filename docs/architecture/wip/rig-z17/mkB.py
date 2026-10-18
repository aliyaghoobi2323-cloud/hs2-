# mkB.py NAME REPS VARIANTS SCENS...: Part B plan (base vs core), as mkplan3.py: interleaved, the
# variants' order rotated per repetition; a core run gets its own window core trace file.
# scen syntax: kind[+ENV=V...]@dn-rtt[-l<loss%>][-up<mbit>][-q<ms>]
import sys
S = "/tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z12"
VAR = {
 "base": ("base2", ""),
 "core": ("core2", "Z5_POOL=128"),
  "off":  ("core2", "Z5_POOL=128 Z5_WCORE=0"),
}
KIND = {
 "dl1i": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=24 Z5_TRACE=1 Z5_TRACE2=1",
 "ul1i": "Z5_RELAY=1 Z5_UPL=1 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=24 Z5_TRACE=1 Z5_TRACE2=1",
 "dl4": "Z5_RELAY=1 Z5_FAST=4 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=24 Z5_TRACE=1 Z5_TRACE2=1",
 "dlchg": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=36 Z5_CHG_MBIT=50 Z5_CHG_AT=18 Z5_CHG_S=12 Z5_TRACE=1 Z5_TRACE2=1",
 "lm1": "Z5_TEST=TestZ5LM Z5_LM_DUR_S=130",
 "lm1u": "Z5_TEST=TestZ5LM Z5_LM_UP=1 Z5_LM_DUR_S=130",
}
name, reps, vs = sys.argv[1], int(sys.argv[2]), sys.argv[3].split(",")
for rep in range(1, reps + 1):
    for sc in sys.argv[4:]:
        kind, path = sc.split("@")
        p = path.split("-")
        dn, rtt = p[0], p[1]
        loss, up, q = "0", dn, "50"
        for x in p[2:]:
            if x.startswith("l"): loss = str(float(x[1:]) / 100)
            if x.startswith("up"): up = x[2:]
            if x.startswith("q"): q = x[1:]
        k = (rep - 1) % len(vs)
        for v in vs[k:] + vs[:k]:
            b, env = VAR[v]
            sid = sc.replace("@", "-")
            rid = f"{name}-{sid}-{v}-r{rep}"
            if b.startswith("core"):
                env += f" Z5_WC_TRACE={S}/res/tr/{rid}.jsonl"
            print(f"{rid} {b} Z5_SCEN={sc} Z5_VER={v} Z5_REP={rep} {env} "
                  f"Z5_DN_MBIT={dn} Z5_UP_MBIT={up} Z5_RTT_MS={rtt} Z5_LOSS={loss} Z5_QMS={q} Z5_MTU=1500 {KIND[kind]}")
