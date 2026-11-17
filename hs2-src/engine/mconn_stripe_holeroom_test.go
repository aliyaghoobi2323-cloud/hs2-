package engine

import (
	"testing"
	"time"
)

// hs133: the engine's in-package tests build subs without a link; their
// peer counts as resending (capExtMConnKeepStall) unless a test says not.
func init() { ksPeerOK = func(*msub) bool { return true } }

func setKsPeer(t *testing.T, ok bool) {
	t.Helper()
	old := ksPeerOK
	ksPeerOK = func(*msub) bool { return ok }
	t.Cleanup(func() { ksPeerOK = old })
}

func setHoleRoomCaps(t *testing.T, conn, global int) {
	t.Helper()
	oc, og := holeRoomConnMax, holeRoomGlobalMax
	holeRoomConnMax, holeRoomGlobalMax = conn, global
	t.Cleanup(func() { holeRoomConnMax, holeRoomGlobalMax = oc, og })
}

// holeRoomFill: a receiver with an old hole at offset 0; a sub delivers
// everything after it (data[ck:]) until it blocks. Returns the receiver, the
// filler's done channel and the reader's result.
func holeRoomFill(t *testing.T, id uint64, data []byte) (*mConn, chan struct{}, chan error) {
	t.Helper()
	const ck = 16 << 10
	m := newMConn(id, false, nil)
	s := &msub{m: m}
	put := func(o int) {
		c := getChunk(mRecData)
		copy(c.buf[:], data[o:o+ck])
		m.deliver(s, uint64(o), c, ck)
	}
	readErr := make(chan error, 1)
	go func() { readErr <- readAllCheck(m, data) }()
	put(ck)
	m.r.mu.Lock()
	m.r.lastProgress = time.Now().Add(-2 * time.Second) // the hole is old
	m.r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for o := 2 * ck; o < len(data); o += ck {
			put(o)
		}
	}()
	last, still := -1, 0
	for still < 5 {
		time.Sleep(40 * time.Millisecond)
		m.r.mu.Lock()
		b := m.r.oooBytes
		m.r.mu.Unlock()
		if b == last {
			still++
		} else {
			last, still = b, 0
		}
	}
	return m, done, readErr
}

// holeRoomClose: the hole chunk arrives (in order: no bound applies), the
// filler drains, the reader gets every byte; the extra is uncharged.
func holeRoomClose(t *testing.T, m *mConn, data []byte, done chan struct{}, readErr chan error) {
	t.Helper()
	const ck = 16 << 10
	c := getChunk(mRecData)
	copy(c.buf[:], data[:ck])
	m.deliver(&msub{m: m}, 0, c, ck)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the filler stayed blocked after the hole closed")
	}
	m.recvFin(uint64(len(data)))
	if err := <-readErr; err != nil {
		t.Fatal(err)
	}
	m.end(errMConnClosed, false)
}

func oooOver(m *mConn) int {
	m.r.mu.Lock()
	defer m.r.mu.Unlock()
	return m.r.oooBytes - max(m.r.oooMax, mRecvBuf)
}

// an older peer (no capExtMConnKeepStall): no hole room at all — the rel61
// receiver (bounded by the reorder bound), and the conn still completes when
// the hole fills.
func TestHoleRoomOldPeer(t *testing.T) {
	setKeepStall(t, true)
	setKsPeer(t, false)
	base := holeRoomBytes.Load()
	g0, f0, h0, o0 := keepStallStat.gapRoom.Load(), keepStallStat.gapFill.Load(), keepStallStat.holeSub.Load(), holeRoomStat.oldPeer.Load()
	data := randBytes(24<<20, 1331)
	m, done, readErr := holeRoomFill(t, 1331, data)
	if over := oooOver(m); over > 0 {
		t.Fatalf("old peer: %d bytes over the reorder bound", over)
	}
	if keepStallStat.gapRoom.Load() != g0 || keepStallStat.gapFill.Load() != f0 || keepStallStat.holeSub.Load() != h0 {
		t.Fatal("old peer: a hole room was used")
	}
	if holeRoomStat.oldPeer.Load() == o0 {
		t.Fatal("old peer: the refusal was not counted")
	}
	holeRoomClose(t, m, data, done, readErr)
	if d := holeRoomBytes.Load() - base; d != 0 {
		t.Fatalf("holeRoomBytes left %d", d)
	}
}

