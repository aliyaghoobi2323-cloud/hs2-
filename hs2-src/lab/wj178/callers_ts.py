# callers_ts.py ADDR N OUT T0FILE: callers.py (cd152) with talk spurts: stream i
# with i < NSPURT (env, default N/2) sends in spurts of 1.5-3 s at 2 Mbit then
# pauses 0.8-2.5 s (voice-activity detection: nothing goes either way while
# the user is silent — the echo server mirrors it); the others send all the
# time. Each logs exactly when/how it dies (one JSON line) and reconnects after 1 s.
import asyncio, json, sys, time, os, random
ADDR, N, OUT, T0F = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
NSP = int(os.environ.get("NSPURT", N // 2))
host, port = ADDR.rsplit(":", 1); port = int(port)
FR, GAP = 2000, 0.008  # 2000 B / 8 ms = 2 Mbit/s
f = open(OUT, "a", buffering=1)
def t0():
    try: return int(open(T0F).read())
    except Exception: return 0
def log(**k):
    now = time.time(); k["utc"] = time.strftime("%H:%M:%S", time.gmtime(now)) + ("%.3f" % (now % 1))[1:]
    k["el"] = round(now - t0(), 1) if t0() else None
    f.write(json.dumps(k) + "\n")
async def stream(i):
    await asyncio.sleep(i * 0.2)
    gen = 0; rnd = random.Random(i)
    while True:
        gen += 1; st = {"tx": 0, "rx": 0, "lrx": time.time(), "stall": 0, "talk": 1}
        t = time.time()
        try:
            r, w = await asyncio.wait_for(asyncio.open_connection(host, port), 20)
        except Exception as e:
            log(ev="connfail", i=i, gen=gen, err=repr(e)); await asyncio.sleep(1); continue
        lp = w.get_extra_info("sockname")[1]; log(ev="up", i=i, gen=gen, lport=lp, ms=round((time.time()-t)*1000), spurt=i < NSP)
        w.write(b"E"); buf = b"x" * FR
        async def wr():
            nxt = time.time(); end = nxt + rnd.uniform(1.5, 3)
            while True:
                w.write(buf); st["tx"] += FR
                await w.drain()
                nxt += GAP
                if i < NSP and nxt >= end:
                    st["talk"] = 0
                    await asyncio.sleep(rnd.uniform(0.8, 2.5))
                    st["talk"] = 1; st["lrx"] = max(st["lrx"], time.time() - 0.5)
                    nxt = time.time(); end = nxt + rnd.uniform(1.5, 3)
                d = nxt - time.time()
                if d > 0: await asyncio.sleep(d)
                else: nxt = time.time()
        async def rd():
            while True:
                b = await r.read(65536)
                if not b: raise EOFError("eof (tunnel FIN)")
                st["rx"] += len(b); st["lrx"] = time.time()
        async def wd():
            while True:
                await asyncio.sleep(1)
                if not st["talk"]:
                    continue
                g = time.time() - st["lrx"]
                if g > 5 and not st["stall"]:
                    st["stall"] = 1; log(ev="stall", i=i, gen=gen, lport=lp, rx=st["rx"], tx=st["tx"])
                if g < 1 and st["stall"]:
                    st["stall"] = 0; log(ev="unstall", i=i, gen=gen, lport=lp)
        tasks = {asyncio.ensure_future(wr()): "write", asyncio.ensure_future(rd()): "read", asyncio.ensure_future(wd()): "wd"}
        done, pend = await asyncio.wait(tasks.keys(), return_when=asyncio.FIRST_EXCEPTION)
        for x in pend: x.cancel()
        d = list(done)[0]; e = d.exception()
        log(ev="DIED", i=i, gen=gen, lport=lp, phase=tasks[d], err=repr(e), age=round(time.time()-t, 1), tx=st["tx"], rx=st["rx"], lastrx_ago=round(time.time()-st["lrx"], 1))
        try: w.transport.abort()
        except Exception: pass
        await asyncio.sleep(1)
async def main():
    await asyncio.gather(*[stream(i) for i in range(N)])
asyncio.run(main())
