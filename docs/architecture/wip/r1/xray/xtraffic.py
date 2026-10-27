#!/usr/bin/env python3
"""xtraffic.py: one xray user's traffic, through the client's SOCKS inbound
(R1 round 3, xray: does XHTTP's XMUX put a gamer's flow and a download of the
same user into one TCP connection, and what does the game's latency become).

  xtraffic.py server [--host 127.0.0.1] [--base 7001]
      base+0  TCP game: 64-byte frames, each sent back at once with the
              server's receive time stamped in it (one-way delays, same clock)
      base+1  TCP download: request "D" + total bytes (u64, 0 = none) +
              seconds (f64); 32 KiB writes until either, then close
      base+2  TCP web: requests of 8 bytes (size), each answered with that
              many bytes; the client closes
      base+3  UDP game: as base+0, one datagram per frame
  xtraffic.py client --socks HOST:PORT --target IP [--base 7001]
                     --scenario share|lat [--warm 5 --dur 20 --hz 50]
                     --out OUT.json
      share: the connection-sharing timeline (step 1): two games (TCP and
             UDP), three web bursts, two downloads, a second game later
      lat:   a TCP and a UDP game for warm + dur + 3 s, a download from warm
             for dur s (step 2)
Standard library only. Times: time.time() (one clock: client and server run
on one host, in two network namespaces).
"""
import argparse, json, socket, struct, sys, threading, time

FR = 64                      # game frame
FMT = "!IIddd"               # seq, flow, t_send, t_srv, (spare)
FSZ = struct.calcsize(FMT)   # 32; padded to FR


def recvn(s, n):
    b = bytearray()
    while len(b) < n:
        c = s.recv(n - len(b))
        if not c:
            return None
        b += c
    return bytes(b)


# ---------------------------------------------------------------- server
def srv_game(c):
    try:
        c.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
        while True:
            f = recvn(c, FR)
            if f is None:
                break
            seq, flow, ts, _, sp = struct.unpack(FMT, f[:FSZ])
            c.sendall(struct.pack(FMT, seq, flow, ts, time.time(), sp) + f[FSZ:])
    except OSError:
        pass
    finally:
        c.close()


def srv_dl(c):
    try:
        h = recvn(c, 17)
        if h is None or h[:1] != b"D":
            return
        total, secs = struct.unpack("!Qd", h[1:])
        buf = b"\x5a" * 32768
        sent, t_end = 0, time.time() + (secs if secs > 0 else 1e9)
        while time.time() < t_end and (total == 0 or sent < total):
            n = len(buf) if total == 0 else min(len(buf), total - sent)
            c.sendall(buf[:n])
            sent += n
    except OSError:
        pass
    finally:
        c.close()


def srv_web(c):
    try:
        while True:
            h = recvn(c, 8)
            if h is None:
                break
            (n,) = struct.unpack("!Q", h)
            c.sendall(b"\x33" * n)
    except OSError:
        pass
    finally:
        c.close()


def accept_loop(ls, fn):
    while True:
        c, _ = ls.accept()
        threading.Thread(target=fn, args=(c,), daemon=True).start()


def udp_loop(us):
    while True:
        f, a = us.recvfrom(2048)
        if len(f) >= FSZ:
            seq, flow, ts, _, sp = struct.unpack(FMT, f[:FSZ])
            us.sendto(struct.pack(FMT, seq, flow, ts, time.time(), sp) + f[FSZ:], a)


def server(a):
    for off, fn in ((0, srv_game), (1, srv_dl), (2, srv_web)):
        ls = socket.socket()
        ls.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        ls.bind((a.host, a.base + off))
        ls.listen(256)
        threading.Thread(target=accept_loop, args=(ls, fn), daemon=True).start()
    us = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    us.bind((a.host, a.base + 3))
    threading.Thread(target=udp_loop, args=(us,), daemon=True).start()
    print("xtraffic server on %s:%d-%d" % (a.host, a.base, a.base + 3), flush=True)
    while True:
        time.sleep(3600)


# ---------------------------------------------------------------- client
class Ctx:
    def __init__(self, a):
        self.a = a
        h, p = a.socks.rsplit(":", 1)
        self.socks = (h, int(p))
        self.t0 = time.time()
        self.flows = []
        self.lock = threading.Lock()

    def rel(self, t=None):
        return round((time.time() if t is None else t) - self.t0, 4)

    def add(self, f):
        with self.lock:
            self.flows.append(f)


