#!/usr/bin/env python3
"""fieldmix.py CAP.jsonl.gz: what the edge's user port really sees from the
owner's users (R1 round 3, xray): the 20-minute capture of R1 task 3
(hs2-src/engine/testdata/c1_replay.jsonl.gz; one line per user TCP connection
and 100 ms tick that moved bytes, no addresses).

Per connection: lifetime in the capture, bytes, and 2-s windows (the
classifier's span, hs2-src/engine/classifier.go) each judged
  bulk     either direction >= 64 KiB in the window (classLightSpan), or
           >= 16 KiB in one 200 ms (classLightBurst)
  light    some bytes, under both
  steady   a light window with bytes in >= 15 of its 20 ticks (a game, a
           call, a stream of small messages: 30-64 Hz traffic fills ~every
           tick)
A connection is "mixed" when it has a bulk window and at least 10 light
windows (20 s of light activity). For R1's QUIET/BULK placement (β2) a
connection moves to BULK once and never back: light and steady windows after
the first bulk window are light traffic riding a BULK link.
"""
import gzip, json, sys
from collections import defaultdict

SPAN, BURST = 64 << 10, 16 << 10


def main():
    path = sys.argv[1]
    ticks = defaultdict(dict)   # flow -> tick -> [up, dn]
    tmin, tmax = None, 0
    with gzip.open(path, "rt") as fh:
        for line in fh:
            r = json.loads(line)
            k = round(r["t"] / 100)
            tmin = k if tmin is None else min(tmin, k)
            tmax = max(tmax, k)
            u = ticks[r["f"]].setdefault(k, [0, 0])
            u[0] += r["up"]
            u[1] += r["dn"]
    span_s = (tmax - tmin) / 10
    out = {"capture_s": round(span_s, 1), "conns": len(ticks)}
    life = []
    n_bulk = n_mixed = n_steady = n_steady_bulk = 0
    light_s = light_after_s = steady_s = steady_after_s = 0
    bytes_tot = bytes_mixed = 0
    long_mixed = 0
    for f, tk in ticks.items():
        ks = sorted(tk)
        first, last = ks[0], ks[-1]
        lf = (last - first) / 10
        cens = first - tmin < 100 or tmax - last < 100
        life.append((lf, cens))
        b = sum(v[0] + v[1] for v in tk.values())
        bytes_tot += b
        wins = []
        for w0 in range(first - (first - tmin) % 20, last + 1, 20):
            up = dn = 0
            act = 0
            burst = False
            for k in range(w0, w0 + 20):
                v = tk.get(k)
                if v:
                    up += v[0]
                    dn += v[1]
                    act += 1
            # 200 ms bursts: two ticks
            for k in range(w0, w0 + 20, 2):
                a, c = tk.get(k, [0, 0]), tk.get(k + 1, [0, 0])
                if a[0] + c[0] >= BURST or a[1] + c[1] >= BURST:
                    burst = True
            if up == 0 and dn == 0:
                wins.append("idle")
            elif up >= SPAN or dn >= SPAN or burst:
                wins.append("bulk")
            elif act >= 15:
                wins.append("steady")
            else:
                wins.append("light")
        has_bulk = "bulk" in wins
        nl = sum(1 for w in wins if w in ("light", "steady"))
        ns = wins.count("steady")
        light_s += 2 * nl
        steady_s += 2 * ns
        if has_bulk:
            n_bulk += 1
            i = wins.index("bulk")
            after = wins[i + 1:]
            light_after_s += 2 * sum(1 for w in after if w in ("light", "steady"))
            steady_after_s += 2 * after.count("steady")
            if nl >= 10:
                n_mixed += 1
                bytes_mixed += b
                if lf >= 300:
                    long_mixed += 1
        if ns >= 5:
            n_steady += 1
            if has_bulk:
                n_steady_bulk += 1
    life.sort()
    lv = [x for x, _ in life]
    q = lambda p: round(lv[min(len(lv) - 1, int(p / 100 * len(lv)))], 1)
    out.update({
        "life_s_p50": q(50), "life_s_p90": q(90), "life_s_p99": q(99), "life_s_max": q(100),
        "conns_life_ge_60s": sum(1 for x in lv if x >= 60), "conns_life_ge_300s": sum(1 for x in lv if x >= 300),
        "conns_censored": sum(1 for _, c in life if c),
        "conns_with_bulk": n_bulk, "conns_mixed": n_mixed, "conns_mixed_life_ge_300s": long_mixed,
        "bytes_share_mixed": round(bytes_mixed / max(1, bytes_tot), 3),
        "light_s": light_s, "light_s_after_first_bulk": light_after_s,
        "light_share_after_first_bulk": round(light_after_s / max(1, light_s), 3),
        "steady_conns": n_steady, "steady_conns_with_bulk": n_steady_bulk,
        "steady_s": steady_s, "steady_s_after_first_bulk": steady_after_s,
        "steady_share_after_first_bulk": round(steady_after_s / max(1, steady_s), 3),
    })
    print(json.dumps(out, indent=1))


if __name__ == "__main__":
    main()
