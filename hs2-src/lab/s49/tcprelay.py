#!/usr/bin/env python3
"""tcprelay.py LISTEN_HOST LISTEN_PORT TARGET_HOST TARGET_PORT - plain TCP relay (low volume: the iperf3 control connection)."""
import asyncio, sys

async def pipe(r, w):
    try:
        while True:
            b = await r.read(65536)
            if not b:
                break
            w.write(b)
            await w.drain()
    except Exception:
        pass
    finally:
        try:
            w.close()
        except Exception:
            pass

async def main():
    lh, lp, th, tp = sys.argv[1], int(sys.argv[2]), sys.argv[3], int(sys.argv[4])
    async def handle(r, w):
        try:
            tr, tw = await asyncio.open_connection(th, tp)
        except Exception:
            w.close(); return
        await asyncio.gather(pipe(r, tw), pipe(tr, w))
    srv = await asyncio.start_server(handle, lh, lp)
    async with srv:
        await srv.serve_forever()

asyncio.run(main())
