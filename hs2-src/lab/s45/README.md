# s45 harness (rel45, docs/reality-architecture.md §11.5)

Lab 1 copy at /opt/hs2lab/s45. `run1.sh NAME ARM VARIANT` under
/opt/hs2lab/labslot.sh: reverse l3mtcp, 3 lb paths, 150 links, crowd + call +
game + heavy (c1: 1 % loss, 8 down; u8: 8 up, 75 % of the links killed at
12 s). Arms (FARM): tls, reality (dialer pinned), r70 (reality, target in its
own netns behind a 35+35 ms netem veth, concurrent `tgt.py`), rpy (reality,
`tgt.py` on loopback), an / anb (tls -> both at 8 s -> reality at 14 s by
SIGHUP, dialer not pinned; anb = HS2BIN hs2-s45b, the fixed build; an.txt
gets an `openssl s_client` probe with our tls name 1 s and 32 s after the
switch).

`patchrun.py` is the change to the lab's run.sh (smuxwin labsrc2 copy):
HEAVY_CTL=direct (u8: iperf3 control over the bare path via `tcprelay.py`,
data streams `--cport 41000+i` DNATed to 8444), mutex/block profiles next to
the CPU profile when PPROF=1 (`HS2_PPROF_LOCKS`). run.sh also copies
heavy.json to LOGDIR.

Analysis: `an.py res/` (medians), `mw.py res/ reality tls` (means, sd,
Mann-Whitney U with a 20000-permutation p), `an2.py res/` (switch/setup).
