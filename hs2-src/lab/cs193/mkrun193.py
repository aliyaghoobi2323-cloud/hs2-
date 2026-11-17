# mkrun193.py: /opt/hs2lab/cs193/h/run147.sh on lab3 = rr155's run147.sh + the lb74 run.sh NETEM_EVENTS block
# (rr155's run147.sh has no NETEM_EVENTS support: on lab3 the rr155/rr157 rep.sh dark/flap/loss events never ran).
s = open("/opt/hs2lab/rr155/run147.sh").read()
b = open("/opt/hs2lab/lb74/labsrc/lab/run.sh").read()
i = b.index("# lb74: NETEM_EVENTS="); j = b.index('rm -f "$W/lbsamp.jsonl"', i)
a = 'rm -f "$W/lbsamp.jsonl" "$W/lossevents.txt"'
assert s.count(a) == 1
open("/opt/hs2lab/cs193/h/run147.sh", "w").write(s.replace(a, b[i:j] + a))
