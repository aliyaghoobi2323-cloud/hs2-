#!/usr/bin/env python3
"""sumjson.py FILE.json [LABEL]: a go test -json stream summed up: per
package the top-level tests and the subtests passed / failed / skipped, the
package's result and time, and each failure with the tail of its output.
Exit 1 when anything failed (a test, or a package with no test result)."""
import json, sys, collections

path = sys.argv[1]
label = sys.argv[2] if len(sys.argv) > 2 else path
pk = collections.OrderedDict()
out = collections.defaultdict(list)
bad_lines = 0
for line in open(path, errors="replace"):
    line = line.strip()
    if not line.startswith("{"):
        if line:
            bad_lines += 1
            out[("", "")].append(line)
        continue
    try:
        ev = json.loads(line)
    except ValueError:
        bad_lines += 1
        continue
    p = ev.get("Package", "")
    t = ev.get("Test", "")
    a = ev.get("Action")
    st = pk.setdefault(p, {"top": collections.Counter(), "sub": collections.Counter(), "res": None, "el": None, "fails": []})
    if a == "output":
        out[(p, t)].append(ev.get("Output", "").rstrip("\n"))
        continue
    if a in ("pass", "fail", "skip"):
        if t:
            (st["sub"] if "/" in t else st["top"])[a] += 1
            if a == "fail":
                st["fails"].append(t)
        else:
            st["res"] = a
            st["el"] = ev.get("Elapsed")
tot = collections.Counter()
rc = 0
print("== %s" % label)
print("%-55s %-5s %7s  %-22s %-22s" % ("package", "res", "s", "top pass/fail/skip", "sub pass/fail/skip"))
for p, st in pk.items():
    if not p:
        continue
    for k in ("pass", "fail", "skip"):
        tot["top_" + k] += st["top"][k]
        tot["sub_" + k] += st["sub"][k]
    res = st["res"] or "none"
    if res != "pass" and res != "skip":
        rc = 1
    print("%-55s %-5s %7s  %-22s %-22s" % (p, res, "%.1f" % st["el"] if st["el"] is not None else "-",
          "%d/%d/%d" % (st["top"]["pass"], st["top"]["fail"], st["top"]["skip"]),
          "%d/%d/%d" % (st["sub"]["pass"], st["sub"]["fail"], st["sub"]["skip"])))
print("TOTAL top-level: %d pass, %d fail, %d skip; subtests: %d pass, %d fail, %d skip; packages %d (%d not pass)" % (
    tot["top_pass"], tot["top_fail"], tot["top_skip"], tot["sub_pass"], tot["sub_fail"], tot["sub_skip"],
    len([p for p in pk if p]), len([p for p, s in pk.items() if p and s["res"] not in ("pass", "skip")])))
for p, st in pk.items():
    if p and st["res"] not in ("pass", "skip"):
        print("\n-- package %s: %s" % (p, st["res"] or "no result (build failure, panic or timeout)"))
        for l in out[(p, "")][-25:]:
            print("   " + l)
    for t in st["fails"]:
        print("\n-- FAIL %s %s" % (p, t))
        for l in out[(p, t)][-20:]:
            print("   " + l)
if out[("", "")]:
    print("\n-- non-JSON lines (build errors and the like):")
    for l in out[("", "")][-30:]:
        print("   " + l)
sys.exit(rc)
