import re, glob, os
# per run: the Iran steering line's asks over the run (asked-dial, dropped, held) and the rate between the first and last line
for d in sorted(glob.glob('/opt/hs2lab/sd151/res/*')):
    f = d + '/ir-steer.log'
    if not os.path.exists(f): continue
    rows = []
    for l in open(f, errors='replace'):
        m = re.match(r'(\d+):(\d+):([\d.]+) stripe steer: .*picks=(\d+) on-cut-path=(\d+).*asked-dial=(\d+)(?: asked-dial-dropped=(\d+) steer-held=(\d+))?', l)
        if m:
            t = int(m.group(1)) * 3600 + int(m.group(2)) * 60 + float(m.group(3))
            rows.append((t, int(m.group(4)), int(m.group(5)), int(m.group(6)), int(m.group(7) or 0), int(m.group(8) or 0)))
    if not rows: print(os.path.basename(d), 'no line'); continue
    a, b = rows[0], rows[-1]
    dt = max(b[0] - a[0], 1)
    env = open(d + '/env.txt').read().split()
    print('%-5s %-8s %-18s lines %3d span %4.0fs picks %4d on-cut %3d asked-dial %5d (%.2f/s) dropped %5d (%.2f/s) held %4d' % (
        os.path.basename(d), env[0], " ".join(env[3:6]) if len(env) > 3 else '', len(rows), dt, b[1], b[2], b[3], (b[3] - a[3]) / dt, b[4], (b[4] - a[4]) / dt, b[5]))
