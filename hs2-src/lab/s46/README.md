# s46 harness (rel46, docs/reality-architecture.md §11.6)

Copy of s45 at /opt/hs2lab/s46 on the lab hosts; `labsrc` = the lab's run.sh
(smuxwin labsrc2) patched with `../s45/patchrun.py`, plus `go.mod`, `go.sum`
and `third_party/` of the tree (run.sh builds lab/netem and lab/probe).

`run1.sh NAME FARM VARIANT BIN [EXTRA_IR_ENV]`: binary `hs2-BIN`
(r31, r40, r45, r46...); `BIN=r31p` runs hs2-r31 with the production env only
(Iran: nothing but the defaults; kharej: `HS2_LB_FLAP_PEN=1`); `XKH=...` in
the environment adds kharej env. Variants c0, c1, s (crowd/call/game, no
heavy), u8 (8 uploads, 75 % of the links killed at 12 s).
`run2.sh` = run1.sh with `irF2.sh`/`khF2.sh`: arm `r128` (reality, the
openssl target limited to TLS_AES_128_GCM_SHA256) and a per-second `ss -ti`
summary of the link sockets on kharej (`kh-ss.txt`: n, sum cwnd, mean rtt,
sum retrans, sum delivery rate Mbit/s).
`drv.sh JOBS` / `drv2.sh JOBS`: 3 slots, lines "NAME FARM V BIN [IR env]".
`hs.py res` prints each run's per-second heavy series; `sum.py res` groups by
variant and arm (frozen = the upload's last 5 s all 0).
