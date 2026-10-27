#!/usr/bin/env python3
"""patch_jitter.py HS2SRC: the OWD beat jittered (0.5 to 1.5 times the tick):
a fixed beat is a timing tell (the judge's DPI capture of version 4 showed
the BULK link's upload direction at one record each 25.08 ms, p90 25.8)."""
import sys

root = sys.argv[1]
p = root + "/engine/owd.go"
s = open(p, encoding="utf-8").read()


def rep(old, new):
    global s
    if s.count(old) != 1:
        sys.exit("owd.go: %d matches for %r" % (s.count(old), old[:80]))
    s = s.replace(old, new)


rep('''	"math"
	"sync"''', '''	"math"
	"math/rand/v2"
	"sync"''')
rep('''		t := time.NewTimer(o.tick())''', '''		t := time.NewTimer(owdJitter(o.tick()))''')
rep('''// queue is the queue delay of this end's sending direction on the link,''', '''// owdJitter is a beat of 0.5 to 1.5 times t: a fixed beat is a timing
// tell (the judge's capture of version 4: on a download's BULK link the
// upload direction carried one record each 25.08 ms, p90 25.8 ms; an idle
// QUIET link's 1 s beat would be the same tell). The queue reading takes
// the least of owdLook, so the mean beat is what counts.
func owdJitter(t time.Duration) time.Duration {
	return t/2 + time.Duration(rand.Int64N(int64(t)+1))
}

// queue is the queue delay of this end's sending direction on the link,''')
open(p, "w", encoding="utf-8", newline="\n").write(s)
print("patched")
