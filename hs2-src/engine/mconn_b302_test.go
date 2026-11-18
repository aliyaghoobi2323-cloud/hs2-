package engine

import (
	"math/rand"
	"sync"
	"testing"
)

// TestB302CopiesSame: over random open / markCopy / close sequences on several
// links, the b302 counter equals rel69m's walk after every step (the choice
// input of every light/call placement predicate is the same number).
func TestB302CopiesSame(t *testing.T) {
	rng := rand.New(rand.NewSource(302))
	for round := 0; round < 6; round++ {
		links := []*v2PipeLink{newV2PipeLink(t), newV2PipeLink(t), newV2PipeLink(t)}
		var open [][]*countedStream = make([][]*countedStream, len(links))
		for step := 0; step < 400; step++ {
			i := rng.Intn(len(links))
			l := links[i]
			switch r := rng.Intn(10); {
			case r < 4 || len(open[i]) == 0:
				open[i] = append(open[i], l.open(t))
			case r < 7:
				open[i][rng.Intn(len(open[i]))].markCopy() // also on a copy already
			case r < 9:
				k := rng.Intn(len(open[i]))
				open[i][k].Close()
				if rng.Intn(3) == 0 { // a closed stream marked later (late join write)
					open[i][k].markCopy()
				}
				open[i] = append(open[i][:k], open[i][k+1:]...)
			default:
				if len(open[i]) > 0 {
					open[i][0].Close() // double close
				}
			}
			for j, lj := range links {
				walk := linkCopiesWalk(lj.mtcpLink)
				b302CopyCnt.Store(true)
				got := linkCopies(lj.mtcpLink)
				b302CopyCnt.Store(false)
				old := linkCopies(lj.mtcpLink)
				b302CopyCnt.Store(true)
				if got != walk || old != walk {
					t.Fatalf("round %d step %d link %d: counter %d walk %d old %d", round, step, j, got, walk, old)
				}
			}
		}
	}
}

// TestB302CopiesConcurrent: concurrent marks and closes (race detector) leave
// the counter equal to the walk.
func TestB302CopiesConcurrent(t *testing.T) {
	l := newV2PipeLink(t)
	var css []*countedStream
	for i := 0; i < 200; i++ {
		css = append(css, l.open(t))
	}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := g; i < len(css); i += 4 {
				css[i].markCopy()
				if i%3 == 0 {
					css[i].Close()
				}
				_ = linkCopies(l.mtcpLink)
			}
		}(g)
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := len(css) - 1 - g; i >= 0; i -= 4 {
				if i%5 == 0 {
					css[i].Close()
				}
			}
		}(g)
	}
	wg.Wait()
	if got, walk := linkCopies(l.mtcpLink), linkCopiesWalk(l.mtcpLink); got != walk {
		t.Fatalf("counter %d walk %d", got, walk)
	}
}
