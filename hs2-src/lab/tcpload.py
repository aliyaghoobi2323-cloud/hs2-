#!/usr/bin/env python3
import socket, sys, threading, time, json
mode = sys.argv[1]
if mode == "server":
    host, port, rate = sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
    s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind((host, port)); s.listen(512)
    def serve(c):
        buf = b"x" * 4096; step = 0.02; per = max(1, int(rate * step))
        try:
            nxt = time.time()
            while True:
                n = per
                while n > 0:
                    k = c.send(buf[:min(n, len(buf))]); n -= k
                nxt += step; d = nxt - time.time()
                if d > 0: time.sleep(d)
                else: nxt = time.time()
        except Exception:
            pass
    while True:
        c, _ = s.accept(); threading.Thread(target=serve, args=(c,), daemon=True).start()
else:
    host, port, n, secs, out = sys.argv[2], int(sys.argv[3]), int(sys.argv[4]), float(sys.argv[5]), sys.argv[6]
    t0 = time.time(); tot = [0]*n; late=[0]*n; maxgap=[0.0]*n
    def run(i):
        try:
            c = socket.create_connection((host, port), timeout=10); c.settimeout(1.0)
        except Exception as e:
            tot[i] = -1; return
        last=time.time()
        while time.time() - t0 < secs:
            try:
                b = c.recv(65536)
                if not b: break
                now=time.time(); maxgap[i]=max(maxgap[i],now-last); last=now
                tot[i] += len(b)
                if now - t0 >= 10: late[i] += len(b)
            except socket.timeout:
                continue
            except Exception:
                break
    ts = [threading.Thread(target=run, args=(i,), daemon=True) for i in range(n)]
    for t in ts: t.start()
    for t in ts: t.join()
    json.dump({"tot":tot,"late":late,"maxgap":maxgap,"secs":secs}, open(out, "w"))
