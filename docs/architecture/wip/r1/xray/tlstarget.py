#!/usr/bin/env python3
"""tlstarget.py CERT KEY HOST:PORT: a TLS 1.3 server (ALPN h2, http/1.1) that
answers any request with a short page and closes: the REALITY "target" of the
lab's xray server (REALITY relays each client hello to it and mirrors its
handshake). Threaded, standard library only."""
import socket, ssl, sys, threading


def serve(c, ctx):
    try:
        c.settimeout(10)
        s = ctx.wrap_socket(c, server_side=True)
        try:
            s.recv(4096)
            s.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
        finally:
            s.close()
    except (OSError, ssl.SSLError):
        try:
            c.close()
        except OSError:
            pass


def main():
    cert, key, addr = sys.argv[1:4]
    h, p = addr.rsplit(":", 1)
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_3
    ctx.load_cert_chain(cert, key)
    ctx.set_alpn_protocols(["h2", "http/1.1"])
    ls = socket.socket()
    ls.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    ls.bind((h, int(p)))
    ls.listen(256)
    print("tlstarget on %s" % addr, flush=True)
    while True:
        c, _ = ls.accept()
        threading.Thread(target=serve, args=(c, ctx), daemon=True).start()


if __name__ == "__main__":
    main()
