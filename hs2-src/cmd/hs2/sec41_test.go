package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

// sec41: the old "carrier":"reality" is flagged (replayable, unlimited relay).
func TestCheckWarnsLegacyRealityCarrier(t *testing.T) {
	cfg := `{"mode":"dial","carrier":"reality","addr":"1.2.3.4:443","sni":"www.site.ir","shared_key":"` + strings.Repeat("ab", 32) + `"}`
	_, warns := checkConfig([]byte(cfg), func(net.IP) bool { return false }, time.Now())
	for _, w := range warns {
		if strings.Contains(w, "OLD experimental carrier") {
			return
		}
	}
	t.Fatalf("no legacy-carrier warning in %q", warns)
}
