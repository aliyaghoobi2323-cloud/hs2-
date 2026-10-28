#!/usr/bin/env python3
# udpt.py: the small TCP/UDP helpers of the r1-udp reproduction (standard
# library only). Every address is HOST:PORT ([v6]:PORT for IPv6).
#
#   udpt.py echo ADDR [--no-udp] [--no-tcp]   echo server: TCP and UDP on ADDR
#   udpt.py tcpcheck ADDR [--src IP] [--n N]   N round trips of 1 KiB over one connection
#   udpt.py udpcheck ADDR [--src IP] [--n N] [--connected]
#                                              N datagrams, one at a time, each waits
#                                              up to 2 s for its echo; --connected: a
#                                              connected socket (as game clients and
#                                              xray use), which takes replies only from
#                                              ADDR itself
#   udpt.py hold ADDR                          bind UDP on ADDR and sleep (a port taken by
#                                              another program)
# tcpcheck/udpcheck print one JSON object: {"ok": k, "n": N, "err": "..."}.
import json
import os
import socket
import sys
import threading
import time


def split(addr):
    host, _, port = addr.rpartition(":")
    host = host.strip("[]")
    fam = socket.AF_INET6 if ":" in host else socket.AF_INET
    return fam, host, int(port)


def arg(name, default=None):
    if name in sys.argv:
        return sys.argv[sys.argv.index(name) + 1]
    return default


def echo(addr):
    fam, host, port = split(addr)
    ths = []
    if "--no-tcp" not in sys.argv:
        ls = socket.socket(fam, socket.SOCK_STREAM)
        ls.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        ls.bind((host, port))
        ls.listen(256)

        def serve(c):
            try:
                while True:
                    b = c.recv(65536)
                    if not b:
                        break
                    c.sendall(b)
            except OSError:
                pass
            c.close()

        def acc():
            while True:
                c, _ = ls.accept()
                threading.Thread(target=serve, args=(c,), daemon=True).start()

        ths.append(threading.Thread(target=acc, daemon=True))
    if "--no-udp" not in sys.argv:
        us = socket.socket(fam, socket.SOCK_DGRAM)
        us.bind((host, port))

        def uecho():
            while True:
                b, a = us.recvfrom(65536)
                us.sendto(b, a)

        ths.append(threading.Thread(target=uecho, daemon=True))
    for t in ths:
        t.start()
    print("echo on %s (tcp=%s udp=%s)" % (addr, "--no-tcp" not in sys.argv, "--no-udp" not in sys.argv), flush=True)
    while True:
        time.sleep(3600)


def tcpcheck(addr):
    fam, host, port = split(addr)
    n = int(arg("--n", "5"))
    src = arg("--src")
    ok, err = 0, ""
    try:
        c = socket.socket(fam, socket.SOCK_STREAM)
        c.settimeout(5)
        if src:
            c.bind((src, 0))
        c.connect((host, port))
        for i in range(n):
            msg = os.urandom(1024)
            c.sendall(msg)
            got = b""
            while len(got) < len(msg):
                b = c.recv(len(msg) - len(got))
                if not b:
                    raise OSError("closed after %d bytes" % len(got))
                got += b
            if got == msg:
                ok += 1
        c.close()
    except OSError as e:
        err = str(e)
    print(json.dumps({"ok": ok, "n": n, "err": err}))


def udpcheck(addr):
    fam, host, port = split(addr)
    n = int(arg("--n", "20"))
    src = arg("--src")
    ok, err = 0, ""
    s = socket.socket(fam, socket.SOCK_DGRAM)
    s.settimeout(2)
    if src:
        s.bind((src, 0))
    conn = "--connected" in sys.argv
    if conn:
        s.connect((host, port))
    for i in range(n):
        msg = b"udpt-%d-" % i + os.urandom(48)
        try:
            if conn:
                s.send(msg)
            else:
                s.sendto(msg, (host, port))
            end = time.time() + 2
            while time.time() < end:
                b, _ = s.recvfrom(65536)
                if b == msg:
                    ok += 1
                    break
        except OSError as e:
            err = str(e)
    print(json.dumps({"ok": ok, "n": n, "err": err}))


def hold(addr):
    fam, host, port = split(addr)
    s = socket.socket(fam, socket.SOCK_DGRAM)
    s.bind((host, port))
    print("holding udp %s" % addr, flush=True)
    while True:
        time.sleep(3600)


if __name__ == "__main__":
    cmd, addr = sys.argv[1], sys.argv[2]
    {"echo": echo, "tcpcheck": tcpcheck, "udpcheck": udpcheck, "hold": hold}[cmd](addr)
