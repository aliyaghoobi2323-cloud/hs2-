#!/usr/bin/env python3
# trparse.py LOG: HS2_DL76_TRACE lines per second (grouped by the log's
# second), streams moving >1 MiB/s: on the writer side the share of the
# second spent waiting for window / in the frame writer; on the reader side
# window and unread backlog.
import re, sys, collections
by = collections.OrderedDict()
for l in open(sys.argv[1], errors='replace'):
    if 'dl76tr' not in l: continue
    ts = l.split()[1] if len(l.split()) > 1 else ''
    d = dict(re.findall(r'(\w+)=(\S+)', l))
    by.setdefault(ts, []).append(d)
for ts, rows in by.items():
    w = [r for r in rows if int(r['wr']) > 1024]; rd = [r for r in rows if int(r['rd']) > 1024]
    out = ts
    if w:
        out += ' W n=%d MB=%.0f winwait=%s sock=%s pwinK=%s infl=%s' % (len(w), sum(int(r['wr']) for r in w)/1024,
            sorted(int(r['winwait']) for r in w), sorted(int(r['sock']) for r in w), sorted(int(r['pwin']) for r in w), sorted(int(r['infl']) for r in w))
    if rd:
        out += ' R n=%d MB=%.0f rwinK=%s bufK=%s off=%d' % (len(rd), sum(int(r['rd']) for r in rd)/1024,
            sorted(int(r['rwin']) for r in rd), sorted(int(r['buf']) for r in rd), sum(r['off'] == 'true' for r in rd))
    print(out)
