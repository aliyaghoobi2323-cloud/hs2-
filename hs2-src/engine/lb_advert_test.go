package engine

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

func TestLBAdvertWireAndValidate(t *testing.T) {
	in := []LBAdvertIP{{"10.1.0.1", 60}, {"10.1.0.2", 40}, {"2001:db8::1", 0}}
	out, err := readAdvert(bytes.NewReader(encodeAdvert(in)))
	if err != nil || !advertEqual(in, out) {
		t.Fatalf("round trip: %v %v", out, err)
	}
	if err := ValidateLBAdvert(in); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLBAdvert(nil); err != nil {
		t.Fatal("empty list must be valid (nothing announced)")
	}
	many := make([]LBAdvertIP, lbAdvertMax+1)
	for i := range many {
		many[i] = LBAdvertIP{fmt.Sprintf("10.2.0.%d", i+1), 0}
	}
	many[0].Weight = 100
	bad := []struct {
		l    []LBAdvertIP
		want string
	}{
		{[]LBAdvertIP{{"10.1.0.1", 60}, {"10.1.0.1", 40}}, "twice"},
		{[]LBAdvertIP{{"0.0.0.0", 100}}, "unicast"},
		{[]LBAdvertIP{{"224.0.0.1", 100}}, "unicast"},
		{[]LBAdvertIP{{"nope", 100}}, "unicast"},
		{[]LBAdvertIP{{"10.1.0.1", 60}, {"10.1.0.2", 30}}, "sum to 90"},
		{[]LBAdvertIP{{"10.1.0.1", 160}, {"10.1.0.2", -60}}, "out of 0..100"},
		{many, "max 32"},
	}
	for _, c := range bad {
		if err := ValidateLBAdvert(c.l); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: want %q, got %v", c.l, c.want, err)
		}
	}
	// a message claiming more than the max is refused on read
	if _, err := readAdvert(bytes.NewReader([]byte{1, lbAdvertMax + 1})); err == nil {
		t.Fatal("oversized advert accepted")
	}
}

// advSmuxPair is an edge (client) session and the exit's accepted streams.
func advSmuxPair(t *testing.T) (*smux.Session, *smux.Session) {
	t.Helper()
	a, b := net.Pipe()
	cs, err := smux.Client(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := smux.Server(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); ss.Close() })
	return cs, ss
}

type sessOpener struct{ s *smux.Session }

func (o sessOpener) OpenRawStream() (*smux.Stream, error) { return o.s.OpenStream() }

// exitServe accepts the edge's streams and serves kindLBAdvert like
// serveStream (adv nil: an exit without capLBAdvert closes the stream).
func exitServe(ss *smux.Session, adv *LBAdvert) {
	go func() {
		for {
			st, err := ss.AcceptStream()
			if err != nil {
				return
			}
			var k [1]byte
			if _, err := io.ReadFull(st, k[:]); err != nil || k[0] != kindLBAdvert || adv == nil {
				st.Close()
				continue
			}
			go adv.serve(st)
		}
	}()
}

