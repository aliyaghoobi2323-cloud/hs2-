package engine

import (
	"bytes"
	"io"
	"math/rand/v2"
	"net"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/obfs"
	"github.com/xtaci/smux"
)

// The link scheduler's own tests (linksched.go): a queued session whose
// writer the test plays (lsManual), so what the scheduler hands out next is
// seen frame by frame.

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// lsManual is a client session on a linkSched with no writer: the test
// takes each item (take) and reports it written (done).
type lsManual struct {
	t   *testing.T
	ls  *linkSched
	cli *smux.Session
	tb  []byte
}

func newLSManual(t *testing.T) *lsManual {
	t.Helper()
	a, b := net.Pipe()
	m := &lsManual{t: t, ls: newLinkSched(nopWriter{})}
	var err error
	if m.cli, err = smux.ClientQueued(a, newSmuxConfig(), m.ls); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.cli.Close(); m.ls.close(); a.Close(); b.Close() })
	return m
}

// waitN waits until the scheduler holds n items and the user u's FIFO w
// writers (u nil: any).
func (m *lsManual) waitN(n int, tag uint32, w int) {
	m.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.ls.mu.Lock()
		got, gw := m.ls.n, 0
		if u := m.ls.users[tag]; u != nil {
			gw = u.waiting()
		}
		m.ls.mu.Unlock()
		if got == n && gw == w {
			return
		}
		if time.Now().After(deadline) {
			m.m(got, gw, n, w)
		}
		time.Sleep(time.Millisecond)
	}
}

func (m *lsManual) m(got, gw, n, w int) {
	m.t.Helper()
	m.t.Fatalf("the scheduler holds %d items and %d waiting writers, want %d and %d", got, gw, n, w)
}

func (m *lsManual) take() lsItem {
	m.t.Helper()
	m.ls.mu.Lock()
	it, ok := m.ls.next(&m.tb)
	m.ls.mu.Unlock()
	if !ok {
		m.t.Fatal("the scheduler is empty")
	}
	return it
}

func (m *lsManual) done(it lsItem) {
	if it.f != nil {
		it.f.Done(it.f.Len(), nil)
	}
	m.ls.mu.Lock()
	if it.u != nil {
		it.u.mem(-it.f.Len())
	}
	m.ls.mu.Unlock()
}

// open opens a stream of user tag, playing the writer for its SYN.
func (m *lsManual) open(tag uint32) *smux.Stream {
	m.t.Helper()
	ch := make(chan *smux.Stream, 1)
	go func() {
		st, err := m.cli.OpenStreamSyn(smux.EncodeSyn(tag, 0))
		if err != nil {
			ch <- nil
			return
		}
		ch <- st
	}()
	m.waitN(1, tag, 0)
	it := m.take()
	if it.f == nil || !it.f.Control() {
		m.t.Fatal("the SYN is not the item taken")
	}
	m.done(it)
	st := <-ch
	if st == nil {
		m.t.Fatal("open failed")
	}
	return st
}

// A control frame is next whatever data is queued (§3.2, the control lane):
// a stream's FIN and a window update pass the frames of every user.
func TestAngleBCtrlAheadOfData(t *testing.T) {
	m := newLSManual(t)
	sts := make([]*smux.Stream, 4)
	for i := range sts {
		sts[i] = m.open(uint32(i + 1))
	}
	for _, st := range sts[:3] {
		go st.Write(make([]byte, 64<<10))
	}
	m.waitN(3, 0, 0)
	go sts[3].Close() // its FIN
	m.waitN(4, 0, 0)
	if it := m.take(); it.f == nil || !it.f.Fin() || it.f.Sid() != sts[3].ID() {
		t.Fatalf("the FIN is not next behind 3 data frames")
	}
	it := m.take()
	if it.f == nil || it.f.Control() {
		t.Fatal("no data frame after the FIN")
	}
	m.done(it)
}

