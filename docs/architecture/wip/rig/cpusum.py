# cpusum.py FILE.jsonl: cpubench, per streams and variant, median CPU s/GiB [runs] and Gbit/s
import json, sys, statistics as st
R = {}
for l in open(sys.argv[1]):
    if not l.strip().startswith("{"): continue
    d = json.loads(l); R.setdefault((d["streams"], d["v"]), []).append(d)
for k in sorted(R):
    xs = [d["cpu_s_per_gib"] for d in R[k]]; gs = [d["gbit"] for d in R[k]]
    print(k, f"cpu/GiB {st.median(xs):.2f} [" + " ".join(f"{x:.2f}" for x in xs) + f"]  Gbit/s {st.median(gs):.2f} ext_max_mib", [d["ext_max_mib"] for d in R[k]])
