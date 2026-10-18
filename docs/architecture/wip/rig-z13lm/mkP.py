# mkP.py NAME REPS VARIANTS SCENS...: a z12 pilot plan, as z11's mkB.py: interleaved, the
# variants' order rotated per repetition; a core run gets its own window core trace file.
# Every run has Z5_TXQ=10000 (the TUN device's 500-packet tx queue drops under BBR's pacing).
# scen syntax: kind@dn-rtt[-l<loss%>][-up<mbit>][-q<ms>]
# variants: base, n4, core0..core9, and pacing-cap ones: basepK / corepK (core binary K;
#   basep: the core binary with the core off, windows as base, the sender's pacing capped)
import sys, re
S = "/tmp/claude-0/-home-user-hs2-/48e4aec7-ad72-5cc1-b09e-d3366fa010b6/scratchpad/z13"
def var(v):
    if v == "base":
        return "base", ""
    if v == "n4":
        return "n4", "Z5_POOL=128"
    m = re.fullmatch(r"core(\d)", v)
    if m:
        return v, "Z5_POOL=128"
    m = re.fullmatch(r"coreb(\d)", v)
    if m:
        return "core" + m.group(1), "Z5_POOL=128 Z5_WC_BURST=0.55"
    m = re.fullmatch(r"basep(\d)", v)
    if m:
        return "core" + m.group(1), "Z5_POOL=128 Z5_WCORE=0 Z5_WC_PACECAP=1.25"
    m = re.fullmatch(r"off(\d)", v)
    if m:
        return "core" + m.group(1), "Z5_POOL=128 Z5_WCORE=0"
    m = re.fullmatch(r"corep(\d)", v)
    if m:
        return "core" + m.group(1), "Z5_POOL=128 Z5_WC_PACECAP=1.25"
    raise SystemExit("unknown variant " + v)
COMMON = "Z5_WARM_S=6 Z5_DUR_S=24 Z5_TRACE=1 Z5_TRACE2=1"
KIND = {
 "dl1i": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 " + COMMON,
 "ul1i": "Z5_RELAY=1 Z5_UPL=1 Z5_INT=1 " + COMMON,
 "dl4": "Z5_RELAY=1 Z5_FAST=4 Z5_INT=1 " + COMMON,
 "dlchg": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=36 Z5_CHG_MBIT=50 Z5_CHG_AT=18 Z5_CHG_S=12 Z5_TRACE=1 Z5_TRACE2=1",
 "lm1": "Z5_TEST=TestZ5LM Z5_LM_DUR_S=130",
 "lm1u": "Z5_TEST=TestZ5LM Z5_LM_UP=1 Z5_LM_DUR_S=130",
 # multi-link: one big download from the start, Z5_LM_INTS interactive users arriving
 # every Z5_LM_INT_EVERY s (each 200 B request / 2 KiB reply every 100 ms)
 "lmi": "Z5_TEST=TestZ5LM Z5_LM_DUR_S=90 Z5_LM_INTS=4 Z5_LM_INT_AT=15 Z5_LM_INT_EVERY=15",
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
            b, env = var(v)
            sid = sc.replace("@", "-")
            rid = f"{name}-{sid}-{v}-r{rep}"
            if b.startswith("core") and "Z5_WCORE=0" not in env:
                env += f" Z5_WC_TRACE={S}/res/tr/{rid}.jsonl"
            print(f"{rid} {b} Z5_SCEN={sc} Z5_VER={v} Z5_REP={rep} {env} "
                  f"Z5_DN_MBIT={dn} Z5_UP_MBIT={up} Z5_RTT_MS={rtt} Z5_LOSS={loss} Z5_QMS={q} Z5_MTU=1500 Z5_TXQ=10000 {KIND[kind]}")