// The stream path's bound per user (§3.2): 64 frames of a user in the
// scheduler at most; the 65th writer waits in the user's FIFO, without a
// drop, and the waiting writers go in, in the order they came, as the
// user's frames leave. Another user is not held by it.
func TestAngleBUserBound64(t *testing.T) {
	m := newLSManual(t)
	const n = 100
	sts := make([]*smux.Stream, n)
	for i := range sts {
		sts[i] = m.open(5)
	}
	other := m.open(6)
	errs := make(chan error, n+1)
	for _, st := range sts {
		go func() { _, err := st.Write(make([]byte, 2*lsLightData)); errs <- err }()
	}
	m.waitN(lsUserPending, 5, n-lsUserPending)
	go func() { _, err := other.Write(make([]byte, 100)); errs <- err }()
	m.waitN(lsUserPending+1, 5, n-lsUserPending)
	m.ls.mu.Lock()
	u := m.ls.users[5]
	var fifo []uint32
	for _, w := range u.wait[u.wHead:] {
		fifo = append(fifo, w.f.Sid())
	}
	blocked := m.ls.blocked
	m.ls.mu.Unlock()
	if blocked != n-lsUserPending {
		t.Fatalf("blocked writers %d, want %d", blocked, n-lsUserPending)
	}
	// each write is two light frames (lsLightData): 200 frames
	// of user 5 and the other user's one
	var admitted []uint32
	frames := 0
	for frames < 2*n+1 {
		m.ls.mu.Lock()
		var head uint32
		if u.waiting() > 0 {
			head = u.wait[u.wHead].f.Sid()
		}
		pending := u.pending
		m.ls.mu.Unlock()
		if pending > lsUserPending {
			t.Fatalf("user 5 has %d frames in the scheduler", pending)
		}
		it := m.take()
		m.ls.mu.Lock()
		if head != 0 && (u.waiting() == 0 || u.wait[u.wHead].f.Sid() != head) {
			admitted = append(admitted, head)
		}
		m.ls.mu.Unlock()
		m.done(it)
		frames++
		if frames < 2*n+1 {
			deadline := time.Now().Add(5 * time.Second)
			for {
				m.ls.mu.Lock()
				k := m.ls.n
				m.ls.mu.Unlock()
				if k > 0 || time.Now().After(deadline) {
					break
				}
				time.Sleep(100 * time.Microsecond)
			}
		}
	}
	for range n + 1 {
		if err := <-errs; err != nil {
			t.Fatalf("a writer failed: %v", err)
		}
	}
	// (the second frames of the streams wait too, after them)
	if len(admitted) < len(fifo) {
		t.Fatalf("%d waiting writers went in, %d waited", len(admitted), len(fifo))
	}
	for i := range fifo {
		if admitted[i] != fifo[i] {
			t.Fatalf("waiting writer %d went in out of order: stream %d, want %d", i, admitted[i], fifo[i])
		}
	}
}

// smux asks the scheduler each frame's size: lsLightData while the stream
// is light (unknown, within its first 32 KiB), the link's bulk frame
// (BulkFrameSize of its rate) once it is bulk.
func TestAngleBFrameSizes(t *testing.T) {
	m := newLSManual(t)
	st := m.open(3)
	m.ls.tick(ctrlNow(), abRate) // 10 Mbit/s: 6250 B
	go st.Write(make([]byte, 256<<10))
	var sizes []int
	for range 20 {
		m.waitN(1, 0, 0)
		it := m.take()
		sizes = append(sizes, it.f.DataLen())
		m.done(it)
		if len(sizes) == 17 { // 34 KiB queued: bulk at its next tick
			time.Sleep(classTick)
			m.ls.tick(ctrlNow(), abRate)
		}
	}
	for i, s := range sizes[:16] {
		if s != lsLightData {
			t.Fatalf("frame %d of a new stream: %d B, want %d (%v)", i, s, lsLightData, sizes)
		}
	}
	if s := sizes[len(sizes)-1]; s != BulkFrameSize(abRate) {
		t.Fatalf("a bulk stream's frame: %d B, want %d (%v)", s, BulkFrameSize(abRate), sizes)
	}
}

// tunPkt4 is an IPv4 packet of n bytes: TCP of port sport, or an ICMP echo
// request (icmp).
func tunPkt4(sport uint16, n int, icmp bool) *[]byte {
	p := make([]byte, n)
	copy(p, tcpPacket(sport, 443))
	if icmp {
		p[9] = 1
		p[20] = 8
	}
	return &p
}

// attachedTun is a link scheduler (manual) with a TUN stream attached.
func attachedTun(t *testing.T) (*lsManual, *l3Link, *l3Set) {
	m := newLSManual(t)
	st := m.open(0)
	l := newStreamL3Link(st, func() bool { return false }, nil)
	if l.ls != m.ls {
		t.Fatal("the TUN stream's link is not on the link scheduler")
	}
	set := &l3Set{}
	m.ls.attachTun(l, &set.pool, &set.drops)
	return m, l, set
}

