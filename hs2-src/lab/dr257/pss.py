#!/usr/bin/env python3
# dr259: per-path tunnel-link sampler (kh side = download direction, ir side = upload direction).
# Every 1 s: for each tunnel link (peer 192.168.5X.1 / 50.2), print "ts side path rtt acked_bytes lastsnd_ms".
# Light-link rtt per path is the proxy for light-user latency on that path; acked deltas show where bytes go.
import re, subprocess, sys, time
ns, out = sys.argv[1], sys.argv[2]
f = open(out, "a", buffering=1)
R = re.compile(r"(\S+):(\d+)\s+(\S+):(\d+)\s*\n\s+(.*)")
while True:
    t = time.time()
    for side in ("kh", "ir"):
        try:
            s = subprocess.run(["ip", "netns", "exec", side + ns, "ss", "-tinH", "state", "established",
                                "( dst 192.168.0.0/16 )"], capture_output=True, text=True, timeout=5).stdout
        except Exception:
            continue
        for m in R.finditer(s):
            loc, rem, info = m.group(1), m.group(3), m.group(5)
            path = (rem if side == "kh" else loc).split(".")[2]
            rt = re.search(r" rtt:([\d.]+)", info); ak = re.search(r"bytes_acked:(\d+)", info)
            ls = re.search(r"lastsnd:(\d+)", info)
            if not rt: continue
            f.write("%.1f %s %s %s:%s %s %s %s\n" % (t, side, path, loc, m.group(2), rt.group(1),
                                                ak.group(1) if ak else 0, ls.group(1) if ls else 99999))
    time.sleep(max(0, 1 - (time.time() - t)))
