package tlscarrier

import (
	"bytes"
	"net"
	"testing"
	"time"
)

// Frames coalesced with AppendFrame/WriteRaw read back one by one, interleaved
// with a padded frame, through both ReadFrame and ReadFrameReuse.
func TestAppendFrameRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	w, r := NewCarrier(a), NewCarrier(b)
	big := bytes.Repeat([]byte{7}, 3000)
	go func() {
		var batch []byte
		batch = AppendFrame(batch, 1, []byte("one"))
		batch = AppendFrame(batch, 2, nil)
		batch = AppendFrame(batch, 1, big)
		w.WriteRaw(batch)
		w.SendFramePadded(1, []byte("pad"), make([]byte, 500))
		w.WriteRaw(AppendFrame(nil, 1, []byte("last")))
	}()
	r.SetReadDeadline(time.Now().Add(2 * time.Second))
	want := []struct {
		ft byte
		p  []byte
	}{{1, []byte("one")}, {2, nil}, {1, big}, {1, []byte("pad")}, {1, []byte("last")}}
	for i, f := range want {
		read := r.ReadFrameReuse
		if i%2 == 1 {
			read = r.ReadFrame
		}
		ft, p, err := read()
		if err != nil || ft != f.ft || !bytes.Equal(p, f.p) {
			t.Fatalf("frame %d: ft=%d len=%d err=%v", i, ft, len(p), err)
		}
	}
}
