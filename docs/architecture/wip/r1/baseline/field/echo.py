# Echo server for the field probe on a given address: echo.py HOST PORT.
import asyncio, sys
async def handle(r, w):
    try:
        while True:
            d = await r.read(4096)
            if not d:
                break
            w.write(d)
            await w.drain()
    except OSError:
        pass
    finally:
        w.close()
async def main():
    srv = await asyncio.start_server(handle, sys.argv[1], int(sys.argv[2]))
    async with srv:
        await srv.serve_forever()
asyncio.run(main())
