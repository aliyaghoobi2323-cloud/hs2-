# trv.py TRACE [side]: one line per wake of a window core trace (B in KiB, rates in Mbit/s)
import json, sys
side = sys.argv[2] if len(sys.argv) > 2 else None
for l in open(sys.argv[1]):
    d = json.loads(l)
    if side and d['side'] != side: continue
    ev = d.get('events') or []
    st = d.get('streams') or []
    ws = ",".join(f"{s.get('win',0)>>10}/{s.get('ceil',0)>>10}" for s in st[:3]) if st else ""
    print(d['t'][14:21], d['side'][:2], f"{d['state']:<7}", 'B', d['b']>>10, 'pub', d['b_pub']>>10, 'nF', d['n_f'],
          'rF %.0f' % (d['r_f']*8/1e6), 'R^ %.0f' % (d['r_hat']*8/1e6), 'qt %.1f' % d['q_turn_ms'], 'qr %.1f' % d['q_ref_ms'],
          'qs %.1f%s' % (d['q_snd_ms'], '' if d['q_snd_ok'] else '?'), 'qo %.1f' % d['q_own_ms'], 'qc %.1f' % d['q_ctrl_ms'],
          'snd %.4f' % d['snd'], 'ooo', d['ooo'], 'hit %.2f' % d['hit'], 'tau %.0f' % d['tau_hold_ms'], ws, ev)
