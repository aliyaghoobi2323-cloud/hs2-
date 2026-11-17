#!/usr/bin/env python3
# gen63d.py > j63d.txt: r63d rotated job list (each round runs its 3 arms on the 3 labslots at once, arm/slot rotated).
L = []
def rnd(mode, k, arms):
    r = k % 3; a = arms[r:] + arms[:r]
    for x in a: L.append(f"{mode}x{k:02d}{x} {x} {mode}")
for i in range(3): L.append(f"dkd{i+1}M M dk")
d8 = u1 = d1 = 0
for k in range(14):
    if d8 < 12: rnd("d8", d8, "NPM"); d8 += 1
    if u1 < 10: rnd("u1", u1, "NOP"); u1 += 1
    if k % 3 == 2 and d1 < 4: rnd("d1", d1, "NPM"); d1 += 1
while d1 < 4: rnd("d1", d1, "NPM"); d1 += 1
L += ["dkd4M M dk", "dkd5N N dk", "dkd6P P dk"]
print("\n".join(L))
# r63d second batch (gen63d.py e > j63e.txt): E = rel63c edge + rel61h2 exit (edge first), dark x3, d8/d1 E/P/M.
import sys
if len(sys.argv) > 1 and sys.argv[1] == "e":
    L = [f"dke{i+1}E E dk" for i in range(3)]
    for k in range(4): rnd("d8", 20 + k, "EPM")
    for k in range(3): rnd("d1", 20 + k, "EPM")
    print("\n".join(L), file=sys.stderr)
