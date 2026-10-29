package engine

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// stubCC stands in the kernel: it takes the names in ok and records each
// set (R1 round 5, quietcc.go).
func stubCC(t *testing.T, ok ...string) *[]string {
	t.Helper()
	var sets []string
	prev, prevLog := setCongestion, quietCCLogf
	setCongestion = func(tc *net.TCPConn, cc string) bool {
		for _, o := range ok {
			if o == cc {
				sets = append(sets, cc)
				return true
			}
		}
		return false
	}
	quietCCLogf = func(string, ...any) {}
	t.Cleanup(func() { setCongestion, quietCCLogf = prev, prevLog })
	return &sets
}

// TestQuietCCList: the first of the list the kernel takes; none taken
// leaves the socket's own (""), and the log says which one ran.
func TestQuietCCList(t *testing.T) {
	stubCC(t, "cubic", "bbr")
	tc := &net.TCPConn{}
	cases := []struct{ list, want string }{
		{"illinois,cubic", "cubic"},
		{"cubic", "cubic"},
		{" illinois , bbr ", "bbr"},
		{"illinois,westwood", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := setQuietCC(tc, c.list); got != c.want {
			t.Errorf("setQuietCC(%q) = %q, want %q", c.list, got, c.want)
		}
	}
	var logs []string
	quietCCLogf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	quietCCSeen.got = "?"
	setQuietCC(tc, "illinois,cubic")
	setQuietCC(tc, "illinois,cubic") // the same: no second line
	if len(logs) != 1 || !strings.Contains(logs[0], "run cubic") || !strings.Contains(logs[0], "not in this kernel") {
		t.Fatalf("log lines %q", logs)
	}
}

// TestQuietCCFew: a pool of up to five links runs QuietCCFew on its QUIET
// links, a larger one (or one whose size is not known) QuietCC.
func TestQuietCCFew(t *testing.T) {
	prevFew, prevCC, prevMax := QuietCCFew, QuietCC, quietPoolMax.Load()
	t.Cleanup(func() { QuietCCFew, QuietCC = prevFew, prevCC; quietPoolMax.Store(prevMax) })
	QuietCCFew, QuietCC = "illinois,cubic", "cubic"
	for _, c := range []struct {
		max  int
		want string
	}{{0, "cubic"}, {1, "illinois,cubic"}, {5, "illinois,cubic"}, {6, "cubic"}, {300, "cubic"}} {
		notePoolMax(c.max)
		if got := quietCCList(); got != c.want {
			t.Errorf("max_links %d: %q, want %q", c.max, got, c.want)
		}
	}
	QuietCCFew = ""
	notePoolMax(1)
	if got := quietCCList(); got != "cubic" {
		t.Errorf("QuietCCFew empty: %q", got)
	}
}

// ccFeed gives the watcher n readings a second apart: each second segs
// sent and resent of them, at queue q.
func ccFeed(w *quietCCWatch, st *tcpStat, now *time.Duration, n int, segs, resent uint32, q time.Duration) {
	for i := 0; i < n; i++ {
		*now += time.Second
		st.segsOut += segs
		st.retrans += uint64(resent)
		st.minRTT = 75000
		st.rtt = 75000 + uint32(q/time.Microsecond)
		w.step(*now, *st)
	}
}

// TestQuietCCWatch: random loss (a share past QuietLossPpm with no queue)
// switches the socket to QuietCCLoss within the window; the losses of a
// queue (CoDel's at its flow queue) never do; quietCalm without loss
// switches it back; a kernel that refuses it is tried once.
func TestQuietCCWatch(t *testing.T) {
	sets := stubCC(t, "cubic", "bbr", "illinois")
	w := newQuietCCWatch(&net.TCPConn{}, "cubic", "bbr")
	var st tcpStat
	var now time.Duration
	// no loss: nothing
	ccFeed(w, &st, &now, 10, 1500, 0, time.Millisecond)
	if w.on || len(*sets) != 0 {
		t.Fatalf("no loss switched: on %v sets %v", w.on, *sets)
	}
	// CoDel's losses: 0.1% resent at an 8 ms queue (its flow queue)
	ccFeed(w, &st, &now, 10, 1500, 2, 8*time.Millisecond)
	if w.on {
		t.Fatalf("a queue's losses switched (ppm %d, queue %v)", w.ppm, w.queue)
	}
	// the field's random loss: 0.03% of 1500 segments a second (one
	// resent each two seconds) at a 1 ms queue
	for i := 0; i < 2*quietLossWin && !w.on; i++ {
		ccFeed(w, &st, &now, 1, 1500, uint32(i%2), time.Millisecond)
	}
	if !w.on || (*sets)[len(*sets)-1] != "bbr" {
		t.Fatalf("random loss did not switch: on %v ppm %d queue %v sets %v", w.on, w.ppm, w.queue, *sets)
	}
	at := now
	// still lossy (its own queue may grow now): it stays
	ccFeed(w, &st, &now, 20, 1500, 1, 20*time.Millisecond)
	if !w.on {
		t.Fatal("left the loss CC while the loss went on")
	}
	// calm: back after quietCalm, not before (the window still holds the
	// latest loss for quietLossWin readings)
	last := now
	ccFeed(w, &st, &now, int(quietCalm/time.Second)-5, 1500, 0, time.Millisecond)
	if !w.on {
		t.Fatalf("back before quietCalm (%v after the latest loss)", now-last)
	}
	ccFeed(w, &st, &now, quietLossWin+10, 1500, 0, time.Millisecond)
	if w.on || (*sets)[len(*sets)-1] != "cubic" || w.switches != 2 {
		t.Fatalf("not back to cubic after calm: on %v sets %v switches %d (switched at %v)", w.on, *sets, w.switches, at)
	}
	if !strings.Contains(w.line(3), "quiet cc link 3: cubic (loss switch to bbr") {
		t.Fatalf("line %q", w.line(3))
	}
	// a kernel without it: tried once, the socket keeps its own
	sets2 := stubCC(t, "cubic")
	w2 := newQuietCCWatch(&net.TCPConn{}, "cubic", "nosuchcc")
	var st2 tcpStat
	var now2 time.Duration
	ccFeed(w2, &st2, &now2, 20, 1500, 3, time.Millisecond)
	if w2.on || !w2.failed || len(*sets2) != 0 {
		t.Fatalf("refused CC: on %v failed %v sets %v", w2.on, w2.failed, *sets2)
	}
}

// TestQuietCCWrap: the kernel's u32 segment counter wraps without a false
// share.
func TestQuietCCWrap(t *testing.T) {
	stubCC(t, "cubic", "bbr")
	w := newQuietCCWatch(&net.TCPConn{}, "cubic", "bbr")
	st := tcpStat{segsOut: ^uint32(0) - 2000}
	var now time.Duration
	ccFeed(w, &st, &now, 6, 1500, 0, time.Millisecond)
	if w.on || w.ppm != 0 {
		t.Fatalf("wrap read as loss: ppm %d", w.ppm)
	}
}