def socks_connect(ctx, port):
    """TCP through the SOCKS5 inbound to target:port; returns the socket."""
    s = socket.create_connection(ctx.socks, timeout=15)
    s.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
    s.sendall(b"\x05\x01\x00")
    if recvn(s, 2) != b"\x05\x00":
        raise OSError("socks greeting")
    ip = socket.inet_aton(ctx.a.target)
    s.sendall(b"\x05\x01\x00\x01" + ip + struct.pack("!H", port))
    r = recvn(s, 10)
    if r is None or r[1] != 0:
        raise OSError("socks connect %r" % (r,))
    s.settimeout(None)
    return s


def socks_udp(ctx):
    """UDP ASSOCIATE: (control TCP socket, UDP socket, relay address)."""
    s = socket.create_connection(ctx.socks, timeout=15)
    s.sendall(b"\x05\x01\x00")
    if recvn(s, 2) != b"\x05\x00":
        raise OSError("socks greeting")
    s.sendall(b"\x05\x03\x00\x01" + b"\0\0\0\0\0\0")
    r = recvn(s, 10)
    if r is None or r[1] != 0:
        raise OSError("socks udp associate %r" % (r,))
    relay = (socket.inet_ntoa(r[4:8]), struct.unpack("!H", r[8:10])[0])
    if relay[0] == "0.0.0.0":
        relay = (ctx.socks[0], relay[1])
    u = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    u.settimeout(0.5)
    return s, u, relay


def game(ctx, name, t_start, t_end, hz, udp=False):
    """A game: one FR-byte frame each 1/hz s both ways; RTT and one-way delays."""
    f = {"kind": "udpgame" if udp else "game", "name": name, "port": ctx.a.base + (3 if udp else 0),
         "start": None, "end": None, "samples": [], "err": None}
    ctx.add(f)
    time.sleep(max(0, ctx.t0 + t_start - time.time()))
    f["start"] = ctx.rel()
    sent = {}
    done = threading.Event()
    try:
        if udp:
            ctl, s, relay = socks_udp(ctx)
            hdr = b"\0\0\0\x01" + socket.inet_aton(ctx.a.target) + struct.pack("!H", f["port"])
        else:
            s = socks_connect(ctx, f["port"])
    except OSError as e:
        f["err"] = "connect: %s" % e
        return

    def rx():
        while not done.is_set() or sent:
            try:
                if udp:
                    d, _ = s.recvfrom(2048)
                    d = d[10:]
                else:
                    d = recvn(s, FR)
                    if d is None:
                        break
            except socket.timeout:
                if done.is_set():
                    break
                continue
            except OSError:
                break
            tr = time.time()
            seq, _, ts, tsv, _ = struct.unpack(FMT, d[:FSZ])
            sent.pop(seq, None)
            f["samples"].append([round(ts - ctx.t0, 4), round((tr - ts) * 1000, 3),
                                 round((tsv - ts) * 1000, 3), round((tr - tsv) * 1000, 3)])

    th = threading.Thread(target=rx, daemon=True)
    th.start()
    pad = b"\0" * (FR - FSZ)
    seq, nxt = 0, time.time()
    while nxt < ctx.t0 + t_end:
        time.sleep(max(0, nxt - time.time()))
        ts = time.time()
        fr = struct.pack(FMT, seq, 0, ts, 0.0, 0.0) + pad
        sent[seq] = ts
        try:
            if udp:
                s.sendto(hdr + fr, relay)
            else:
                s.sendall(fr)
        except OSError as e:
            f["err"] = "send: %s" % e
            break
        seq += 1
        nxt += 1.0 / hz
    f["sent"] = seq
    # answers to the last frames: up to 2 s
    t_wait = time.time() + 2
    while sent and time.time() < t_wait:
        time.sleep(0.05)
    done.set()
    f["lost"] = len(sent)
    f["lost_t"] = sorted(round(t - ctx.t0, 3) for t in list(sent.values()))
    f["end"] = ctx.rel()
    try:
        if udp:
            ctl.close()
        else:
            s.shutdown(socket.SHUT_WR)
        th.join(1)
        s.close()
    except OSError:
        pass


