#!/usr/bin/env python3
# idle139.py ADDR N HOLD OUT: N echo conns ('E' probe protocol) to ADDR; one 16-byte round trip at open,
# idle HOLD s, one more round trip (10 s timeout). OUT gets one JSON line: ok / fail by kind.
import asyncio, json, sys, time
addr, n, hold, out = sys.argv[1], int(sys.argv[2]), float(sys.argv[3]), sys.argv[4]
h, p = addr.rsplit(":", 1)
R = {"open_fail": 0, "first_fail": 0, "ok": 0, "eof": 0, "reset": 0, "timeout": 0, "other": 0}
async def one(i):
    try:
        r, w = await asyncio.wait_for(asyncio.open_connection(h, int(p)), 15)
    except Exception:
        R["open_fail"] += 1; return
    try:
        w.write(b"E" + b"x" * 16); await w.drain()
        await asyncio.wait_for(r.readexactly(17), 15)
    except Exception:
        R["first_fail"] += 1; w.close(); return
    await asyncio.sleep(hold)
    try:
        w.write(b"y" * 16); await w.drain()
        b = await asyncio.wait_for(r.readexactly(16), 10)
        R["ok" if b == b"y" * 16 else "other"] += 1
    except asyncio.IncompleteReadError: R["eof"] += 1
    except ConnectionResetError: R["reset"] += 1
    except asyncio.TimeoutError: R["timeout"] += 1
    except Exception: R["other"] += 1
    w.close()
async def main():
    t = []
    for i in range(n):
        t.append(asyncio.create_task(one(i))); await asyncio.sleep(0.02)
    await asyncio.gather(*t)
asyncio.run(main())
R.update(n=n, hold=hold, end=time.time())
open(out, "a").write(json.dumps(R) + "\n")
