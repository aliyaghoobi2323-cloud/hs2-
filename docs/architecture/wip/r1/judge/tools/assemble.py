#!/usr/bin/env python3
"""assemble.py IN OUT JUDGEDIR: judge.fa.md from its template and the tables."""
import re
import sys

src, out, jd = sys.argv[1:4]
t = open(src, encoding="utf-8").read()


def sections(path):
    txt = open(path, encoding="utf-8").read()
    parts = re.split(r"\n(?=#### )", "\n" + txt)
    return [p.strip("\n") for p in parts if p.strip().startswith("####")]


def pick(path, keep):
    return "\n\n".join(s for s in sections(path) if keep(s.splitlines()[0]))


def demote(s):
    # the tables' headings sit one level under the section they land in
    return s


P10_MIX = ("lone16", "one_vs_16", "newheavy", "tunping 100")
rep = {
    "{{P10}}": pick(jd + "/b2/p10.md", lambda h: any(h.startswith("#### " + m) for m in P10_MIX)),
    "{{S5B2}}": pick(jd + "/b2/s5.md", lambda h: h.startswith("#### s5 1000") or h.startswith("#### s5 50 ") or "TUNV6" in h),
    "{{T3}}": pick(jd + "/b2/s5.md", lambda h: h.startswith("#### s5 5 Mbit") or h.startswith("#### s5 1 Mbit")),
}
for k, v in rep.items():
    assert v, k
    t = t.replace(k, v)


def tab(m):
    return open(jd + "/" + m.group(1), encoding="utf-8").read().strip("\n")


t = re.sub(r"\{\{T:([^}]+)\}\}", tab, t)
assert "{{" not in t
open(out, "w", encoding="utf-8").write(t)
print("ok", len(t))
