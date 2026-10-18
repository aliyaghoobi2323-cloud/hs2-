# mkplan3.py (mkplan2 + the full-matrix kinds; a variant "v:suffix" runs the binary v+suffix)
# mkplan2.py NAME REPS VARIANTS SCENS... -> plan lines "<id> <bin> ENV..." (interleaved:
# each rep runs every scenario across all variants, the variants' order rotated by rep)
# scen syntax: kind[+ENV=V...]@dn-rtt[-l<loss%>][-up<mbit>][-q<ms>]
# variant syntax: name or name+ENV=V+ENV=V (extra env on a VAR entry)
import sys
VAR = {
 "base": ("base", ""),
 "n4":   ("n4", "Z5_POOL=128"),
 "dly":  ("dly", "Z5_POOL=128"),
 "fin":  ("fin", "Z5_POOL=128"),
 "dly2": ("dly2", "Z5_POOL=128"),
 "d2rcv": ("dly2", "Z5_POOL=128 Z5_QSIG=rcv"),
 "d2srtt": ("dly2", "Z5_POOL=128 Z5_QSIG=srtt"),
 "d2ctrl": ("dly2", "Z5_POOL=128 Z5_QSIG=ctrl"),
 "d2off": ("dly2", "Z5_POOL=128 Z5_QSIG=off"),
 "d3": ("dly3", "Z5_POOL=128"),
 "s10": ("dly3", "Z5_POOL=128 Z5_QSIG=srtt Z5_MMAX=1 Z5_QLO=0.03125 Z5_QHI=0.0625"),
 "s05": ("dly3", "Z5_POOL=128 Z5_QSIG=srtt Z5_MMAX=0.5 Z5_QLO=0.03125 Z5_QHI=0.0625"),
 "r05": ("dly3", "Z5_POOL=128 Z5_QSIG=rcv Z5_MMAX=0.5"),
 "ps05": ("dly4", "Z5_POOL=128 Z5_QSIG=srtt Z5_MMAX=0.5 Z5_QLO=0.03125 Z5_QHI=0.0625"),
 "ps10": ("dly4", "Z5_POOL=128 Z5_QSIG=srtt Z5_MMAX=1 Z5_QLO=0.03125 Z5_QHI=0.0625"),
 "pr05": ("dly4", "Z5_POOL=128 Z5_QSIG=rcv Z5_MMAX=0.5"),
 "poff": ("dly4", "Z5_POOL=128 Z5_QSIG=off"),
 "nps05": ("dly4", "Z5_POOL=128 Z5_QSIG=srtt Z5_MMAX=0.5 Z5_QLO=0.03125 Z5_QHI=0.0625 Z5_PAUSE=0"),
 "q25": ("dly5", "Z5_POOL=128 Z5_QSIG=off Z5_MMAX=0.25 Z5_QLO=0.0625 Z5_QHI=0.125"),
 "q50": ("dly5", "Z5_POOL=128 Z5_QSIG=off Z5_MMAX=0.5 Z5_QLO=0.0625 Z5_QHI=0.125"),
 "qs50": ("dly5", "Z5_POOL=128 Z5_QSIG=srtt Z5_MMAX=0.5 Z5_QLO=0.0625 Z5_QHI=0.125"),
 "q50np": ("dly5", "Z5_POOL=128 Z5_QSIG=off Z5_MMAX=0.5 Z5_QLO=0.0625 Z5_QHI=0.125 Z5_PAUSE=0"),
 "st10": ("dly6", "Z5_POOL=128 Z5_QSIG=off Z5_QC=0 Z5_PAUSE=0"),
 "g25": ("dly6", "Z5_POOL=128 Z5_QSIG=off Z5_MMAX=0.25 Z5_MMIN=0.05 Z5_QLO=0.0625 Z5_QHI=0.125 Z5_PAUSE=0"),
 "g25s": ("dly6", "Z5_POOL=128 Z5_QSIG=srtt Z5_MMAX=0.25 Z5_MMIN=0.05 Z5_QLO=0.0625 Z5_QHI=0.125 Z5_PAUSE=0"),
 "g50s": ("dly6", "Z5_POOL=128 Z5_QSIG=srtt Z5_MMAX=0.5 Z5_MMIN=0.05 Z5_QLO=0.0625 Z5_QHI=0.125 Z5_PAUSE=0"),
 "s8st": ("dly8", "Z5_POOL=128 Z5_QSIG=off Z5_QC=0 Z5_PAUSE=0"),
 "s8s4": ("dly8", "Z5_POOL=128 Z5_QSIG=off Z5_QC=0 Z5_PAUSE=0 Z5_STEPDIV=4"),
 "s8s4p": ("dly8", "Z5_POOL=128 Z5_QSIG=off Z5_QC=0 Z5_STEPDIV=4 Z5_PGAP=2 Z5_PSHARE=2"),
 "s8p": ("dly8", "Z5_POOL=128 Z5_QSIG=off Z5_QC=0 Z5_PGAP=2 Z5_PSHARE=2"),
 "s9st": ("dly9", "Z5_POOL=128 Z5_QSIG=off Z5_QC=0 Z5_PAUSE=0"),
 "s9o4": ("dly9", "Z5_POOL=128 Z5_QSIG=off Z5_QC=0 Z5_PAUSE=0 Z5_STEPDIV=4 Z5_STEPOLD=1"),
 "s9o2": ("dly9", "Z5_POOL=128 Z5_QSIG=off Z5_QC=0 Z5_PAUSE=0 Z5_STEPOLD=1"),
 "sgA": ("dly9", "Z5_POOL=128 Z5_QSIG=srtt Z5_QC=0 Z5_PAUSE=0 Z5_STEPOLD=1 Z5_MMAX=0.1 Z5_MMIN=-1 Z5_QOVER=0 Z5_QLO=0.03125 Z5_QHI=0.0625"),
 "sgB": ("dly9", "Z5_POOL=128 Z5_QSIG=srtt Z5_QC=0 Z5_PAUSE=0 Z5_STEPOLD=1 Z5_STEPDIV=4 Z5_MMAX=0.1 Z5_MMIN=-1 Z5_QOVER=0 Z5_QLO=0.03125 Z5_QHI=0.0625"),
 "c05": ("dly3", "Z5_POOL=128 Z5_QSIG=ctrl Z5_MMAX=0.5 Z5_QLO=0.03125 Z5_QHI=0.0625"),
}
KIND = {
 "dl1": "Z5_FAST=1 Z5_WARM_S=6 Z5_DUR_S=14",
 "ul1": "Z5_UPL=1 Z5_WARM_S=6 Z5_DUR_S=14",
 "dl1i": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=24",
 "ul1i": "Z5_RELAY=1 Z5_UPL=1 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=24",
 "dlul": "Z5_RELAY=1 Z5_FAST=1 Z5_UPL=1 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=24",
 "dl4": "Z5_RELAY=1 Z5_FAST=4 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=24",
 "fts12": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FTS=12 Z5_FTS_S=3 Z5_FTS_KBS=32 Z5_F_AT=0 Z5_BG_AT=2 Z5_WARM_S=4 Z5_DUR_S=20",
 "slow16": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_SLOW=16 Z5_SLOW_MBIT=2 Z5_WARM_S=8 Z5_DUR_S=15",
 "frz32": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=32 Z5_FREEZE_KB=16 Z5_WARM_S=8 Z5_DUR_S=15",
 "seqf6": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=6 Z5_FREEZE_KB=65536 Z5_FREEZE_SEQ_S=1 Z5_WARM_S=4 Z5_DUR_S=30",
 "seqf12": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=12 Z5_FREEZE_KB=32768 Z5_FREEZE_SEQ_S=1 Z5_WARM_S=4 Z5_DUR_S=36",
 "lm1": "Z5_TEST=TestZ5LM Z5_LM_DUR_S=130",
 "dl8": "Z5_RELAY=1 Z5_FAST=8 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=24",
 "dl16": "Z5_RELAY=1 Z5_FAST=16 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=24",
 "dlchg": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=36 Z5_CHG_MBIT=50 Z5_CHG_AT=18 Z5_CHG_S=12",
 "ulchg": "Z5_RELAY=1 Z5_UPL=1 Z5_INT=1 Z5_WARM_S=6 Z5_DUR_S=36 Z5_CHG_MBIT=50 Z5_CHG_AT=18 Z5_CHG_S=12",
 "slow2": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_SLOW=2 Z5_SLOW_MBIT=2 Z5_WARM_S=8 Z5_DUR_S=15",
 "slow4": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_SLOW=4 Z5_SLOW_MBIT=2 Z5_WARM_S=8 Z5_DUR_S=15",
 "slow8": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_SLOW=8 Z5_SLOW_MBIT=2 Z5_WARM_S=8 Z5_DUR_S=15",
 "ftf4": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=4 Z5_FREEZE_KB=32768 Z5_WARM_S=4 Z5_DUR_S=20",
 "z3f8s": "Z5_RELAY=1 Z5_FAST=1 Z5_INT=1 Z5_FROZEN=3 Z5_FREEZE_KB=8192 Z5_SLOW=8 Z5_SLOW_MBIT=8 Z5_WARM_S=8 Z5_DUR_S=15",
 "lm1u": "Z5_TEST=TestZ5LM Z5_LM_UP=1 Z5_LM_DUR_S=130",
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
        for vx in vs[k:] + vs[:k]:
            vp = vx.split("+")
            vn, _, suf = vp[0].partition(":")
            b, env = VAR[vn]
            b += suf
            vp[0] = vn
            vx = "+".join(vp)
            env = (env + " " + " ".join(vp[1:])).strip()
            v = vx.replace("+", "_").replace("=", "")
            sid = sc.replace("@", "-").replace("+", "_").replace("=", "")
            print(f"{name}-{sid}-{v}-r{rep} {b} Z5_SCEN={sc} Z5_VER={v} Z5_REP={rep} {env} "
                  f"Z5_DN_MBIT={dn} Z5_UP_MBIT={up} Z5_RTT_MS={rtt} Z5_LOSS={loss} Z5_QMS={q} Z5_MTU=1500 {KIND[kind]} {extra}")
