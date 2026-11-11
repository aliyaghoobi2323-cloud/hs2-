package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestMonoNanoRoundTrip(t *testing.T) {
	now := time.Now()
	x := monoNano(now)
	if d := now.Sub(monoTime(x)); d < -time.Microsecond || d > time.Microsecond {
		t.Fatalf("round trip off by %v", d)
	}
	// it reads like UnixNano near start-up (status lines stay readable)
	if d := time.Duration(x - now.UnixNano()); d < -time.Second || d > time.Second {
		t.Fatalf("monoNano %d far from UnixNano %d", x, now.UnixNano())
	}
	// encoded from the monotonic clock: exactly the elapsed time since the base
	if got, want := x-monoBaseNano, int64(now.Sub(monoBase)); got != want {
		t.Fatalf("offset %d, want %d", got, want)
	}
	// fake clocks (no monotonic reading) keep their wall meaning
	f := time.Unix(1000, 5)
	if monoNano(f) != f.UnixNano() {
		t.Fatal("fake clock not passed through")
	}
	if d := f.Add(time.Second).Sub(monoTime(monoNano(f))); d != time.Second {
		t.Fatalf("fake clock sub %v", d)
	}
}

// No hot-path atomic may hold the wall clock again (an NTP step would move
// them all, see monoclock.go): engine code converts with monoNano/monoTime.
func TestNoWallClockNanosInEngine(t *testing.T) {
	allow := map[string]bool{"control.go": true, "monoclock.go": true} // control: an echoed ping stamp, rtt>0 guarded
	re := regexp.MustCompile(`\.UnixNano\(\)|time\.Unix\(0,`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || allow[f] {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range strings.Split(string(b), "\n") {
			if re.MatchString(l) && !strings.HasPrefix(strings.TrimSpace(l), "//") {
				t.Errorf("%s:%d: wall-clock nanos: %s", f, i+1, strings.TrimSpace(l))
			}
		}
	}
}
