# mkplan.py NAME REPS VARIANTS SCENS... -> plan lines "<id> <bin> ENV..." (interleaved: each rep runs
# every scenario across all variants, the variants' order rotated by rep)
# scen syntax: kind[+ENV=V...]@dn-rtt[-l<loss%>][-up<mbit>][-q<ms>]
import sys

VAR = {
 "base": ("base", ""),
 "n4":   ("n4", "Z5_POOL=128"),
 "p4":   ("p4", "Z5_POOL=128"),
 "p5":   ("p5", "Z5_POOL=128"),
 "p6":   ("p6", "Z5_POOL=128"),
 "p7":   ("p7", "Z5_POOL=128"),
 "p8":   ("p8", "Z5_POOL=128"),
 "pf":   ("pf", "Z5_POOL=128"),
 "pg":   ("pg", "Z5_POOL=128"),
 "ph":   ("ph", "Z5_POOL=128"),
}
KIND = {
 "dl1i": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_TRACE=1 Z5_WARM_S=6 Z5_DUR_S=20",
 "ul1i": "Z5_RELAY=1 Z5_UPL=1 Z5_INT=1 Z5_TRACE=1 Z5_WARM_S=6 Z5_DUR_S=20",
 "dl1iL": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_TRACE=1 Z5_WARM_S=10 Z5_DUR_S=30",
 "ul1iL": "Z5_RELAY=1 Z5_UPL=1 Z5_INT=1 Z5_TRACE=1 Z5_WARM_S=10 Z5_DUR_S=30",
 "dl8i": "Z5_RELAY=1 Z5_FAST=8 Z5_INT=1 Z5_TRACE=1 Z5_WARM_S=6 Z5_DUR_S=20",
 "dl16i": "Z5_RELAY=1 Z5_FAST=16 Z5_INT=1 Z5_TRACE=1 Z5_WARM_S=6 Z5_DUR_S=20",
 "dlsc": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_TRACE=1 Z5_WARM_S=6 Z5_DUR_S=54 Z5_DN_SCHED=24:50,39:300",
 "ulsc": "Z5_RELAY=1 Z5_UPL=1 Z5_INT=1 Z5_TRACE=1 Z5_WARM_S=6 Z5_DUR_S=54 Z5_UP_SCHED=24:50,39:300",
 "dl4": "Z5_RELAY=1 Z5_FAST=4 Z5_INT=1 Z5_TRACE=1 Z5_WARM_S=6 Z5_DUR_S=20",
 "dlul1": "Z5_RELAY=1 Z5_FAST=1 Z5_UPL=1 Z5_INT=1 Z5_TRACE=1 Z5_WARM_S=6 Z5_DUR_S=20",
 "dl1": "Z5_FAST=1 Z5_WARM_S=6 Z5_DUR_S=10",
 "ul1": "Z5_UPL=1 Z5_WARM_S=6 Z5_DUR_S=10",
 "dlul": "Z5_FAST=1 Z5_UPL=1 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=12",
 "dl8": "Z5_FAST=8 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=12",
 "dl16": "Z5_FAST=16 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=12",
 "mix8": "Z5_FAST=4 Z5_UPL=4 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=12",
 "idb1": "Z5_RELAY=1 Z5_FAST=0 Z5_INT=1 Z5_IDB=1 Z5_IDB_MB=96 Z5_IDB_GAP_S=14 Z5_IDB_KBS=32 Z5_BG_AT=1 Z5_WARM_S=4 Z5_DUR_S=26",
 "idb1s": "Z5_RELAY=1 Z5_FAST=0 Z5_INT=1 Z5_IDB=1 Z5_IDB_MB=96 Z5_IDB_GAP_S=3 Z5_IDB_KBS=32 Z5_BG_AT=1 Z5_WARM_S=4 Z5_DUR_S=22",
 "idb1f": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_IDB=1 Z5_IDB_MB=96 Z5_IDB_GAP_S=14 Z5_IDB_KBS=32 Z5_BG_AT=1 Z5_WARM_S=4 Z5_DUR_S=26",
 "fts12nf": "Z5_RELAY=1 Z5_FAST=0 Z5_INT=1 Z5_FTS=12 Z5_FTS_S=3 Z5_FTS_KBS=32 Z5_F_AT=0 Z5_BG_AT=2 Z5_WARM_S=4 Z5_DUR_S=20",
 "fts12": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FTS=12 Z5_FTS_S=3 Z5_FTS_KBS=32 Z5_F_AT=0 Z5_BG_AT=2 Z5_WARM_S=4 Z5_DUR_S=20",
 "slow2": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_SLOW=2 Z5_SLOW_MBIT=2 Z5_WARM_S=8 Z5_DUR_S=15",
 "slow4": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_SLOW=4 Z5_SLOW_MBIT=2 Z5_WARM_S=8 Z5_DUR_S=15",
 "slow8": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_SLOW=8 Z5_SLOW_MBIT=2 Z5_WARM_S=8 Z5_DUR_S=15",
 "slow16": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_SLOW=16 Z5_SLOW_MBIT=2 Z5_WARM_S=8 Z5_DUR_S=15",
 "frz32": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=32 Z5_FREEZE_KB=16 Z5_WARM_S=8 Z5_DUR_S=15",
 "ftf4": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=4 Z5_FREEZE_KB=32768 Z5_WARM_S=4 Z5_DUR_S=20",
 "ftf8": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=8 Z5_FREEZE_KB=32768 Z5_WARM_S=4 Z5_DUR_S=20",
 "z3f8s": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=3 Z5_FREEZE_KB=8192 Z5_SLOW=8 Z5_SLOW_MBIT=8 Z5_WARM_S=8 Z5_DUR_S=15",
 "seqf6": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=6 Z5_FREEZE_KB=65536 Z5_FREEZE_SEQ_S=1 Z5_WARM_S=4 Z5_DUR_S=30",
 "seqf12": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=12 Z5_FREEZE_KB=32768 Z5_FREEZE_SEQ_S=1 Z5_WARM_S=4 Z5_DUR_S=36",
}
name, reps, vs = sys.argv[1], int(sys.argv[2]), sys.argv[3].split(",")
for rep in range(1, reps + 1):
    for sc in sys.argv[4:]:
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
        k = (rep - 1) % len(vs)
        for v in vs[k:] + vs[:k]:
            b, env = VAR[v]
            import os
            if os.environ.get("BINSFX"): b = b + os.environ["BINSFX"]
            sid = sc.replace("@", "-").replace("+", "_").replace("=", "")
            print(f"{name}-{sid}-{v}-r{rep} {b} Z5_SCEN={sc} Z5_VER={v} Z5_REP={rep} {env} "
                  f"Z5_DN_MBIT={dn} Z5_UP_MBIT={up} Z5_RTT_MS={rtt} Z5_LOSS={loss} Z5_QMS={q} Z5_MTU=1500 {KIND[kind]} {extra}")
