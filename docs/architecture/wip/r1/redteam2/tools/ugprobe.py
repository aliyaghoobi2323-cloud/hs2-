#!/usr/bin/env python3
# ugprobe.py — the R1 red team's (second pass) user through the tunnel during
# an upgrade (upgrade.sh): every 100 ms a new TCP connection to the edge's
# user port 8443, 16 bytes written and echoed back within 1 s; one line per
# try: "<epoch> ok" or "<epoch> fail <why>". Runs until killed.
import socket, sys, time, os

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8443
msg = os.urandom(16)
nxt = time.time()
while True:
    t = time.time()
    why = "ok"
    try:
        s = socket.create_connection(("127.0.0.1", PORT), timeout=1.0)
        s.settimeout(1.0)
        s.sendall(msg)
        got = b""
        while len(got) < len(msg):
            b = s.recv(64)
            if not b:
                break
            got += b
        s.close()
        if got != msg:
            why = "fail short"
    except Exception as e:
        why = "fail " + type(e).__name__
    print("%.3f %s" % (t, why), flush=True)
    nxt += 0.1
    d = nxt - time.time()
    if d > 0:
        time.sleep(d)
    else:
        nxt = time.time()
