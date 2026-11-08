#!/usr/bin/env python3
"""Kill most of the tunnel's links mid-run and time the pool's rebuild.

    lab/killrebuild.py IR_NS KH_NS PORT AT_S PCT OUT.json [WINDOW_S]

At AT_S seconds it counts the established link sockets (local or remote port
PORT) in the Iran namespace (N), resets PCT% of them from the Kharej side with
one `ss -K` (both ends see a reset at once), then samples the Iran side's count
every 50 ms for WINDOW_S seconds. OUT.json gets one JSON object:

    kill_n        links before the kill      kill_k   links reset
    kill_min      the lowest count after it
    rebuild_t50/t90/t100_s  seconds from the kill until the count is back to
                  50% / 90% / 100% of N (-1: not within the window)
    kill_series   [[t, count], ...] every change, for plots
"""
import json, subprocess, sys, time


def links(ns, port):
    out = subprocess.run(['ip', 'netns', 'exec', ns, 'ss', '-tnH', 'state', 'established',
                          f'( sport = :{port} or dport = :{port} )'],
                         capture_output=True, text=True).stdout
    return [l.split() for l in out.splitlines() if l.strip()]


def port_of(addr):
    return addr.rsplit(':', 1)[1]


def main():
    ir, kh, port, at, pct, out = sys.argv[1:7]
    window = float(sys.argv[7]) if len(sys.argv) > 7 else 30.0
    time.sleep(float(at))
    n = len(links(ir, port))
    socks = links(kh, port)
    k = len(socks) * int(pct) // 100
    # columns (state filtered): Recv-Q Send-Q Local Peer
    terms = ' or '.join(f'( sport = :{port_of(s[2])} and dport = :{port_of(s[3])} )' for s in socks[:k])
    t0 = time.monotonic()
    if terms:
        subprocess.run(['ip', 'netns', 'exec', kh, 'ss', '-K', 'state', 'established', f'( {terms} )'],
                       capture_output=True)
    series, last, lo = [], None, n
    marks = {50: -1.0, 90: -1.0, 100: -1.0}
    while True:
        t = time.monotonic() - t0
        c = len(links(ir, port))
        if c != last:
            series.append([round(t, 3), c])
            last = c
        lo = min(lo, c)
        if lo < n:  # only after the kill has shown
            for p in marks:
                if marks[p] < 0 and c * 100 >= n * p:
                    marks[p] = round(t, 3)
        if t >= window or (marks[100] >= 0):
            break
        time.sleep(0.05)
    res = {'kill_n': n, 'kill_k': k, 'kill_min': lo,
           'rebuild_t50_s': marks[50], 'rebuild_t90_s': marks[90], 'rebuild_t100_s': marks[100],
           'kill_series': series}
    with open(out, 'w') as f:
        json.dump(res, f)


if __name__ == '__main__':
    main()
