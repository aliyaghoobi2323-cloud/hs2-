package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tSub is a sub over one end of a net.Pipe whose writes can be held back
// (a link in loss recovery) and whose written bytes are counted.
type tSub struct {
	net.Conn
	gate   atomic.Pointer[chan struct{}]
	rate   atomic.Int64 // bytes/s this end's writes are held to (0: none)
	turn   atomic.Int64 // ns every write waits first (a smux turn behind other streams)
	wrote  atomic.Int64
	raw    bool        // the first sub of a raw pair (mconnRaw)
	drop   atomic.Bool // writes "succeed" and vanish (a link's buffers lost at its reset)
	failW  atomic.Bool // the next write fails, nothing of it delivered, and the link dies
	closed atomic.Bool
	dead   chan struct{}
	once   sync.Once
}

func newTSub(c net.Conn) *tSub { return &tSub{Conn: c, dead: make(chan struct{})} }

func (t *tSub) mconnRaw() bool { return t.raw }

func (t *tSub) Write(p []byte) (int, error) {
	if g := t.gate.Load(); g != nil {
		select {
		case <-*g:
		case <-t.dead:
			return 0, io.ErrClosedPipe
		}
	}
	if d := t.turn.Load(); d > 0 {
		time.Sleep(time.Duration(d))
	}
	if r := t.rate.Load(); r > 0 {
		time.Sleep(time.Duration(int64(len(p)) * int64(time.Second) / r))
	}
	if t.drop.Load() {
		return len(p), nil
	}
	if t.failW.Load() {
		t.Close()
		return 0, io.ErrClosedPipe
	}
	n, err := t.Conn.Write(p)
	t.wrote.Add(int64(n))
	return n, err
}

func (t *tSub) Close() error {
	t.once.Do(func() {
		t.closed.Store(true)
		close(t.dead)
	})
	return t.Conn.Close()
}

// stall holds this end's writes for d.
func (t *tSub) stall(d time.Duration) {
	g := make(chan struct{})
	t.gate.Store(&g)
	time.AfterFunc(d, func() {
		t.gate.Store(nil)
		close(g)
	})
}

type tLink struct {
	plane      mPlane
	key        int
	avoid      []any
	iran, exit *tSub
}

// tNet connects an Iran mConn to an exit mConn: every open is a fresh pipe
// whose far end is attached to the exit.
type tNet struct {
	exit  *mConn
	raw   bool         // the first sub is raw (mconn_raw.go)
	fail  atomic.Bool  // later opens fail
	hook  func(*tLink) // called on each new link before the exit sees it
	mu    sync.Mutex
	links []*tLink
}

func (n *tNet) open(p mPlane, avoid []any) (mSub, any, error) {
	if n.fail.Load() {
		return nil, nil, errMConnNoLink
	}
	a, b := net.Pipe()
	l := &tLink{plane: p, avoid: append([]any(nil), avoid...), iran: newTSub(a), exit: newTSub(b)}
	n.mu.Lock()
	l.key = len(n.links)
	n.links = append(n.links, l)
	n.mu.Unlock()
	if n.hook != nil {
		n.hook(l)
	}
	role := byte(1)
	if l.key == 0 {
		role = 0
		if n.raw {
			role = mconnRoleFirstRaw
			l.iran.raw = true
		}
	}
	n.exit.attach(l.exit, role)
	return l.iran, l.key, nil
}

func (n *tNet) link(i int) *tLink {
	n.mu.Lock()
	defer n.mu.Unlock()
	if i < len(n.links) {
		return n.links[i]
	}
	return nil
}

func (n *tNet) count(p mPlane) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	k := 0
	for _, l := range n.links {
		if l.plane == p {
			k++
		}
	}
	return k
}

func mWaitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func liveSubs(m *mConn) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subs)
}

func modeOf(m *mConn) mPlane {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode
}

// lightOnly turns off heavy detection on m (it stays in light mode).
func lightOnly(m *mConn) {
	m.mu.Lock()
	m.heavy = true
	m.mu.Unlock()
}

