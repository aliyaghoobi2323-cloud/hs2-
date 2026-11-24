//go:build linux

package handoff

import (
	"os"
	"reflect"
	"testing"
	"time"
)

// b428: UNUSED round trip (empty and full) and its decode rejects.
func TestUnusedCodec(t *testing.T) {
	for _, names := range [][]string{nil, {"tcp::80"}, {"tcp::80", "udp::80", "tcp:10.0.0.1:8443"}} {
		b, err := encode(&Msg{Type: TypeUnused, Names: names})
		if err != nil {
			t.Fatal(err)
		}
		m, _, err := decode(b, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(names) == 0 && len(m.Names) == 0 {
			continue
		}
		if !reflect.DeepEqual(m.Names, names) {
			t.Fatalf("%v != %v", m.Names, names)
		}
	}
	for _, bad := range [][]byte{
		{wireVer, TypeUnused, 0, 0},             // no count
		{wireVer, TypeUnused, 0, 2, 1, 0},       // zero-length name
		{wireVer, TypeUnused, 0, 3, 1, 5, 'a'},  // name past the end
		{wireVer, TypeUnused, 0, 3, 0, 1, 'a'},  // trailing bytes
		{wireVer, TypeUnused, 0, 1, MaxFDs + 1}, // too many
	} {
		if _, _, err := decode(bad, 0); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	if _, _, err := decode([]byte{wireVer, TypeUnused, 0, 1, 0}, 1); err == nil {
		t.Fatal("fds on UNUSED accepted")
	}
}

// b428: UNUSED flows only when both Hellos carry FeatUnused; a mixed pair
// (old rc7 peer either way) keeps today's exchange and unused never runs.
func TestGiveUnusedNegotiation(t *testing.T) {
	both := FeatListening | FeatUnused
	for _, c := range []struct{ give, take uint32 }{{both, both}, {both, FeatListening}, {FeatListening, both}, {FeatUnused, FeatUnused}, {0, 0}} {
		a, b := pair(t)
		r, w, _ := os.Pipe()
		got := make(chan []string, 1)
		done := make(chan error, 1)
		go func() {
			_, err := a.GiveListeningUnused(Hello{Proto: Proto, Features: c.give}, []FD{{"tcp::80", r}}, time.Second, 2*time.Second, nil, func(n []string) { got <- n })
			done <- err
		}()
		tk, err := b.Take(Hello{Proto: Proto, Features: c.take}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := tk.Listening(time.Second); err != nil {
			t.Fatalf("%+v: listening: %v", c, err)
		}
		if err := tk.Unused([]string{"tcp::80", "udp::80"}, time.Second); err != nil {
			t.Fatalf("%+v: unused: %v", c, err)
		}
		if err := tk.Ready(time.Second); err != nil {
			t.Fatalf("%+v: ready: %v", c, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("%+v: give: %v", c, err)
		}
		want := c.give&c.take&FeatUnused != 0
		select {
		case n := <-got:
			if !want || len(n) != 2 || n[0] != "tcp::80" || n[1] != "udp::80" {
				t.Fatalf("%+v: unused %v (want %v)", c, n, want)
			}
		default:
			if want {
				t.Fatalf("%+v: unused not delivered", c)
			}
		}
		tk.Close()
		r.Close()
		w.Close()
	}
}
