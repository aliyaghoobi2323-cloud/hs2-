//go:build linux

package handoff

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// b438: LBSTATE codec, size bound, fds rejected.
func TestLBStateCodec(t *testing.T) {
	for _, blob := range [][]byte{nil, []byte(`{"at":1}`), bytes.Repeat([]byte{'x'}, MaxLBState)} {
		b, err := encode(&Msg{Type: TypeLBState, Blob: blob})
		if err != nil {
			t.Fatal(err)
		}
		m, _, err := decode(b, 0)
		if err != nil || !bytes.Equal(m.Blob, blob) {
			t.Fatalf("round trip %d bytes: %v", len(blob), err)
		}
	}
	if _, err := encode(&Msg{Type: TypeLBState, Blob: make([]byte, MaxLBState+1)}); err == nil {
		t.Fatal("oversize LBSTATE encoded")
	}
	if _, _, err := decode([]byte{wireVer, TypeLBState, 0, 1, 'x'}, 1); err == nil {
		t.Fatal("fds on LBSTATE accepted")
	}
}

// b438: LBSTATE flows (after FDS and before GO) only when both Hellos carry
// FeatLBState; a mixed pair keeps today's exchange. A big state crosses the
// socket whole.
func TestGiveLBStateNegotiation(t *testing.T) {
	all := FeatListening | FeatUnused | FeatLBState
	old := FeatListening | FeatUnused
	big := bytes.Repeat([]byte{'s'}, 20000)
	for _, c := range []struct{ give, take uint32 }{{all, all}, {all, old}, {old, all}, {old, old}, {FeatLBState, FeatLBState}} {
		a, b := pair(t)
		r, _, _ := os.Pipe()
		calls := 0
		a.LBState = func() []byte { calls++; return append(big, byte('0'+calls)) }
		done := make(chan error, 1)
		go func() {
			_, err := a.GiveListeningUnused(Hello{Proto: Proto, Features: c.give}, []FD{{"tcp::80", r}}, time.Second, 2*time.Second, nil, nil)
			done <- err
		}()
		tk, err := b.Take(Hello{Proto: Proto, Features: c.take}, time.Second)
		if err != nil {
			t.Fatalf("%+v: take: %v", c, err)
		}
		if err := tk.Listening(time.Second); err != nil {
			t.Fatal(err)
		}
		if err := tk.Unused(nil, time.Second); err != nil {
			t.Fatal(err)
		}
		if err := tk.Ready(time.Second); err != nil {
			t.Fatalf("%+v: ready: %v", c, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("%+v: give: %v", c, err)
		}
		want := c.give&c.take&FeatLBState != 0
		if want != (len(tk.LBState) > 0) || want != (len(tk.LBStateGo) > 0) {
			t.Fatalf("%+v: lbstate %d/%d bytes, want %v", c, len(tk.LBState), len(tk.LBStateGo), want)
		}
		if want && (tk.LBState[len(big)] != '1' || tk.LBStateGo[len(big)] != '2' || calls != 2) {
			t.Fatalf("%+v: snapshots out of order (calls %d)", c, calls)
		}
		if f := tk.File("tcp::80"); f != nil {
			f.Close()
		}
		tk.Close()
		a.Close()
		r.Close()
	}
}
