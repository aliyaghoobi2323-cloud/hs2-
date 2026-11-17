# an155.py ARMDIR: deaths per event window (calls DIED/CORRUPT; Iran+exit mconn ends by error), RSS
import json, re, sys, glob, collections, time, calendar
A = sys.argv[1]
t0 = int(open(A + "/t0").read())
ST = [(0, "pre"), (20, "ip1"), (90, "rst"), (160, "ip2x"), (230, "flap"), (340, "loss_all"), (495, "post")]
def win(el):
    x = el % 500; n = "pre"
    for a, k in ST:
        if x >= a: n = k
    return n
day = time.strftime("%Y-%m-%d", time.gmtime(t0))
def el_of(hms):
    return calendar.timegm(time.strptime(day + " " + hms.split(".")[0], "%Y-%m-%d %H:%M:%S")) - t0
calls = collections.Counter(); corrupt = 0; ups = 0
for l in open(A + "/calls.jsonl"):
    d = json.loads(l)
    if d["ev"] == "up": ups += 1
    if d["ev"] == "CORRUPT": corrupt += 1
    if d["ev"] == "DIED" and d.get("el") is not None: calls[win(d["el"])] += 1
print("calls DIED by window:", dict(calls), "corrupt:", corrupt, "ups:", ups)
for side in ("ir", "kh"):
    fs = glob.glob(A + "/full/%s.log" % side) + glob.glob(A + "/w*/%s.log" % side)
    if not fs: continue
    ends = collections.Counter()
    for l in open(fs[0], errors="replace"):
        if " end err=" not in l or "mtrace" not in l: continue
        m = re.match(r"(\S+) mtrace \S+ id=\S+ .*?end err=(.*?) rst=", l)
        if not m: continue
        e = m.group(2)
        if "closed" in e or "peer done" in e or "aborted" in e: continue
        ends[(win(el_of(m.group(1))), e[:40])] += 1
    print(side, "error ends:")
    for k, v in sorted(ends.items()): print("  ", v, k)
rs = [l.split() for l in open(A + "/proc.txt") if "rss_kb=" in l]
for side in ("ir", "kh"):
    v = [(int(x[0]), int(x[3].split("=")[1])) for x in rs if x[1] == side]
    if v: print(side, "rss_kb first/max/last:", v[0], max(r for _, r in v), v[-1])
fw = collections.Counter()
for l in open(A + "/fails.txt"):
    try: fw[win(float(l))] += 1
    except ValueError: pass
print("crowd fails by window (probe clock):", dict(fw), "total", sum(fw.values()))
