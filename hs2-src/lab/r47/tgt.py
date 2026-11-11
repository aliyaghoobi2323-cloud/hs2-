#!/usr/bin/env python3
"""tgt.py HOST PORT CERT KEY - a concurrent TLS 1.3 web target (openssl s_server -www serves one connection at a time)."""
import asyncio, ssl, sys

async def main():
    h, p, cert, key = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.load_cert_chain(cert, key)
    ctx.set_alpn_protocols(["http/1.1"])
    async def handle(r, w):
        try:
            buf = b""
            while b"\r\n\r\n" not in buf and len(buf) < 65536:
                b = await asyncio.wait_for(r.read(4096), 30)
                if not b:
                    break
                buf += b
            if buf:
                body = b"<html><body>lab target</body></html>"
                w.write(b"HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: %d\r\n\r\n%s" % (len(body), body))
                await w.drain()
        except Exception:
            pass
        finally:
            try:
                w.close()
            except Exception:
                pass
    srv = await asyncio.start_server(handle, h, p, ssl=ctx, backlog=1024)
    async with srv:
        await srv.serve_forever()

asyncio.run(main())
