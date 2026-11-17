package engine

import (
	"io"
	"testing"
	"time"
)

// iranStreams: the streams still in Iran's smux sessions (smuxNet keeps
// each open's client session, Iran's, at an even index).
func (n *smuxNet) iranStreams() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	k := 0
	for i := 0; i < len(n.sess); i += 2 {
		k += n.sess[i].NumStreams()
	}
	return k
}

// TestMConnEndClosesSubsWithoutPeer (mem66): the user is gone and the backend
// behind the exit never closes (no FIN ever comes back). Every sub of the
// finished Iran connection — the raw first sub its reader never came back
// to included — must leave its smux session within mEndSubGrace, or the
// stream stays in the session's map with every byte the peer sends after
// (lab: Iran's heap grew until the backend's connections closed).
func TestMConnEndClosesSubsWithoutPeer(t *testing.T) {
	setLite(t, true)
	setDeadlineFix(t, true)
	resetLiteBudget()
	for _, how := range []string{"end", "close"} {
		n := &smuxNet{t: t, exit: newMConnExitRaw(78)}
		iran, err := newMConnIran(78, n.open)
		if err != nil {
			t.Fatal(err)
		}
		if iran.raw1 == nil {
			t.Fatal("the first sub is not raw")
		}
		exit := n.exit
		mWaitFor(t, "exit raw sub", time.Second, func() bool { return liveSubs(exit) == 1 })
		req := randBytes(1500, 91)
		if _, err := iran.Write(req); err != nil {
			t.Fatal(err)
		}
		if err := readN(exit, req); err != nil {
			t.Fatal(err)
		}
		go io.Copy(io.Discard, exit) // the backend reads, never closes
		// a reply Iran's app never reads
		if err := xfer(exit, randBytes(64<<10, 92), 16<<10, 0); err != nil {
			t.Fatal(err)
		}
		if n.iranStreams() == 0 {
			t.Fatal("no Iran stream open before the end")
		}
		switch how {
		case "end": // the connection ends under its raw reader (a reset)
			iran.end(errMConnAborted, true)
		case "close": // the user goes (relayStream's Close)
			iran.Close()
		}
		start := time.Now()
		mWaitFor(t, how+": Iran's streams leave their sessions", mEndSubGrace+2*time.Second, func() bool { return n.iranStreams() == 0 })
		t.Logf("%s: Iran's streams gone after %v", how, time.Since(start).Round(time.Millisecond))
		exit.Close()
		n.close()
	}
}