// TUN on the link scheduler (§3.2): a flow's queue holds 64 packets and
// 20 ms, a user's 1024 (past them the head of its longest flow goes); a
// ping (ICMP echo) is light whatever it sends and goes in the latency
// lane, ahead of the bulk TUN flows and of the link's bulk streams.
func TestAngleBTunQueues(t *testing.T) {
	m, l, set := attachedTun(t)
	ls := m.ls
	st := m.open(13) // a stream of the link (for later)
	// a flow of 100 packets: 64 kept, the oldest 36 dropped
	for range 100 {
		if !ls.pushTun(l, tunPkt4(1000, 1000, false), 11, 1) {
			t.Fatal("pushTun refused")
		}
	}
	ls.mu.Lock()
	f, u := ls.tun.flows[1], ls.users[11]
	if f.len() != tunFlowPkts || ls.drops[0]+ls.drops[1] != 100-tunFlowPkts {
		t.Fatalf("flow queue %d, drops %v", f.len(), ls.drops)
	}
	// 20 flows of that user: 1024 kept in all
	ls.mu.Unlock()
	for fl := uint32(2); fl < 22; fl++ {
		for range 64 {
			ls.pushTun(l, tunPkt4(uint16(fl), 200, false), 11, fl)
		}
	}
	ls.mu.Lock()
	if u.tunPkts != tunUserPkts {
		t.Fatalf("user queue %d packets, want %d", u.tunPkts, tunUserPkts)
	}
	ls.mu.Unlock()
	if set.drops.Load() == 0 {
		t.Fatal("the l3Set's drops were not told")
	}
	// all of them stale: nothing goes, everything drops
	time.Sleep(2 * tunFlowSojourn)
	ls.mu.Lock()
	_, ok := ls.next(&m.tb)
	left := u.tunPkts
	ls.mu.Unlock()
	if ok || left != 0 {
		t.Fatalf("stale packets went out (%v) or stayed (%d)", ok, left)
	}
	// a bulk TUN flow, a bulk stream, then a ping: the ping goes first
	for range 40 {
		ls.pushTun(l, tunPkt4(3000, 1400, false), 12, 30)
	}
	time.Sleep(2 * classTick)
	ls.tick(ctrlNow(), abRate) // bulk now; what it queued is stale: 10 fresh packets
	for range 10 {
		ls.pushTun(l, tunPkt4(3000, 1400, false), 12, 30)
	}
	if it := m.take(); it.tf == nil || it.tf.id != 30 || it.pkts != 4 { // its turn queued while it was new
		t.Fatalf("the TUN flow's first turn is not next (or not a bulk frame of 4 packets: %d)", it.pkts)
	}
	go st.Write(make([]byte, 128<<10))
	for range (classNewBytes + lsLightData - 1) / lsLightData { // the stream's first 32 KiB (light), ahead of the TUN flow's bulk turn
		m.waitN(2, 0, 0)
		it := m.take()
		if it.f == nil || it.f.Sid() != st.ID() {
			t.Fatalf("not the new stream's light frame: f %v tf %v light %v", it.f != nil, it.tf != nil, it.light)
		}
		m.done(it)
	}
	m.waitN(2, 0, 0) // the TUN flow's turn and the stream's first bulk frame
	ping := tunPkt4(0, 84, true)
	ls.pushTun(l, ping, 14, 99)
	it := m.take()
	if it.tf == nil || it.tf.id != 99 || !it.light {
		t.Fatalf("the ping is not next: item tf=%v f=%v", it.tf, it.f)
	}
	if got := m.tb; len(got) != tunHdr+84 {
		t.Fatalf("the ping's frame is %d B", len(got))
	}
	// then the bulk
	if it := m.take(); it.light {
		t.Fatal("a light item after the ping")
	} else {
		m.done(it)
	}
}

// The pinned flow table (§5.1, both ends): a flow keeps the link its first
// packet took, while it lives, though a link added later wins the flow's
// rendezvous; when its link dies, it moves.
func TestAngleBTunPinned(t *testing.T) {
	var s l3Set
	l1 := &l3Link{id: 0x1234567}
	s.add(l1)
	var pins l3Pins
	// a flow l2 will win
	l2 := &l3Link{id: 0x89abcdef}
	var flow uint32
	for h := uint32(1); ; h++ {
		if mix32(h^l2.id) > mix32(h^l1.id) {
			flow = h
			break
		}
	}
	now := ctrlNow()
	if s.pinned(&pins, flow, now) != l1 {
		t.Fatal("the only link not picked")
	}
	s.add(l2)
	if s.pickFlow(flow) != l2 {
		t.Fatal("the test's flow does not move by rendezvous")
	}
	if s.pinned(&pins, flow, now) != l1 {
		t.Fatal("a pinned flow moved to a new link")
	}
	l1.dead.Store(true)
	if s.pinned(&pins, flow, now) != l2 {
		t.Fatal("a flow stayed on a dead link")
	}
	// idle past l3PinIdle: the entry goes at the next sweep
	later := now + int64(l3PinIdle+l3PinSweep)
	s.pinned(&pins, flow+1, later)
	if _, ok := pins.m[flow]; ok {
		t.Fatal("an idle pin stayed")
	}
}

