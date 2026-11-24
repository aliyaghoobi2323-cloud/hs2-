package engine

import (
	"net/netip"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/censor"
)

// b429: on the Iran side every own abort is counted once, and counted
// reported once, by the first control ping that carries it, however many
// links on the IP ping.
func TestCensorOwnAbortReportedOnce(t *testing.T) {
	ip := netip.MustParseAddr("192.0.2.60")
	own := func() (a, r uint64) {
		st := censor.PeerCounters()
		return st.OwnAborts, st.OwnReported
	}
	a0, r0 := own()
	censorAbortBits(ip) // baseline ping: nothing to report
	for i := 0; i < 3; i++ {
		censorNoteOwnAbort(ip)
	}
	if a, r := own(); a-a0 != 3 || r != r0 {
		t.Fatalf("counted %d reported %d, want 3, 0", a-a0, r-r0)
	}
	w1 := censorAbortBits(ip) // link 1's ping
	w2 := censorAbortBits(ip) // link 2's ping, same count
	if w1 != w2 {
		t.Fatalf("links carry different counts %x %x", w1, w2)
	}
	if _, r := own(); r-r0 != 3 {
		t.Fatalf("reported %d want 3", r-r0)
	}
	censorNoteOwnAbort(ip)
	censorAbortBits(ip)
	censorAbortBits(ip)
	if a, r := own(); a-a0 != 4 || r-r0 != 4 {
		t.Fatalf("counted %d reported %d, want 4, 4", a-a0, r-r0)
	}
}
