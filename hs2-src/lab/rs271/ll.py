#!/usr/bin/env python3
# rs271 long-lived conns: ll.py server PORT | client HOST PORT N PERIOD DUR OUT
# client: N conns opened over 20 s, each sends 16 B every PERIOD s (random phase) and expects the echo.
# Records per conn the first failure: eof / rst / werr (send error), and tmo = send time of the first
# keepalive not echoed within 10 s (where an app would start its timeout).
import asyncio, sys, time, json, random, struct


async def srv(port):
    async def h(r, w):
        try:
            while True:
                d = await r.read(4096)
                if not d:
                    break
                w.write(d)
                await w.drain()
        except Exception:
            pass
        try:
            w.close()
        except Exception:
            pass
    s = await asyncio.start_server(h, '127.0.0.1', port, backlog=4096)
    async with s:
        await s.serve_forever()


async def cli(host, port, n, per, dur, out):
    t0 = time.time()
    res = {}
    stats = {'open_fail': 0, 'echo': 0, 'rtt': []}
    end = t0 + dur

    async def one(i):
        await asyncio.sleep(random.random() * 20)
        try:
            r, w = await asyncio.wait_for(asyncio.open_connection(host, port), 15)
        except Exception as e:
            stats['open_fail'] += 1
            res[i] = {'open': 'fail', 'err': type(e).__name__}
            return
        st = {'open': time.time(), 'last_echo': None, 'fail': None, 'kind': None, 'tmo': None}
        res[i] = st
        acked = [0]

        def fail(k):
            if st['fail'] is None:
                st['fail'] = time.time()
                st['kind'] = k

        async def rd():
            buf = b''
            while True:
                try:
                    d = await r.read(4096)
                except ConnectionResetError:
                    fail('rst')
                    return
                except Exception as e:
                    fail('rerr:' + type(e).__name__)
                    return
                if not d:
                    fail('eof')
                    return
                buf += d
                while len(buf) >= 16:
                    seq, ts = struct.unpack('>Qd', buf[:16])
                    buf = buf[16:]
                    now = time.time()
                    acked[0] = max(acked[0], seq)
                    st['last_echo'] = now
                    stats['echo'] += 1
                    if len(stats['rtt']) < 200000:
                        stats['rtt'].append(round((now - ts) * 1000, 1))
        rt = asyncio.ensure_future(rd())
        await asyncio.sleep(random.random() * per)
        seq = 0
        while time.time() < end and st['fail'] is None:
            seq += 1
            try:
                w.write(struct.pack('>Qd', seq, time.time()))
                await asyncio.wait_for(w.drain(), 10)
            except Exception as e:
                fail('werr:' + type(e).__name__)
                break
            t = time.time()
            while time.time() < t + 10 and acked[0] < seq and st['fail'] is None:
                await asyncio.sleep(0.2)
            if acked[0] < seq and st['fail'] is None and st['tmo'] is None:
                st['tmo'] = t
            await asyncio.sleep(max(0, t + per - time.time()))
        await asyncio.sleep(max(0, end - time.time()))
        rt.cancel()
        try:
            w.close()
        except Exception:
            pass

    async def tick():
        f = open(out + '.tick', 'w')
        while True:
            al = sum(1 for s in res.values() if s.get('open') != 'fail' and s.get('fail') is None)
            ks = {}
            for s in res.values():
                if s.get('kind'):
                    ks[s['kind']] = ks.get(s['kind'], 0) + 1
            tm = sum(1 for s in res.values() if s.get('tmo'))
            f.write('%.1f alive %d fails %s tmo %d echo %d\n' % (time.time(), al, json.dumps(ks), tm, stats['echo']))
            f.flush()
            await asyncio.sleep(2)
    tk = asyncio.ensure_future(tick())
    await asyncio.gather(*[one(i) for i in range(n)])
    tk.cancel()
    rt = sorted(stats['rtt'])

    def p(q):
        return rt[min(len(rt) - 1, int(q * len(rt)))] if rt else None
    json.dump({'t0': t0, 'open_fail': stats['open_fail'], 'echo': stats['echo'], 'rtt_p50': p(.5), 'rtt_p99': p(.99),
               'rtt_max': rt[-1] if rt else None, 'conns': res}, open(out, 'w'))


if __name__ == '__main__':
    import resource
    resource.setrlimit(resource.RLIMIT_NOFILE, (65536, 65536))
    if sys.argv[1] == 'server':
        asyncio.run(srv(int(sys.argv[2])))
    else:
        asyncio.run(cli(sys.argv[2], int(sys.argv[3]), int(sys.argv[4]), float(sys.argv[5]), float(sys.argv[6]), sys.argv[7]))
