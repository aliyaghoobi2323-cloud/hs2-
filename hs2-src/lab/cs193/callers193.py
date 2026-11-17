# callers193.py (cs193) = rr155 byte-checked callers.py + receive-gap logging: ev=gap for every gap > GAPLOG s
# (env, 0.5) and maxgap_ms on DIED (the longest receive gap of that call generation).
# callers.py ADDR N OUT T0FILE: N long-lived 2 Mbit echo "call" streams (probe server mode E);
# each logs exactly when/how it dies (one JSON line) and reconnects after 1 s.
# rr155: the stream is a fixed pseudo-random pattern (period PL, prime) and every echoed byte is
# checked against it at its offset: a lost, duplicated or reordered byte logs CORRUPT and ends the
# stream (phase=verify). A DIED line carries ok_bytes (verified) and the tx/rx counts.
import asyncio, json, sys, time, random, os
GL = float(os.environ.get("GAPLOG", "0.5"))
ADDR, N, OUT, T0F = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
host, port = ADDR.rsplit(":", 1); port = int(port)
FR, GAP = 2000, 0.008  # 2000 B / 8 ms = 2 Mbit/s
PL = 1000003
PAT = random.Random(155).randbytes(PL)
PAT2 = PAT + PAT[:200000]
f = open(OUT, "a", buffering=1)
def t0():
    try: return int(open(T0F).read())
    except Exception: return 0
def log(**k):
    now = time.time(); k["utc"] = time.strftime("%H:%M:%S", time.gmtime(now)) + ("%.3f" % (now % 1))[1:]
    k["el"] = round(now - t0(), 1) if t0() else None
    f.write(json.dumps(k) + "\n")
def pat(off, n):
    o = off % PL
    return PAT2[o:o + n]
class Corrupt(Exception): pass
async def stream(i):
    await asyncio.sleep(i * 0.2)
    gen = 0
    while True:
        gen += 1; st = {"tx": 0, "rx": 0, "lrx": time.time(), "stall": 0, "mg": 0.0}
        t = time.time()
        try:
            r, w = await asyncio.wait_for(asyncio.open_connection(host, port), 20)
        except Exception as e:
            log(ev="connfail", i=i, gen=gen, err=repr(e)); await asyncio.sleep(1); continue
        lp = w.get_extra_info("sockname")[1]; log(ev="up", i=i, gen=gen, lport=lp, ms=round((time.time()-t)*1000))
        w.write(b"E"); why = {}
        try:
            await asyncio.wait_for(r.readexactly(1), 20)  # the server echoes the mode byte first
        except Exception as e:
            log(ev="DIED", i=i, gen=gen, lport=lp, phase="hdr", err=repr(e), age=round(time.time()-t, 1), tx=0, rx=0, ok_bytes=0, lastrx_ago=0)
            w.transport.abort(); await asyncio.sleep(1); continue
        async def wr():
            nxt = time.time()
            while True:
                w.write(pat(st["tx"], FR)); st["tx"] += FR
                await w.drain()
                nxt += GAP; d = nxt - time.time()
                if d > 0: await asyncio.sleep(d)
                else: nxt = time.time()
        async def rd():
            while True:
                b = await r.read(65536)
                if not b: raise EOFError("eof (tunnel FIN)")
                o = st["rx"]
                if b != pat(o, len(b)):
                    k = next(j for j in range(len(b)) if b[j] != pat(o + j, 1)[0])
                    log(ev="CORRUPT", i=i, gen=gen, lport=lp, off=o + k, tx=st["tx"])
                    raise Corrupt("corrupt at %d" % (o + k))
                now = time.time(); g = now - st["lrx"]
                if st["rx"] > 0:
                    st["mg"] = max(st["mg"], g)
                    if g > GL: log(ev="gap", i=i, gen=gen, ms=round(g * 1000))
                st["rx"] += len(b); st["lrx"] = now
        async def wd():
            while True:
                await asyncio.sleep(1)
                g = time.time() - st["lrx"]
                if g > 5 and not st["stall"]:
                    st["stall"] = 1; log(ev="stall", i=i, gen=gen, lport=lp, rx=st["rx"], tx=st["tx"])
                if g < 1 and st["stall"]:
                    st["stall"] = 0; log(ev="unstall", i=i, gen=gen, lport=lp)
        tasks = {asyncio.ensure_future(wr()): "write", asyncio.ensure_future(rd()): "read", asyncio.ensure_future(wd()): "wd"}
        done, pend = await asyncio.wait(tasks.keys(), return_when=asyncio.FIRST_EXCEPTION)
        for x in pend: x.cancel()
        d = list(done)[0]; e = d.exception()
        ph = "verify" if isinstance(e, Corrupt) else tasks[d]
        log(ev="DIED", i=i, gen=gen, lport=lp, phase=ph, err=repr(e), age=round(time.time()-t, 1), tx=st["tx"], rx=st["rx"], ok_bytes=st["rx"], maxgap_ms=round(st["mg"]*1000), lastrx_ago=round(time.time()-st["lrx"], 1))
        try: w.transport.abort()
        except Exception: pass
        await asyncio.sleep(1)
async def main():
    await asyncio.gather(*[stream(i) for i in range(N)])
asyncio.run(main())
