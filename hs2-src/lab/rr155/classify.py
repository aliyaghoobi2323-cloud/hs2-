# classify.py ARMDIR: each DIED call -> event window + the Iran mtrace end of its mConn (matched by time and bytes)
import json, re, sys, glob, os, collections
A = sys.argv[1]
W = (glob.glob(A + "/w*/ir.log") + glob.glob(A + "/full/ir.log"))[0]
t0 = int(open(A + "/t0").read())
ST = [(0, "pre"), (20, "ip1"), (90, "rst"), (160, "ip2x"), (230, "flap"), (340, "loss_all"), (495, "post")]
def win(el):
    x = el % 500; n = "pre"
    for a, k in ST:
        if x >= a: n = k
    return n
def sec(hms):
    h, m, s = hms.split(":"); return int(h) * 3600 + int(m) * 60 + float(s)
ends, rr = [], collections.defaultdict(list)
for l in open(W, errors="replace"):
    if "mtrace iran" not in l: continue
    m = re.match(r"(\S+) mtrace iran id=(\d+) .*?end err=(.*?) rst=\S+ sent=(\d+)", l)
    if m: ends.append((sec(m.group(1)), m.group(2), m.group(3), int(m.group(4)))); continue
    m = re.match(r"(\S+) mtrace iran id=(\d+) .*?(rescue-rec: answer.*|rescue-rec: RREC.*|rescue: .*)", l)
    if m: rr[m.group(2)].append(m.group(3)[:60])
cnt = collections.Counter()
for l in open(A + "/calls.jsonl"):
    d = json.loads(l)
    if d["ev"] != "DIED": continue
    t = sec(d["utc"]); c = [e for e in ends if -3 < t - e[0] < 1]
    c.sort(key=lambda e: abs(e[3] - d["tx"]))
    mech = c[0][2] if c and abs(c[0][3] - d["tx"]) < 3e6 else "?"
    rid = c[0][1] if mech != "?" else ""
    res = rr.get(rid, [])[-1:] if rid else []
    w = win(d["el"]); cnt[(w, mech[:45], (res[0][:40] if res else "-"))] += 1
    print(d["el"], w, d["i"], d["gen"], d["err"][:28], "|", mech, "|", res)
print("SUMMARY")
for k, v in sorted(cnt.items(), key=lambda x: -x[1]): print(v, k)
