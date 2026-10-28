#!/usr/bin/env python3
"""fillsock.py MB SECONDS: hold about MB of TCP buffers on loopback (a
connection whose reader never reads) for SECONDS, then exit; exits 1 on
SIGTERM. The memory guard's own test (rigguard.sh)."""
import signal
import socket
import sys
import time

signal.signal(signal.SIGTERM, lambda *a: sys.exit(1))
mb, secs = int(sys.argv[1]), float(sys.argv[2])
ls = socket.socket()
ls.bind(("127.0.0.1", 0))
ls.listen(1)
c = socket.socket()
c.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, mb << 20)
c.connect(ls.getsockname())
s, _ = ls.accept()
s.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, mb << 20)
c.setblocking(False)
buf = b"x" * 65536
sent = 0
end = time.time() + secs
while time.time() < end:
    try:
        sent += c.send(buf)
    except BlockingIOError:
        time.sleep(0.05)
print("sent", sent)
