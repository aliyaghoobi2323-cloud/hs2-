# an166.py ARMDIR...: rr166 summary (rr157 an157.py, teardown cut from env.txt TT) per arm (HS2_MCONN_TRACE off, so the call's mechanism is its error text):
# call DIED per event window (excluding the teardown at el >= 995) with error, CORRUPT, crowd fails per window,
# crowd/call/game p99, RSS max, last rescue-rst counter line per side.
import json, re, sys, collections
ST = [(0, "pre"), (20, "ip1"), (90, "rst"), (160, "ip2x"), (230, "flap"), (340, "loss_all"), (495, "post")]
def win(el):
    x = el % 500; n = "pre"
    for a, k in ST:
        if x >= a: n = k
    return n
for A in sys.argv[1:]:
    print("==", A)
    TT = int(re.search(r"TT=(\d+)", open(A + "/env.txt").read()).group(1))
    died = collections.Counter(); corrupt = 0; td = 0
    for l in open(A + "/calls.jsonl"):
        d = json.loads(l)
        if d["ev"] == "CORRUPT": corrupt += 1
        if d["ev"] == "DIED" and d.get("el") is not None:
            if d["el"] >= TT - 5: td += 1; continue
            died[(win(d["el"]), int(d["el"]) // 500, d.get("err", "")[:40])] += 1
    print("calls DIED in-run:", sum(died.values()), "teardown:", td, "CORRUPT:", corrupt)
    for k, v in sorted(died.items()): print("   ", v, "window=%s cycle=%d err=%s" % k)
    fw = collections.Counter()
    for l in open(A + "/fails.txt"):
        try: fw[win(float(l))] += 1
        except ValueError: pass
    print("crowd fails by window:", dict(fw), "total", sum(fw.values()))
    r = json.load(open(A + "/result.json")); p = r.get("probe", r)
    for k in ("crowd", "call", "game"):
        x = p.get(k, {})
        print("  %-5s p50 %s p99 %s max %s fail/lost %s" % (k, x.get("p50_ms"), x.get("p99_ms"), x.get("max_ms"), x.get("fail", x.get("lost"))))
    print("  rss max MB ir %s kh %s" % (r.get("rss_ir_mb"), r.get("rss_kh_mb")))
    last = {}
    for l in open(A + "/resc.txt", errors="replace"):
        f = l.split()
        if len(f) > 2 and "rescue-rst:" in l: last[f[1]] = l[l.index("rescue-rst:"):].strip()
    for s, v in sorted(last.items()): print("  ", s, v)
