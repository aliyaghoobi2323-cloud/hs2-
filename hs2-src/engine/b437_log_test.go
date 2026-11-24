package engine

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// b437 F1: a busy second (>= b327PileBusy placements) logs its line only
// when one link took b327PileSpike or more; every busy second still counts.
func TestB437PileLogsOnlySpikes(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	g := &b327Pile
	links := make([]*managedLink, 50)
	for i := range links {
		links[i] = &managedLink{}
	}
	sec := func(perLink int) {
		now := time.Now().Unix()
		for time.Now().Unix() == now { // the start of a fresh second
			time.Sleep(time.Millisecond)
		}
		for _, ml := range links {
			for k := 0; k < perLink; k++ {
				b327PileNote(ml)
			}
		}
	}
	try := func() string {
		buf.Reset()
		busy0, q0, l0 := g.busy.Load(), g.quiet.Load(), g.logged.Load()
		sec(1) // 50 placements, max 1: busy, not a pile
		sec(9) // 450 placements, max 9: a pile (rolled by the next second's first note)
		sec(1)
		now := time.Now().Unix()
		for time.Now().Unix() == now {
			time.Sleep(time.Millisecond)
		}
		b327PileNote(links[0]) // roll the last second
		if d := g.busy.Load() - busy0; d != 3 {
			return fmt.Sprintf("busy seconds %d, want 3", d)
		}
		if q, l := g.quiet.Load()-q0, g.logged.Load()-l0; q != 2 || l != 1 {
			return fmt.Sprintf("quiet %d logged %d, want 2 and 1", q, l)
		}
		if n := strings.Count(buf.String(), "b327pile: t="); n != 1 || !strings.Contains(buf.String(), "max=9") {
			return "lines:\n" + buf.String()
		}
		return ""
	}
	// b449l: b327Pile is global; a pool another test left running can add a
	// busy second of its own (both -shuffle gate runs: "busy seconds 4,
	// want 3"). Up to 3 tries: one clean try passes, a real miscount fails
	// every try.
	var err string
	for i := 0; i < 3; i++ {
		if err = try(); err == "" {
			break
		}
		t.Logf("try %d: %s", i, err)
	}
	if err != "" {
		t.Fatal(err)
	}
	if !strings.Contains(b327Line(), "lines=") {
		t.Fatalf("mem line part: %q", b327Line())
	}
}

// b437 F5: the draining process's lines carry the prefix after the time.
func TestB437DrainingPrefix(t *testing.T) {
	setZDU(t, true)
	f, p := log.Flags(), log.Prefix()
	d := zduDraining.Load()
	t.Cleanup(func() { log.SetFlags(f); log.SetPrefix(p); zduDraining.Store(d) })
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if !ZDUEnterDrain() {
		t.Fatal("drain not entered")
	}
	log.Print("lb auto2: x")
	if s := buf.String(); !strings.Contains(s, " [draining] lb auto2: x") {
		t.Fatalf("line %q", s)
	}
}
