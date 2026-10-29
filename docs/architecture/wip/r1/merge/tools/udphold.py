#!/usr/bin/env python3
"""udphold.py MOD RES SECONDS: holds UDP on 127.0.0.1 for every port p of the
ephemeral range with p % MOD == RES (Linux gives bind(0) odd ports and
connect() even ones, so a TCP port a test asked for is odd: RES 1 or 3) (as another program's UDP sockets would: xray holds ~270
of them on this host) for SECONDS, to reproduce a user port whose UDP is
taken. Loopback only; the sockets are closed when it exits."""
import socket, sys, time
mod, res, secs = int(sys.argv[1]), int(sys.argv[2]), float(sys.argv[3])
lo, hi = map(int, open("/proc/sys/net/ipv4/ip_local_port_range").read().split())
held = []
for p in range(lo, hi + 1):
    if p % mod != res:
        continue
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.bind(("127.0.0.1", p))
        held.append(s)
    except OSError:
        s.close()
print("holding %d UDP ports on 127.0.0.1 (p %% %d == %d in %d-%d) for %.0f s" % (len(held), mod, res, lo, hi, secs), flush=True)
time.sleep(secs)
