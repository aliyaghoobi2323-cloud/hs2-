# secp99.py ARMDIR...: call/game per-second p99 (probe sec_p99, index = second of the run) grouped by event window:
# median and max of the per-second p99 in each window, and the count of seconds over 500 ms.
import json, sys, statistics
ST = [(0, "pre"), (20, "ip1"), (90, "rst"), (160, "ip2x"), (230, "flap"), (340, "loss_all"), (495, "post")]
def win(el):
    x = el % 500; n = "pre"
    for a, k in ST:
        if x >= a: n = k
    return n
for A in sys.argv[1:]:
    p = json.load(open(A + "/result.json"))["probe"]
    for k in ("call", "game"):
        s = p.get(k, {}).get("sec_p99")
        if not s: print(A, k, "no sec_p99", list(p.get(k, {}).keys())); continue
        g = {}
        for i, v in enumerate(s):
            if v is None or v < 0: continue
            g.setdefault(win(i), []).append(v)
        print(A, k, " ".join("%s:med %d max %d >500ms %d" % (w, statistics.median(v), max(v), sum(x > 500 for x in v))
                            for w, v in sorted(g.items(), key=lambda kv: [n for _, n in ST].index(kv[0]))))
