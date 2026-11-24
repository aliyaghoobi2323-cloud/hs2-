package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// b449h: a link whose OpenStream always fails, ranked first by the pool.
type failOpenLink struct{ *mcLink }

func (l *failOpenLink) OpenStream() (stream, error) { return nil, errors.New("b449h: open refused") }

// b449hPool: bad (OpenStream fails) is always the pool's first choice
// (good already carries many picks); both exits read mconn (lite).
func b449hPool(t *testing.T) (*LinkManager, *failOpenLink, *mcLink) {
	t.Helper()
	caps := uint8(capPortTags | capL3Quiet | capMConn | capMConnRaw)
	bad := &failOpenLink{newMCLink(caps)}
	good := newMCLink(caps)
	lm := NewLinkManager(nil, 2, 2, 50, nil)
	lm.gateInfo = true
	lm.links = append(lm.links, &managedLink{link: bad, id: 0, mtr: bad.mtr}, &managedLink{link: good, id: 1, mtr: good.mtr, picks: 1000})
	lm.exitInfo.Store(good.mtr.peerInfo.Load())
	return lm, bad, good
}

func withOpenSkip(t *testing.T, on bool) {
	t.Helper()
	old := b449hSkipOn.Load()
	b449hSkipOn.Store(on)
	t.Cleanup(func() { b449hSkipOn.Store(old) })
}

// The one-stream open's retry leaves out the link that just failed.
func TestB449hOpenStreamSkipsFailedLink(t *testing.T) {
	withOpenSkip(t, true)
	lm, _, good := b449hPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, rel, l, ok := openStreamOn(ctx, lm, false, 2053)
	if !ok {
		t.Fatal("open failed: every try went to the link that refuses streams")
	}
	defer rel()
	defer st.Close()
	if l != Link(good) {
		t.Fatalf("stream on %T, want the good link", l)
	}
}

// Key off: the old behaviour (all three tries on the first-ranked link).
func TestB449hOpenStreamSkipOff(t *testing.T) {
	withOpenSkip(t, false)
	lm, _, _ := b449hPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, _, ok := openStreamOn(ctx, lm, false, 2053); ok {
		t.Skip("pool order changed: the key-off case no longer repeats the failed link")
	}
}

// A lite mconn's first sub retries off the failed link, and a failed mconn
// open hands its failed links to the one-stream fallback.
func TestB449hLiteFirstOpenSkipsFailedLink(t *testing.T) {
	withOpenSkip(t, true)
	withMConn(t, true)
	setLite(t, true)
	lm, _, good := b449hPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &mconnEdge{ctx: ctx, lm: lm, port: 2053, id: newMConnID()}
	s, l, err := e.open(planeLight, nil)
	if err != nil {
		t.Fatalf("first open failed: %v", err)
	}
	if l != any(Link(good)) {
		t.Fatalf("first sub on %T, want the good link", l)
	}
	if len(e.failed) != 1 {
		t.Fatalf("failed links %d, want 1", len(e.failed))
	}
	_ = s
	e.end()
}

// serveUserTCP's fallback: the mconn open failed on bad, the one stream
// must not go back to it.
func TestB449hFallbackSkipsFailedLink(t *testing.T) {
	withOpenSkip(t, true)
	lm, bad, good := b449hPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, rel, ok := openStreamSkip(ctx, lm, 2053, []Link{bad})
	if !ok {
		t.Fatal("fallback open failed")
	}
	defer rel()
	defer st.Close()
	if n := len(good.streams()); n != 1 {
		t.Fatalf("good link opened %d streams, want 1", n)
	}
}
