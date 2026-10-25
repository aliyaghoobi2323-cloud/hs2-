package smux

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"testing"
	"time"
)

// coverOffRuns are smux's windows in a few runs, a line each: the updates
// a stream got (what had been read, and the window), its claim on the pool
// at the end, and the windows the pool could not cover. off is applied to
// each run's config. The runs: a stream held by its window (heldRounds) with
// a pool; under the window core's ceiling, growth allowed; the same with a
// pool too small for a step past the need; with an empty pool; and with a
// reader that lags (the window comes down). They use nothing base (ec15a8f,
// before the cover) did not have, so that base makes them too.
func coverOffRuns(t *testing.T, off func(*Config)) []string {
	run := func(name string, pool int, ceil uint32, rounds int, lag bool) string {
		cfg := poolConfig(NewWindowPool(pool))
		off(cfg)
		session, s, p := newRawPeer(t, cfg)
		if ceil > 0 {
			session.SetCoreRef(0, 0, true)
			s.SetCoreCeil(ceil)
		}
		if lag {
			// the reader takes half of each round and the rest late: a
			// backlog past StreamLagTarget brings the window down
			for i := 0; i < rounds; i++ {
				p.sync(s)
				n := p.behind()
				p.send(n, 4096)
				p.settle(s)
				readN(t, s, n/2, 4096)
				time.Sleep(5 * time.Millisecond)
				readN(t, s, n-n/2, 4096)
			}
			p.sync(s)
		} else {
			heldRounds(t, p, s, rounds, 5*time.Millisecond)
		}
		_, c := session.CoreSnapshot(nil)
		return fmt.Sprintf("%s claim %d short %d updates %v", name, claimOf(s), c.PoolShort, p.updates())
	}
	return []string{
		run("pool", 4<<20, 0, 16, false),
		run("ceiling", 4<<20, 1<<20, 16, false),
		run("short", 64<<10, 1<<20, 12, false),
		run("empty", 0, 0, 12, false),
		run("lag", 4<<20, 0, 12, true),
	}
}

// With the cover off (Config.NoCover: HS2_TUNE_SMUX_COVER=0; the roll-back
// of the window work takes HS2_TUNE_WCORE=0 too) smux's windows are base's
// (ec15a8f, before the cover) update for update: coverOffRuns gives what
// testdata/cover_off.txt holds, made on ec15a8f. SMUX_COVER_OFF_GOLDEN=1
// writes the file anew from this tree (only for a change to the windows
// meant to show with the cover off too).
func TestCoverOffIsBase(t *testing.T) {
	const golden = "testdata/cover_off.txt"
	got := coverOffRuns(t, func(c *Config) { c.NoCover = true })
	if os.Getenv("SMUX_COVER_OFF_GOLDEN") == "1" {
		if err := ioutil.WriteFile(golden, []byte(strings.Join(got, "\n")+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := ioutil.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(got) != len(want) {
		t.Fatalf("%d runs, %s has %d", len(got), golden, len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("with the cover off:\n%s\nbase:\n%s", got[i], want[i])
		}
	}
}
