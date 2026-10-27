#!/usr/bin/env python3
"""labpatch.py DIR [lateDone]: the judge's lab switches (R1 task 17), never a
candidate's code: HS2_LAB_RELFLOOR=1 (C4-lite's floor min(1 MiB/s, 0.5 x
max delivery 2 s), T3) and, for angle B, HS2_LAB_LATE_DONE=1 (a data frame's
Done after its write, not before: T14 iii)."""
import sys

d = sys.argv[1]
p = d + "/hs2-src/engine/sendcap.go"
s = open(p).read()
old = "\t\tc.own = uint64(math.Max(wcNetKOf()*c.maxDlv(), wcPaceFloor))"
new = ("\t\tfloor := float64(wcPaceFloor)\n"
       "\t\tif labRelFloor {\n"
       "\t\t\tfloor = math.Min(floor, 0.5*c.maxDlv())\n"
       "\t\t}\n"
       "\t\tc.own = uint64(math.Max(wcNetKOf()*c.maxDlv(), floor))")
assert s.count(old) == 1
s = s.replace(old, new)
s += ("\n// labRelFloor (HS2_LAB_RELFLOOR=1; the R1 judge's lab build, T3): the\n"
      "// cap's floor is min(1 MiB/s, half the largest delivery rate of 2 s).\n"
      "var labRelFloor = os.Getenv(\"HS2_LAB_RELFLOOR\") == \"1\"\n")
if '"os"' not in s.split(")")[0]:
    s = s.replace("import (", "import (\n\t\"os\"", 1)
open(p, "w").write(s)

if len(sys.argv) > 2 and sys.argv[2] == "lateDone":
    p = d + "/hs2-src/engine/linksched.go"
    s = open(p).read()
    old = "\t\tearly := it.u != nil\n"
    new = "\t\tearly := it.u != nil && !labLateDone\n"
    assert s.count(old) == 1
    s = s.replace(old, new)
    s += ("\n// labLateDone (HS2_LAB_LATE_DONE=1; the R1 judge's lab build, T14):\n"
          "// a data frame's Done after its write, as smux's own writer gave it.\n"
          "var labLateDone = os.Getenv(\"HS2_LAB_LATE_DONE\") == \"1\"\n")
    if '"os"' not in s.split(")")[0]:
        s = s.replace("import (", "import (\n\t\"os\"", 1)
    open(p, "w").write(s)
print("patched", d)