// user_mem_share (§3.2): a user past its share while memory is short is at
// its cap: its next stream frame waits though it has fewer than 64 in the
// scheduler (one still goes), and the status line counts it.
func TestAngleBMemCap(t *testing.T) {
	oldShare := UserMemShare
	UserMemShare = 4 << 10
	SetTCPMemPressure(true)
	defer func() { UserMemShare = oldShare; SetTCPMemPressure(false) }()
	m := newLSManual(t)
	a, b := m.open(21), m.open(21)
	go a.Write(make([]byte, 2<<10))
	m.waitN(1, 21, 0)
	go b.Write(make([]byte, 4<<10))
	m.waitN(1, 21, 1)
	m.ls.tick(ctrlNow(), 0)
	if !regexp.MustCompile(`users at memory cap 1$`).MatchString(m.ls.line(1)) {
		t.Fatalf("the line: %s", m.ls.line(1))
	}
	if usersAtMemCap() < 1 {
		t.Fatal("usersAtMemCap misses the user")
	}
	m.done(m.take())
	m.waitN(1, 21, 0) // the waiting frame goes in as the other leaves
}

// The status line has the text of §3.2.
func TestAngleBStatusLine(t *testing.T) {
	m := newLSManual(t)
	st := m.open(1)
	go st.Write([]byte("hello"))
	m.waitN(1, 0, 0)
	m.done(m.take())
	m.ls.tick(ctrlNow(), abRate)
	re := regexp.MustCompile(`^sched link 4: lane p50/p99 ([0-9.]+/[0-9.]+|-/-) ms, bulk frame 6\.1 KiB, users [0-9]+ \(active [0-9]+\), drops light/bulk 0/0, blocked-writers 0, users at memory cap 0$`)
	if l := m.ls.line(4); !re.MatchString(l) {
		t.Fatalf("the line: %q", l)
	}
}

// writeHold: a frame's last partial record waits for the next frame's
// bytes; the records keep the sampled sizes, the bytes their order, and a
// run of small frames costs far less padding than written one by one.
func TestShapedConnHold(t *testing.T) {
	frames := make([][]byte, 200)
	var all []byte
	for i := range frames {
		frames[i] = make([]byte, 6250)
		for j := range frames[i] {
			frames[i][j] = byte(rand.IntN(256))
		}
		all = append(all, frames[i]...)
	}
	wire := func(hold bool) (*captureConn, []byte) {
		a, b := net.Pipe()
		capc := &captureConn{Conn: a}
		w := newShapedConn(capc, obfs.NewHTTPSLengthSampler())
		r := newShapedConn(b, nil)
		got := make(chan []byte)
		go func() {
			out := make([]byte, len(all))
			io.ReadFull(r, out)
			got <- out
		}()
		for i, f := range frames {
			if _, err := w.writeHold(f, hold && i < len(frames)-1); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.flushHeld(); err != nil {
			t.Fatal(err)
		}
		return capc, <-got
	}
	plain, gotPlain := wire(false)
	held, gotHeld := wire(true)
	if !bytes.Equal(gotPlain, all) || !bytes.Equal(gotHeld, all) {
		t.Fatal("the bytes changed")
	}
	sizes := map[int]bool{1400: true, 1200: true, 900: true, 600: true, 400: true, 250: true, 150: true, 80: true, 40: true}
	for _, s := range held.snapshot() {
		if !sizes[s] {
			t.Fatalf("a held record of %d B: not a sampled size", s)
		}
	}
	ep, eh := float64(len(all))/float64(plain.total), float64(len(all))/float64(held.total)
	t.Logf("payload share of the wire, 6250 B frames: %.3f one by one, %.3f held", ep, eh)
	if eh < 0.98 || eh <= ep {
		t.Fatalf("held: %.3f of the wire is payload (one by one %.3f)", eh, ep)
	}
}

// The link scheduler's writer over a real session pair, many users and
// streams both ways, with -race: every byte arrives, in order.
func TestAngleBManyUsersRace(t *testing.T) {
	r := newABRig(t, 50e6/8, time.Millisecond, true)
	defer r.close()
	var wg sync.WaitGroup
	var bad atomic.Int32
	for u := uint32(1); u <= 8; u++ {
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				st := r.open(u, 'P')
				for i := range 50 {
					var b [32]byte
					b[0], b[1] = byte(u), byte(i)
					if _, err := st.Write(b[:]); err != nil {
						bad.Add(1)
						return
					}
					if _, err := io.ReadFull(st, b[:]); err != nil || b[0] != byte(u) || b[1] != byte(i) {
						bad.Add(1)
						return
					}
				}
			}()
		}
		r.download(u, 2)
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d echo streams failed", bad.Load())
	}
	if CoreLines() == nil {
		t.Fatal("no core lines")
	}
}