// newTPair returns a connected Iran/exit pair with both light subs up.
func newTPair(t *testing.T, light bool) (*tNet, *mConn, *mConn) {
	t.Helper()
	setLite(t, false) // v3 behaviour whatever HS2_MCONN says
	n := &tNet{exit: newMConnExit(7)}
	iran, err := newMConnIran(7, n.open)
	if err != nil {
		t.Fatal(err)
	}
	if light {
		lightOnly(iran)
		lightOnly(n.exit)
	}
	mWaitFor(t, "two light subs", 2*time.Second, func() bool { return liveSubs(iran) == 2 && liveSubs(n.exit) == 2 })
	return n, iran, n.exit
}

func closeAndWait(t *testing.T, ms ...*mConn) {
	t.Helper()
	for _, m := range ms {
		m.Close()
	}
	for _, m := range ms {
		select {
		case <-m.GetDieCh():
		case <-time.After(mCloseLinger + 2*time.Second):
			t.Fatal("mconn did not end after Close")
		}
	}
}

// noLeak fails if goroutines started since before are still running.
func noLeak(t *testing.T, before int) {
	t.Helper()
	end := time.Now().Add(mRSTWait + 4*time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(end) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines: %d > %d\n%s", runtime.NumGoroutine(), before, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func randBytes(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := 0; i+8 <= n; i += 8 {
		binary.LittleEndian.PutUint64(b[i:], r.Uint64())
	}
	return b
}

// writeRandomly writes data in random-sized pieces, then half-closes.
func writeRandomly(m *mConn, data []byte, seed uint64, maxPiece int) error {
	r := rand.New(rand.NewPCG(seed, 1))
	for len(data) > 0 {
		k := min(len(data), 1+r.IntN(maxPiece))
		if _, err := m.Write(data[:k]); err != nil {
			return err
		}
		data = data[k:]
	}
	return m.CloseWrite()
}

// readAllCheck reads to EOF and compares with want.
func readAllCheck(m *mConn, want []byte) error {
	got, err := io.ReadAll(m)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		i := 0
		for i < len(got) && i < len(want) && got[i] == want[i] {
			i++
		}
		return errors.New("data differs at byte " + itoa(i) + " (got " + itoa(len(got)) + " of " + itoa(len(want)) + ")")
	}
	return nil
}

func itoa(i int) string { return strconv.Itoa(i) }

// transfer runs both directions at once and checks every byte.
func transfer(t *testing.T, iran, exit *mConn, up, down int) {
	t.Helper()
	upData, downData := randBytes(up, 1), randBytes(down, 2)
	errs := make(chan error, 4)
	go func() { errs <- writeRandomly(iran, upData, 3, 64<<10) }()
	go func() { errs <- writeRandomly(exit, downData, 4, 64<<10) }()
	go func() { errs <- readAllCheck(exit, upData) }()
	go func() { errs <- readAllCheck(iran, downData) }()
	for i := 0; i < 4; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("transfer timed out")
		}
	}
}