// Reverse: the edge (iran) listens and announces; the exit (kharej) dials
// and takes the list. Direct: the exit listens and announces; the edge dials.
func TestLBAdvertExchangeBothModes(t *testing.T) {
	list := []LBAdvertIP{{"10.1.0.1", 70}, {"10.1.0.2", 30}}
	for _, mode := range []string{"reverse", "direct"} {
		t.Run(mode, func(t *testing.T) {
			got := make(chan []LBAdvertIP, 4)
			take := func(l []LBAdvertIP) { got <- l }
			var edge, exit *LBAdvert
			if mode == "reverse" {
				edge, exit = NewLBAdvert(list, nil, t.Logf), NewLBAdvert(nil, take, t.Logf)
			} else {
				edge, exit = NewLBAdvert(nil, take, t.Logf), NewLBAdvert(list, nil, t.Logf)
			}
			cs, ss := advSmuxPair(t)
			exitServe(ss, exit)
			if err := edge.exchange(sessOpener{cs}); err != nil {
				t.Fatal(err)
			}
			select {
			case l := <-got:
				if !advertEqual(l, list) {
					t.Fatalf("dialing side got %v", l)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no advert reached the dialing side")
			}
			if !edge.State().PeerHas || !exit.State().PeerHas {
				t.Fatal("both sides should know the peer reads adverts")
			}
			// the same list again: not re-applied
			edge.exchange(sessOpener{cs})
			// a reload on the listening side: the new list reaches the dialer
			nl := []LBAdvertIP{{"10.1.0.1", 20}, {"10.1.0.2", 30}, {"10.1.0.3", 50}}
			if mode == "reverse" {
				edge.SetMine(nl)
			} else {
				exit.SetMine(nl)
			}
			if err := edge.exchange(sessOpener{cs}); err != nil {
				t.Fatal(err)
			}
			select {
			case l := <-got:
				if !advertEqual(l, nl) {
					t.Fatalf("after reload got %v", l)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("reloaded advert did not reach the dialing side")
			}
			// an invalid list is rejected, the last valid one stays
			bad := []LBAdvertIP{{"10.1.0.1", 50}, {"10.1.0.1", 50}}
			if mode == "reverse" {
				edge.SetMine(bad)
			} else {
				exit.SetMine(bad)
			}
			edge.exchange(sessOpener{cs})
			dial := exit
			if mode == "direct" {
				dial = edge
			}
			time.Sleep(50 * time.Millisecond)
			st := dial.State()
			if st.Rejected == "" || !advertEqual(st.Peer, nl) || len(got) != 0 {
				t.Fatalf("invalid advert not rejected: %+v (applied %d)", st, len(got))
			}
		})
	}
}

// A peer without capLBAdvert (older hs2) closes the unknown kind: the edge
// gets an error and nothing is applied — the dialing side keeps its local
// lists (stage 5). Caps advertise capLBAdvert only with an Advert.
func TestLBAdvertOldPeer(t *testing.T) {
	cs, ss := advSmuxPair(t)
	exitServe(ss, nil)
	edge := NewLBAdvert(nil, func([]LBAdvertIP) { t.Fatal("applied without a peer advert") }, nil)
	if err := edge.exchange(sessOpener{cs}); err == nil {
		t.Fatal("an old exit must fail the exchange")
	}
	if edge.State().PeerHas {
		t.Fatal("old peer reported as reading adverts")
	}
	if (KharejConfig{}).info(4).Caps&capLBAdvert != 0 {
		t.Fatal("capLBAdvert without an Advert")
	}
	if (KharejConfig{Advert: NewLBAdvert(nil, nil, nil)}).info(4).Caps&capLBAdvert == 0 {
		t.Fatal("no capLBAdvert with an Advert")
	}
}

// SetPaths changes weights and the path set without touching a live link:
// links stay held (and counted) on kept paths; a removed path with a link
// drains (no new link, dropped once empty); a new path takes new links.
func TestLBSetPathsKeepsLinks(t *testing.T) {
	b, err := NewLB(twoPaths(50, 50), nil)
	if err != nil {
		t.Fatal(err)
	}
	// 4 live links (distinct keys): 2/2
	keys := []*int{new(int), new(int), new(int), new(int)}
	for _, k := range keys {
		i, probe := b.acquire()
		b.result(i, probe, k, nil)
	}
	if st := b.Stats(); st[0].Links != 2 || st[1].Links != 2 {
		t.Fatalf("start %+v", st)
	}
	// weight change 50/50 -> 80/20: the links stay, new ones follow the new shares
	np := twoPaths(80, 20)
	if err := b.SetPaths(np); err != nil {
		t.Fatal(err)
	}
	if st := b.Stats(); st[0].Links != 2 || st[1].Links != 2 || st[0].Weight != 80 {
		t.Fatalf("after weight change %+v", st)
	}
	for k := 0; k < 6; k++ {
		i, probe := b.acquire()
		b.result(i, probe, new(int), nil)
	}
	if st := b.Stats(); st[0].Links != 8 || st[1].Links != 2 {
		t.Fatalf("new links should follow 80/20: %+v", st)
	}
	// remove path 1, add path 3: path 1 drains with its 2 live links
	p3 := LBPath{Local: "10.0.0.1", Remote: "10.1.0.3", Addr: "10.1.0.3:443", Weight: 50}
	if err := b.SetPaths([]LBPath{np[0], p3}); err != nil {
		t.Fatal(err)
	}
	st := b.Stats()
	if len(st) != 3 || st[2].State != "draining" || st[2].Links != 2 {
		t.Fatalf("removed path should drain: %+v", st)
	}
	for k := 0; k < 10; k++ {
		i, _ := b.acquire()
		if b.Paths()[i].Gone {
			t.Fatal("a new link was placed on a removed path")
		}
		b.result(i, false, new(int), nil)
	}
	if st := b.Stats(); st[1].Links == 0 {
		t.Fatalf("the added path got no link: %+v", st)
	}
	// the draining path's links end with their users: then it is dropped
	for _, k := range keys {
		b.release(k)
	}
	for _, p := range b.Paths() {
		if p.Gone {
			t.Fatalf("drained path not dropped: %+v", b.Paths())
		}
	}
	// a cooled path gets its probe at once when the paths are set again
	b.mu.Lock()
	b.until[1] = b.now().Add(time.Minute)
	b.mu.Unlock()
	if i, _ := b.acquire(); i == 1 {
		t.Fatal("cooled path dialed before its cooldown ended")
	} else {
		b.result(i, false, new(int), nil)
	}
	b.SetPaths([]LBPath{np[0], p3})
	if i, probe := b.acquire(); i != 1 || !probe {
		t.Fatalf("re-announced cooled path not probed at once: %d %v", i, probe)
	}
	// exactly one primary is required
	if err := b.SetPaths([]LBPath{p3}); err == nil {
		t.Fatal("a path set without the primary accepted")
	}
}

// The listening side adds and removes listeners at runtime; removing one
// leaves its accepted connections open.
func TestListenerSetAddRemove(t *testing.T) {
	a, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := MergeListeners(a).(ListenerSet)
	defer m.Close()
	go func() {
		for {
			c, err := m.Accept()
			if err != nil {
				if strings.Contains(fmt.Sprint(err), "closed") && len(m.ListenIPs()) == 0 {
					return
				}
				continue
			}
			go io.Copy(c, c) // echo
		}
	}()
	_, port, _ := net.SplitHostPort(a.Addr().String())
	b, err := ListenReuse(net.JoinHostPort("127.0.0.2", port))
	if err != nil {
		t.Skip("no 127.0.0.2:", err)
	}
	m.AddListener(b)
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.2", port))
	if err != nil {
		t.Fatal("added listener not accepting:", err)
	}
	defer c.Close()
	echo := func() error {
		c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write([]byte("x")); err != nil {
			return err
		}
		var r [1]byte
		_, err := io.ReadFull(c, r[:])
		return err
	}
	if err := echo(); err != nil {
		t.Fatal(err)
	}
	if !m.RemoveListener("127.0.0.2") {
		t.Fatal("remove failed")
	}
	if _, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.2", port), time.Second); err == nil {
		t.Fatal("removed listener still accepts")
	}
	if err := echo(); err != nil {
		t.Fatal("established connection broken by the removal:", err)
	}
	if ips := m.ListenIPs(); len(ips) != 1 || ips[0] != "127.0.0.1" {
		t.Fatalf("ips %v", ips)
	}
}

// After a weight change (reload / announcement) the idle-link rebalance moves
// the shares one idle link at a time; a busy link is never closed.
func TestLBSetPathsRebalanceIdleOnly(t *testing.T) {
	b, _ := NewLB(twoPaths(50, 50), nil)
	byPath := addLinks(b, 8) // 4/4
	busy := map[*lbFakeLink]bool{}
	closed := map[*lbFakeLink]bool{}
	for i := 0; i < 2; i++ {
		for k, l := range byPath[i] {
			l := l
			busy[l] = i == 1 && k < 2 // path 1: two busy, two idle
			b.attach(l, lbHooks{retire: func() bool {
				if busy[l] {
					return false
				}
				closed[l] = true
				b.release(l)
				return true
			}})
		}
	}
	if err := b.SetPaths(twoPaths(90, 10)); err != nil {
		t.Fatal(err)
	}
	moved := 0
	for b.rebalance() {
		moved++
		i, probe := b.acquire() // the replacement goes under its share
		if i != 0 {
			t.Fatalf("replacement on path %d", i)
		}
		b.result(i, probe, &lbFakeLink{id: 100 + moved}, nil)
	}
	st := b.Stats()
	if moved != 2 || st[0].Links != 6 || st[1].Links != 2 {
		t.Fatalf("moved %d, %+v", moved, st)
	}
	for l := range closed {
		if busy[l] {
			t.Fatal("a busy link was closed")
		}
	}
}
