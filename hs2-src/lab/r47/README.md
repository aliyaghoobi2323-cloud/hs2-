# r47 harness (rel47, docs/reality-architecture.md §11.6)

Lab 1 copy at /opt/hs2lab/r47. Mixed per-IP fronts with LB: reverse l3mtcp,
3 lb paths, 150 links, 74 ms RTT, 1 % loss, crowd 3000 + call + game + iperf
down x8. `run1.sh NAME "EVENTS"` under /opt/hs2lab/labslot.sh (or `go47.sh`
with PLAN="scenario:arm:i ..." PLANTAG=q).

Fronts (ir.sh): A 192.168.50.1 tls (primary), B 51.1 reality with its own
target (`tgt.py` in netns tgt$LAB_ID behind a veth, breakable), C 52.1 both
(target `openssl s_server` on loopback). PLAIN=1: all tls. BTLS=1: A pure
reality, B tls. The kharej has no front config (announcement only) and starts
cold every run (kh.sh removes the remembered fronts; KEEPPF=1 keeps them).

Events (ir.sh): `bbad` (path B +80 ms, 3 % corrupt: §8.13 scenario c),
`tblk` / `trej` / `tslow` / `tok` (B's target blackholed / RST / +2.5 s /
healthy), `a2both` / `a2real` / `a2tls` (live front of A by SIGHUP).
KILL_AT=12 kills 75 % of the links 12 s into the probe. XENV adds env keys to
both sides (e.g. HS2_MCONN_TRACE=1). HS2BIN picks the binary.

Analysis: `an47.py res/` (per run: crowd/call/game/ping p99, Mbps, failures,
links per path at the end, LB cools/backs, proof timeouts, listener front
counters and reality rejects; per arm: medians [min-max]).