func TestMConnTransferLight(t *testing.T) {
	before := runtime.NumGoroutine()
	n, iran, exit := newTPair(t, true)
	transfer(t, iran, exit, 3<<20, 3<<20)
	if modeOf(iran) != planeLight || modeOf(exit) != planeLight || n.count(planeBulk) != 0 {
		t.Fatal("left light mode")
	}
	// A fast transfer is over mDupMaxRate: one copy of each chunk, all on the
	// primary sub (never spread over two links); the other sub carries only
	// the first chunks, duplicated before the rate was known (the stall test
	// checks the copies of a slow one).
	for _, end := range []string{"iran", "exit"} {
		a, b := n.link(0).iran.wrote.Load(), n.link(1).iran.wrote.Load()
		if end == "exit" {
			a, b = n.link(0).exit.wrote.Load(), n.link(1).exit.wrote.Load()
		}
		if max(a, b) < 3<<20 || min(a, b) > 256<<10 {
			t.Fatalf("%s light subs carried %d and %d bytes; want one sub with all of it", end, a, b)
		}
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// Queue rule: a sender that outruns slow light links (3.2 Mbit/s each, under
// mHeavyRate) builds a standing queue and goes heavy — by the queue, not the
// rate — and moves to a bulk sub. The source is paced at ~4.3 Mbit/s (under
// mFastStartBytes a window too) so neither of the other rules fires first.
func TestMConnHeavyByQueue(t *testing.T) {
	before := runtime.NumGoroutine()
	byQueue, byRate, byFast := obs.mconnHeavyQueue.Load(), obs.mconnHeavyRate.Load(), obs.mconnHeavyFast.Load()
	n, iran, exit := newTPair(t, false)
	n.link(0).iran.rate.Store(400 << 10)
	n.link(1).iran.rate.Store(400 << 10)
	data := randBytes(3<<20, 11)
	errs := make(chan error, 2)
	go func() {
		d := data
		for len(d) > 0 {
			k := min(len(d), 16<<10)
			if _, err := iran.Write(d[:k]); err != nil {
				errs <- err
				return
			}
			d = d[k:]
			if modeOf(iran) == planeLight {
				time.Sleep(30 * time.Millisecond)
			}
		}
		errs <- iran.CloseWrite()
	}()
	go func() { errs <- readAllCheck(exit, data) }()
	mWaitFor(t, "bulk mode", 5*time.Second, func() bool { return modeOf(iran) == planeBulk })
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("transfer timed out")
		}
	}
	if obs.mconnHeavyQueue.Load()-byQueue != 1 || obs.mconnHeavyRate.Load() != byRate || obs.mconnHeavyFast.Load() != byFast {
		t.Fatalf("heavy by queue +%d, by rate +%d, by fast start +%d; want 1, 0, 0",
			obs.mconnHeavyQueue.Load()-byQueue, obs.mconnHeavyRate.Load()-byRate, obs.mconnHeavyFast.Load()-byFast)
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// A steady 2 Mbit/s stream on fast subs (a call) never goes heavy, and once
// over mDupMaxRate every chunk rides one sub, the primary.
func TestMConnSteadyStreamOneSub(t *testing.T) {
	before := runtime.NumGoroutine()
	heavies := obs.mconnHeavyQueue.Load() + obs.mconnHeavyRate.Load() + obs.mconnHeavyFast.Load()
	switches := obs.mconnPrimarySwitch.Load()
	n, iran, exit := newTPair(t, false)
	const writes, piece = 24, 16 << 10 // 16 KiB every 64 ms: 2 Mbit/s
	data := randBytes(writes*piece, 12)
	errs := make(chan error, 2)
	go func() {
		for i := 0; i < writes; i++ {
			if _, err := iran.Write(data[i*piece : (i+1)*piece]); err != nil {
				errs <- err
				return
			}
			time.Sleep(64 * time.Millisecond)
		}
		errs <- iran.CloseWrite()
	}()
	go func() { errs <- readAllCheck(exit, data) }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if modeOf(iran) == planeBulk || obs.mconnHeavyQueue.Load()+obs.mconnHeavyRate.Load()+obs.mconnHeavyFast.Load() != heavies {
		t.Fatal("a 2 Mbit/s stream went heavy")
	}
	if obs.mconnPrimarySwitch.Load() != switches {
		t.Fatal("a 2 Mbit/s stream on healthy subs switched its primary")
	}
	a, b := n.link(0).iran.wrote.Load(), n.link(1).iran.wrote.Load()
	// the first writes (up to mDupMaxRate, at Write granularity) go on both;
	// after that only the primary carries the stream
	if a < writes*piece || b > 2*mDupMaxRate+2*piece {
		t.Fatalf("light subs carried %d and %d bytes; want all on the primary", a, b)
	}
	iran.mu.Lock()
	st := iran.state
	iran.mu.Unlock()
	if st != mStateSingle {
		t.Fatalf("state %d, want single (%d)", st, mStateSingle)
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// Pings (far under mDupMaxRate) ride both subs, in full.
func TestMConnPingsOnBothSubs(t *testing.T) {
	before := runtime.NumGoroutine()
	n, iran, exit := newTPair(t, false)
	var w, l atomic.Int64
	var wg sync.WaitGroup
	pingPong(t, iran, exit, 40, &w, &l, &wg)
	wg.Wait()
	mWaitFor(t, "both copies", 2*time.Second, func() bool {
		a, b := n.link(0).iran.wrote.Load(), n.link(1).iran.wrote.Load()
		return a == b && a >= 40*(64+mDataHdr)
	})
	if modeOf(iran) != planeLight {
		t.Fatal("pings left light mode")
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

func TestMConnTransferSwitching(t *testing.T) {
	before := runtime.NumGoroutine()
	n, iran, exit := newTPair(t, false)
	transfer(t, iran, exit, 4<<20, 4<<20)
	if n.count(planeBulk) != 1 || modeOf(iran) != planeBulk || modeOf(exit) != planeBulk {
		t.Fatalf("bulk links %d, modes %v %v", n.count(planeBulk), modeOf(iran), modeOf(exit))
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// pinger writes 64-byte pings (send time, sequence) every 20 ms and reports
// the longest Write; the reader reports the longest delivery delay.
func pingPong(t *testing.T, from, to *mConn, count int, maxWrite, maxLat *atomic.Int64, wg *sync.WaitGroup) {
	wg.Add(2)
	go func() {
		defer wg.Done()
		var p [64]byte
		for i := 0; i < count; i++ {
			binary.BigEndian.PutUint64(p[0:], uint64(time.Now().UnixNano()))
			binary.BigEndian.PutUint64(p[8:], uint64(i))
			t0 := time.Now()
			if _, err := from.Write(p[:]); err != nil {
				t.Error(err)
				return
			}
			if d := int64(time.Since(t0)); d > maxWrite.Load() {
				maxWrite.Store(d)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	go func() {
		defer wg.Done()
		var p [64]byte
		for i := 0; i < count; i++ {
			if _, err := io.ReadFull(to, p[:]); err != nil {
				t.Error(err)
				return
			}
			lat := time.Now().UnixNano() - int64(binary.BigEndian.Uint64(p[0:]))
			if seq := binary.BigEndian.Uint64(p[8:]); seq != uint64(i) {
				t.Errorf("ping %d arrived as %d", i, seq)
				return
			}
			if lat > maxLat.Load() {
				maxLat.Store(lat)
			}
		}
	}()
}

func TestMConnLightStall(t *testing.T) {
	before := runtime.NumGoroutine()
	n, iran, exit := newTPair(t, true)
	// One sub of each direction is held for 500 ms (loss recovery on its
	// link); the pings must ride the other one.
	n.link(0).iran.stall(500 * time.Millisecond)
	n.link(1).exit.stall(500 * time.Millisecond)
	var w1, l1, w2, l2 atomic.Int64
	var wg sync.WaitGroup
	pingPong(t, iran, exit, 40, &w1, &l1, &wg)
	pingPong(t, exit, iran, 40, &w2, &l2, &wg)
	wg.Wait()
	t.Logf("up: write max %v, delay max %v; down: write max %v, delay max %v",
		time.Duration(w1.Load()), time.Duration(l1.Load()), time.Duration(w2.Load()), time.Duration(l2.Load()))
	if l1.Load() > int64(50*time.Millisecond) || l2.Load() > int64(50*time.Millisecond) {
		t.Fatal("a ping waited behind the stalled sub")
	}
	if w1.Load() > int64(20*time.Millisecond) || w2.Load() > int64(20*time.Millisecond) {
		t.Fatal("Write waited behind the stalled sub")
	}
	// Once released, the stalled subs deliver their copies too (dropped as
	// duplicates) and both carry every ping.
	mWaitFor(t, "stalled copies", 2*time.Second, func() bool {
		return n.link(0).iran.wrote.Load() == n.link(1).iran.wrote.Load() &&
			n.link(0).exit.wrote.Load() == n.link(1).exit.wrote.Load()
	})
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

func TestMConnSubDeath(t *testing.T) {
	before := runtime.NumGoroutine()
	survived := obs.mconnSubSurvived.Load()
	n, iran, exit := newTPair(t, true)
	// under mDupMaxRate (~25 KB/s), so every chunk goes on both subs
	// (the second light sub joins after the first bytes went out on the
	// first alone, which counts as a hole for mHoleSafe; the link dies once
	// that is past, as a link would mid-session)
	up, down := randBytes(200<<10, 5), randBytes(200<<10, 6)
	slowWrite := func(m *mConn, data []byte) error {
		for len(data) > 0 {
			k := min(len(data), 256)
			if _, err := m.Write(data[:k]); err != nil {
				return err
			}
			data = data[k:]
			time.Sleep(10 * time.Millisecond)
		}
		return m.CloseWrite()
	}
	errs := make(chan error, 4)
	go func() { errs <- slowWrite(iran, up) }()
	go func() { errs <- slowWrite(exit, down) }()
	go func() { errs <- readAllCheck(exit, up) }()
	go func() { errs <- readAllCheck(iran, down) }()
	time.Sleep(mHoleSafe + 500*time.Millisecond)
	l0 := n.link(0)
	l0.iran.Close()
	l0.exit.Close()
	mWaitFor(t, "a replacement light sub", 3*time.Second, func() bool { return n.count(planeLight) == 3 })
	if l2 := n.link(2); len(l2.avoid) == 0 || l2.avoid[0] != 1 {
		t.Fatalf("replacement avoided %v, want the live link 1 first", l2.avoid)
	}
	for i := 0; i < 4; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if obs.mconnSubSurvived.Load() == survived {
		t.Fatal("sub death not counted")
	}
	select {
	case <-iran.GetDieCh():
		t.Fatal("mconn died with its sub")
	default:
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// A sub that dies holding the only copy of some bytes resets the connection
// instead of leaving the peer waiting on a gap.
func TestMConnSubDeathLost(t *testing.T) {
	before := runtime.NumGoroutine()
	n, iran, exit := newTPair(t, true)
	n.link(1).iran.stall(time.Hour) // its queue fills; it misses copies
	data := randBytes(1<<20, 7)
	if _, err := iran.Write(data); err != nil {
		t.Fatal(err)
	}
	n.link(0).iran.Close()
	for _, m := range []*mConn{iran, exit} {
		select {
		case <-m.GetDieCh():
		case <-time.After(3 * time.Second):
			t.Fatal("mconn kept going without the bytes")
		}
	}
	if _, err := io.ReadAll(exit); err == nil {
		t.Fatal("exit read a clean EOF of a cut stream")
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// bulkCheck: after the switch and the retirement, only the bulk sub carries
// data and the light subs are closed on both ends.
func bulkCheck(t *testing.T, n *tNet, iran, exit *mConn) {
	t.Helper()
	mWaitFor(t, "bulk mode on both sides", 3*time.Second, func() bool {
		return modeOf(iran) == planeBulk && modeOf(exit) == planeBulk
	})
	if n.count(planeBulk) != 1 {
		t.Fatalf("%d bulk opens", n.count(planeBulk))
	}
	mWaitFor(t, "light subs retired", mRetireAfter+3*time.Second, func() bool {
		for i := 0; i < 2; i++ {
			if l := n.link(i); !l.iran.closed.Load() || !l.exit.closed.Load() {
				return false
			}
		}
		return liveSubs(iran) == 1 && liveSubs(exit) == 1
	})
	var lw [2][2]int64
	for i := 0; i < 2; i++ {
		lw[i] = [2]int64{n.link(i).iran.wrote.Load(), n.link(i).exit.wrote.Load()}
	}
	var bl *tLink
	for i := 0; n.link(i) != nil; i++ {
		if n.link(i).plane == planeBulk {
			bl = n.link(i)
		}
	}
	bi, be := bl.iran.wrote.Load(), bl.exit.wrote.Load()
	// More data both ways, then the half-closes.
	transfer(t, iran, exit, 1<<20, 1<<20)
	for i := 0; i < 2; i++ {
		if w := [2]int64{n.link(i).iran.wrote.Load(), n.link(i).exit.wrote.Load()}; w != lw[i] {
			t.Fatalf("light link %d carried data after retirement", i)
		}
	}
	if bl.iran.wrote.Load()-bi < 1<<20 || bl.exit.wrote.Load()-be < 1<<20 {
		t.Fatal("bulk sub did not carry the data")
	}
	select {
	case <-iran.GetDieCh():
		t.Fatal("mconn died")
	default:
	}
}

// One-way heavy transfer from `from` while the other side keeps a little
// request traffic going; checks every byte.
func heavyOneWay(t *testing.T, from, to *mConn, size int) {
	t.Helper()
	data := randBytes(size, 9)
	errs := make(chan error, 2)
	go func() {
		d := data
		for len(d) > 0 {
			k := min(len(d), 32<<10)
			if _, err := from.Write(d[:k]); err != nil {
				errs <- err
				return
			}
			d = d[k:]
		}
		errs <- nil
	}()
	go func() {
		got := make([]byte, size)
		if _, err := io.ReadFull(to, got); err != nil {
			errs <- err
			return
		}
		if !bytes.Equal(got, data) {
			errs <- errors.New("heavy data differs")
			return
		}
		errs <- nil
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("heavy transfer timed out")
		}
	}
}

func TestMConnBulkDownload(t *testing.T) {
	before := runtime.NumGoroutine()
	bulks := obs.mconnBulk.Load()
	n, iran, exit := newTPair(t, false)
	heavyOneWay(t, exit, iran, 8<<20) // WANTBULK → Iran opens bulk → BULK → echo
	bulkCheck(t, n, iran, exit)
	if obs.mconnBulk.Load()-bulks != 2 {
		t.Fatalf("switches counted %d, want 2 (one per side)", obs.mconnBulk.Load()-bulks)
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

func TestMConnBulkUpload(t *testing.T) {
	before := runtime.NumGoroutine()
	n, iran, exit := newTPair(t, false)
	heavyOneWay(t, iran, exit, 8<<20)
	bulkCheck(t, n, iran, exit)
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

func TestMConnHalfClose(t *testing.T) {
	before := runtime.NumGoroutine()
	_, iran, exit := newTPair(t, true)
	iran.Write([]byte("hello"))
	iran.CloseWrite()
	if _, err := iran.Write([]byte("x")); err == nil {
		t.Fatal("Write after CloseWrite succeeded")
	}
	got, err := io.ReadAll(exit)
	if err != nil || string(got) != "hello" {
		t.Fatalf("exit read %q, %v", got, err)
	}
	// The other direction still flows.
	exit.Write([]byte("world"))
	var p [5]byte
	if _, err := io.ReadFull(iran, p[:]); err != nil || string(p[:]) != "world" {
		t.Fatalf("iran read %q, %v", p[:], err)
	}
	exit.CloseWrite()
	if n, err := iran.Read(p[:]); n != 0 || err != io.EOF {
		t.Fatalf("iran read %d, %v after FIN; want EOF", n, err)
	}
	select {
	case <-iran.GetDieCh():
		t.Fatal("half-closed both ways but not closed: should still be alive")
	default:
	}
	closeAndWait(t, iran, exit)
	if _, err := iran.Write([]byte("x")); err == nil {
		t.Fatal("Write after Close succeeded")
	}
	if _, err := iran.Read(p[:]); err == nil {
		t.Fatal("Read after Close succeeded")
	}
	noLeak(t, before)
}

func TestMConnReset(t *testing.T) {
	before := runtime.NumGoroutine()
	_, iran, exit := newTPair(t, true)
	exit.Write([]byte("partial"))
	iran.abort()
	select {
	case <-exit.GetDieCh():
	case <-time.After(2 * time.Second):
		t.Fatal("RST did not end the exit")
	}
	var p [7]byte
	io.ReadFull(iran, p[:]) // bytes that arrived before the reset may be read
	if _, err := io.ReadAll(exit); err != errMConnReset {
		t.Fatalf("exit read error %v, want reset", err)
	}
	if _, err := exit.Write([]byte("x")); err == nil {
		t.Fatal("Write after reset succeeded")
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

func TestMConnClose(t *testing.T) {
	before := runtime.NumGoroutine()
	_, iran, exit := newTPair(t, true)
	data := randBytes(200<<10, 11)
	iran.Write(data)
	iran.Close() // the queued bytes and FIN still go out
	if err := readAllCheck(exit, data); err != nil {
		t.Fatal(err)
	}
	select {
	case <-iran.GetDieCh():
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not end the mconn once flushed")
	}
	if _, err := exit.Write([]byte("late")); err != nil {
		// the exit may already have seen its subs close; either is fine
		t.Log(err)
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// relayStream over a pair of mConns: the user's bytes flow both ways and the
// whole relay winds down when the client closes.
func TestMConnRelayStream(t *testing.T) {
	before := runtime.NumGoroutine()
	_, iran, exit := newTPair(t, false)
	cli, iranLocal := net.Pipe()
	panelLocal, panel := net.Pipe()
	done := make(chan struct{}, 2)
	go func() { relayStream(iranLocal, iran, nil); done <- struct{}{} }()
	go func() { relayStream(panelLocal, exit, nil); done <- struct{}{} }()
	go func() { // the panel echoes
		io.Copy(panel, panel)
		panel.Close()
	}()
	req := randBytes(2<<20, 12)
	go func() {
		for d := req; len(d) > 0; {
			k := min(len(d), 20<<10)
			cli.Write(d[:k])
			d = d[k:]
		}
	}()
	got := make([]byte, len(req))
	if _, err := io.ReadFull(cli, got); err != nil || !bytes.Equal(got, req) {
		t.Fatalf("echo differs (%v)", err)
	}
	cli.Close()
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(mCloseLinger + 3*time.Second):
			t.Fatal("relay did not end")
		}
	}
	for _, m := range []*mConn{iran, exit} {
		select {
		case <-m.GetDieCh():
		case <-time.After(mCloseLinger + 2*time.Second):
			t.Fatal("mconn did not end with its relay")
		}
	}
	noLeak(t, before)
}

// Reassembly: out of order, duplicated and partially overlapping pieces.
func TestMConnReassembly(t *testing.T) {
	m := newMConn(1, false, nil)
	s := &msub{m: m}
	data := randBytes(100_000, 13)
	type piece struct{ off, end int }
	ps := []piece{{61000, 62000}, {62000, 63000}, {61500, 62500}, {5000, 9000}, {0, 3000}, {7000, 12000}, {2000, 6000}, {0, 1000}, {11000, 30000},
		{40000, 60000}, {30000, 45000}, {59000, 100_000}, {50000, 51000}, {0, 16000}}
	for _, p := range ps {
		for o := p.off; o < p.end; o += mChunkMax {
			e := min(p.end, o+mChunkMax)
			c := getChunk(mRecData)
			copy(c.buf[:], data[o:e])
			m.deliver(s, uint64(o), c, e-o)
		}
	}
	m.recvFin(uint64(len(data)))
	if err := readAllCheck(m, data); err != nil {
		t.Fatal(err)
	}
	if len(m.r.ooo) != 0 || m.r.oooBytes != 0 {
		t.Fatalf("out-of-order left: %d entries, %d bytes", len(m.r.ooo), m.r.oooBytes)
	}
	m.end(errMConnClosed, false)
}

// Fast start: a connection that pulls data at full speed from its first
// moment is heavy at once (mFastStartBytes within mFastStartWindow), not
// after mHeavyQueueFor of standing queue; every byte still arrives.
func TestMConnFastStartHeavy(t *testing.T) {
	before := runtime.NumGoroutine()
	fast := obs.mconnHeavyFast.Load()
	n, iran, exit := newTPair(t, false)
	t0 := time.Now()
	data := randBytes(4<<20, 21)
	errs := make(chan error, 2)
	go func() {
		d := data
		for len(d) > 0 {
			k := min(len(d), 32<<10)
			if _, err := exit.Write(d[:k]); err != nil {
				errs <- err
				return
			}
			d = d[k:]
		}
		errs <- exit.CloseWrite()
	}()
	go func() { errs <- readAllCheck(iran, data) }()
	mWaitFor(t, "bulk mode", 2*time.Second, func() bool { return modeOf(exit) == planeBulk && modeOf(iran) == planeBulk })
	if d := time.Since(t0); d > 400*time.Millisecond {
		t.Fatalf("bulk after %v, want under 400 ms", d)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("transfer timed out")
		}
	}
	if obs.mconnHeavyFast.Load()-fast != 1 {
		t.Fatalf("heavy by fast start +%d, want 1", obs.mconnHeavyFast.Load()-fast)
	}
	if n.count(planeBulk) != 1 {
		t.Fatalf("%d bulk opens", n.count(planeBulk))
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}

// Primary switch: a 2 Mbit/s call rides one sub (over mDupMaxRate); when that
// sub's link gets stuck (writes held 500 ms) and the other light sub is
// idle, the call moves to the other sub within ~mSwitchQueueFor, and what
// the stuck sub held is sent again on it: no chunk waits for the stuck link.
func TestMConnStuckPrimarySwitch(t *testing.T) {
	before := runtime.NumGoroutine()
	switches := obs.mconnPrimarySwitch.Load()
	n, iran, exit := newTPair(t, true)
	const piece, every, count = 8 << 10, 32 * time.Millisecond, 60 // 2 Mbit/s, ~1.9 s
	data := randBytes(count*piece, 22)
	var maxLat, maxLatStall atomic.Int64
	stallAt := time.Now().Add(400 * time.Millisecond) // once over mDupMaxRate
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		stalled := false
		for i := 0; i < count; i++ {
			if !stalled && !time.Now().Before(stallAt) {
				n.link(0).iran.stall(500 * time.Millisecond)
				stalled = true
			}
			p := data[i*piece : (i+1)*piece]
			binary.BigEndian.PutUint64(p[0:], uint64(time.Now().UnixNano()))
			binary.BigEndian.PutUint64(p[8:], uint64(i))
			if _, err := iran.Write(p); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(every)
		}
	}()
	go func() {
		defer wg.Done()
		p := make([]byte, piece)
		for i := 0; i < count; i++ {
			if _, err := io.ReadFull(exit, p); err != nil {
				t.Error(err)
				return
			}
			sent := int64(binary.BigEndian.Uint64(p[0:]))
			lat := time.Now().UnixNano() - sent
			if seq := binary.BigEndian.Uint64(p[8:]); seq != uint64(i) {
				t.Errorf("chunk %d arrived as %d", i, seq)
				return
			}
			if !bytes.Equal(p[16:], data[i*piece+16:(i+1)*piece]) {
				t.Errorf("chunk %d differs", i)
				return
			}
			if lat > maxLat.Load() {
				maxLat.Store(lat)
			}
			if sent >= stallAt.UnixNano() && lat > maxLatStall.Load() {
				maxLatStall.Store(lat)
			}
		}
	}()
	wg.Wait()
	t.Logf("delay max %v (from the stall on: %v)", time.Duration(maxLat.Load()), time.Duration(maxLatStall.Load()))
	if obs.mconnPrimarySwitch.Load()-switches != 1 {
		t.Fatalf("primary switches +%d, want 1", obs.mconnPrimarySwitch.Load()-switches)
	}
	// the rule's worst case: ~mSwitchQueue of chunks (64 ms at 2 Mbit/s) plus
	// mSwitchQueueFor, re-sent on the other sub; slack for the race detector
	if d := time.Duration(maxLat.Load()); d > 300*time.Millisecond {
		t.Fatalf("a call chunk waited %v behind the stuck primary", d)
	}
	if modeOf(iran) != planeLight {
		t.Fatal("left light mode")
	}
	closeAndWait(t, iran, exit)
	noLeak(t, before)
}