// a new peer: the rooms open, but never past holeRoomConnMax; past it the
// conn waits on the hole (the old behaviour) and completes when it fills.
func TestHoleRoomConnCap(t *testing.T) {
	setKeepStall(t, true)
	setKsPeer(t, true)
	const capB = 6 << 20
	setHoleRoomCaps(t, capB, 256<<20)
	base := holeRoomBytes.Load()
	c0 := holeRoomStat.capped.Load()
	data := randBytes(48<<20, 1332)
	m, done, readErr := holeRoomFill(t, 1332, data)
	over := oooOver(m)
	if over <= 0 {
		t.Fatal("new peer: no hole room opened")
	}
	if over > capB {
		t.Fatalf("over the conn cap: %d > %d", over, capB)
	}
	select {
	case <-done:
		t.Fatal("the filler never blocked: the cap did not hold")
	default:
	}
	if holeRoomStat.capped.Load() == c0 {
		t.Fatal("the cap refusal was not counted")
	}
	if got := holeRoomBytes.Load() - base; got != int64(over) {
		t.Fatalf("holeRoomBytes %d, conn over %d", got, over)
	}
	holeRoomClose(t, m, data, done, readErr)
	if d := holeRoomBytes.Load() - base; d != 0 {
		t.Fatalf("holeRoomBytes left %d after the conn", d)
	}
}

// the process cap holds over two connections; the end frees it.
func TestHoleRoomGlobalCap(t *testing.T) {
	setKeepStall(t, true)
	setKsPeer(t, true)
	const glob = 10 << 20
	base := holeRoomBytes.Load()
	setHoleRoomCaps(t, 64<<20, int(base)+glob)
	data := randBytes(40<<20, 1333)
	m1, d1, e1 := holeRoomFill(t, 13331, data)
	m2, d2, e2 := holeRoomFill(t, 13332, data)
	o1, o2 := oooOver(m1), oooOver(m2)
	if o1+o2 > glob {
		t.Fatalf("over the process cap: %d+%d > %d", o1, o2, glob)
	}
	if o1 <= 0 {
		t.Fatal("first conn: no room")
	}
	if got := holeRoomBytes.Load() - base; got != int64(o1+o2) {
		t.Fatalf("holeRoomBytes %d, conns %d+%d", got, o1, o2)
	}
	holeRoomClose(t, m1, data, d1, e1)
	holeRoomClose(t, m2, data, d2, e2)
	if d := holeRoomBytes.Load() - base; d != 0 {
		t.Fatalf("holeRoomBytes left %d", d)
	}
}

// a conn that ends with the hole open (gapCheck's reset) uncharges too.
func TestHoleRoomEndUncharges(t *testing.T) {
	setKeepStall(t, true)
	setKsPeer(t, true)
	base := holeRoomBytes.Load()
	data := randBytes(24<<20, 1334)
	m, done, readErr := holeRoomFill(t, 1334, data)
	if holeRoomBytes.Load() == base {
		t.Fatal("no room charged")
	}
	m.end(errMConnClosed, false)
	<-done
	<-readErr
	if d := holeRoomBytes.Load() - base; d != 0 {
		t.Fatalf("holeRoomBytes left %d after the end", d)
	}
}

// the peer test reads the link's (or, on the exit, the sub's counted link's)
// kindInfo ext; announced with the key on only.
func TestHoleRoomPeerCap(t *testing.T) {
	mtr := &linkMeter{}
	s := &msub{rw: exitCountedStream{nil, &exitConn{mtr: mtr}}}
	if ksPeerOKLink(s) {
		t.Fatal("no peer info: ok")
	}
	mtr.peerInfo.Store(&peerInfo{V2: true, Ext: capExtMConnStripe | capExtMConnResend})
	if ksPeerOKLink(s) {
		t.Fatal("rel61-like peer (no bit10): ok")
	}
	mtr.peerInfo.Store(&peerInfo{V2: true, Ext: capExtMConnStripe | capExtMConnKeepStall})
	if !ksPeerOKLink(s) {
		t.Fatal("keepstall peer: not ok")
	}
	if ksPeerOKLink(&msub{}) {
		t.Fatal("no link: ok")
	}
	withMConn(t, true)
	setLite(t, true)
	oldK, oldS := keepStallOn.Load(), stripeK.Load()
	defer func() { keepStallOn.Store(oldK); stripeK.Store(oldS) }()
	stripeK.Store(8)
	keepStallOn.Store(true)
	if infoExt()&capExtMConnKeepStall == 0 {
		t.Fatal("key on: bit10 not announced")
	}
	keepStallOn.Store(false)
	if infoExt()&capExtMConnKeepStall != 0 {
		t.Fatal("key off: bit10 announced")
	}
}