def download(ctx, name, t_start, secs):
    f = {"kind": "dl", "name": name, "port": ctx.a.base + 1, "start": None, "end": None,
         "ttfb_ms": None, "bytes": 0, "bins": [], "err": None}
    ctx.add(f)
    time.sleep(max(0, ctx.t0 + t_start - time.time()))
    t1 = time.time()
    f["start"] = ctx.rel(t1)
    try:
        s = socks_connect(ctx, f["port"])
        s.sendall(b"D" + struct.pack("!Qd", 0, secs))
        bins = {}
        while True:
            d = s.recv(65536)
            if not d:
                break
            now = time.time()
            if f["ttfb_ms"] is None:
                f["ttfb_ms"] = round((now - t1) * 1000, 1)
            f["bytes"] += len(d)
            k = int((now - ctx.t0) * 10)
            bins[k] = bins.get(k, 0) + len(d)
        s.close()
        f["bins"] = sorted(bins.items())
    except OSError as e:
        f["err"] = str(e)
    f["end"] = ctx.rel()


def web_conn(ctx, f, reqs, size):
    c = {"ttfb_ms": None, "done_ms": None, "err": None}
    t1 = time.time()
    try:
        s = socks_connect(ctx, ctx.a.base + 2)
        for i in range(reqs):
            s.sendall(struct.pack("!Q", size))
            got = 0
            while got < size:
                d = s.recv(65536)
                if not d:
                    raise OSError("closed")
                if c["ttfb_ms"] is None:
                    c["ttfb_ms"] = round((time.time() - t1) * 1000, 1)
                got += len(d)
        c["done_ms"] = round((time.time() - t1) * 1000, 1)
        s.close()
    except OSError as e:
        c["err"] = str(e)
    with ctx.lock:
        f["conns"].append(c)


def web(ctx, name, t_start, conns=6, reqs=3, size=60000):
    """A page load: conns parallel connections, each reqs requests of size bytes."""
    f = {"kind": "web", "name": name, "port": ctx.a.base + 2, "start": None, "end": None, "conns": []}
    ctx.add(f)
    time.sleep(max(0, ctx.t0 + t_start - time.time()))
    f["start"] = ctx.rel()
    ths = [threading.Thread(target=web_conn, args=(ctx, f, reqs, size)) for _ in range(conns)]
    for t in ths:
        t.start()
    for t in ths:
        t.join()
    f["end"] = ctx.rel()


def client(a):
    ctx = Ctx(a)
    jobs = []
    if a.scenario == "share":
        jobs = [(game, ("G1", 0, 45, a.hz)), (game, ("U1", 0, 45, a.hz, True)),
                (web, ("W1", 3)), (download, ("D1", 6, 10)), (web, ("W2", 9)),
                (web, ("W3", 18)), (download, ("D2", 21, 6)), (game, ("G2", 24, 34, a.hz))]
    elif a.scenario == "lat":
        T = a.warm + a.dur + 3
        jobs = [(game, ("G1", 0, T, a.hz)), (game, ("U1", 0, T, a.hz, True)),
                (download, ("D1", a.warm, a.dur))]
    ths = [threading.Thread(target=fn, args=(ctx,) + args) for fn, args in jobs]
    for t in ths:
        t.start()
    for t in ths:
        t.join()
    out = {"scenario": a.scenario, "t0": ctx.t0, "hz": a.hz, "warm": a.warm, "dur": a.dur, "flows": ctx.flows}
    with open(a.out, "w") as fh:
        json.dump(out, fh)


def main():
    p = argparse.ArgumentParser()
    p.add_argument("role", choices=["server", "client"])
    p.add_argument("--host", default="127.0.0.1")
    p.add_argument("--base", type=int, default=7001)
    p.add_argument("--socks", default="127.0.0.1:1080")
    p.add_argument("--target", default="127.0.0.1")
    p.add_argument("--scenario", default="lat")
    p.add_argument("--warm", type=float, default=5)
    p.add_argument("--dur", type=float, default=20)
    p.add_argument("--hz", type=float, default=50)
    p.add_argument("--out", default="/dev/stdout")
    a = p.parse_args()
    server(a) if a.role == "server" else client(a)


if __name__ == "__main__":
    main()
