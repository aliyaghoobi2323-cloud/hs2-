package engine

import (
	"strings"
	"testing"
	"time"
)

// Three links in a row dead within 20 s of coming up → one hint naming tun over
// icmp; a long-lived link resets the run; the hint is not repeated.
func TestShortLivedLinksHint(t *testing.T) {
	var s shortLivedLinks
	if s.note(3*time.Second) != "" || s.note(5*time.Second) != "" {
		t.Fatal("hinted before three short lives")
	}
	h := s.note(4 * time.Second)
	if !strings.Contains(h, "tun → icmp") || !strings.Contains(h, "3 links") {
		t.Fatalf("hint: %q", h)
	}
	if s.note(2*time.Second) != "" {
		t.Fatal("hint repeated")
	}
	s.note(time.Minute) // a link that lived: the path carries TCP now
	if s.note(time.Second) != "" || s.note(time.Second) != "" || s.note(time.Second) == "" {
		t.Fatal("after a long-lived link the run did not restart from zero")
	}
}
