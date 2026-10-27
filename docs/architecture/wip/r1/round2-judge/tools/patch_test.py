import sys

p = sys.argv[1] + "/engine/quiet_test.go"
s = open(p, encoding="utf-8").read()
old = """	const rate = 25e6 / 8
	c := &sendCap{linked: true, alpha: true}
	feed(c, rate, 2*ms, false) // C4-lite's rule: armed from the second reading
"""
new = """	const rate = 25e6 / 8
	// the judge's fix 1: till its own queue first passes 5 ms the cap is
	// C4-lite's (1.25 x the 2 s delivery maximum); the queue ends the start
	s := &sendCap{linked: true, alpha: true}
	feed(s, rate, 2*ms, false)
	feed(s, rate, 2*ms, false) // armed, no queue of its own: C4-lite's cap
	if !s.aStart || !near(s.aRate, wcNetKOf()*rate) {
		t.Fatalf("start: cap %.0f (start %v), want C4-lite's %.0f", s.aRate, s.aStart, wcNetKOf()*rate)
	}
	feed(s, rate, 9*ms, false) // a queue past 5 ms: the start ends, the rules below hold
	if s.aStart || s.aRate >= wcNetKOf()*rate {
		t.Fatalf("a queue: cap %.0f (start %v), want the start over and the cap cut", s.aRate, s.aStart)
	}
	// β2's rules after the start (fix 1 off: armed straight into them)
	TuneAlphaStart = 0
	defer func() { TuneAlphaStart = 1 }()
	c := &sendCap{linked: true, alpha: true}
	feed(c, rate, 2*ms, false) // C4-lite's rule: armed from the second reading
"""
assert s.count(old) == 1
s = s.replace(old, new)
open(p, "w", encoding="utf-8", newline="\n").write(s)
print("ok")
