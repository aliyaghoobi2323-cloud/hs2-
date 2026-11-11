# s49 harness (rel49, docs/reality-architecture.md §11.6.6)

s46 (`../s46/README.md`) moved to /opt/hs2lab/s49. `setup.sh HS2_SRC_TREE`
(run on the lab host after copying this directory to /opt/hs2lab/s49) builds
`labsrc` from the lab's smuxwin labsrc2 run.sh patched with `patchrun.py`
(from s45, with `tcprelay.py`) plus go.mod/go.sum/third_party of the tree.

Arms (`run1.sh NAME FARM V BIN`): `r45` (rel45 binary hs2-r45), `r49`
(hs2-r49 with `HS2_MCONN_STRIPE_RESEND=1` on BOTH sides: the resend and the
last-sub grace), `r49l` (r49 + `HS2_MCONN_STRIPE_LOG=1` both sides).
`drv.sh JOBS`: 3 slots. `hs.py res` per-second heavy series, `sum.py res`
groups by variant/arm; `ttr.py res` time-to-resume per flow after the kill.
