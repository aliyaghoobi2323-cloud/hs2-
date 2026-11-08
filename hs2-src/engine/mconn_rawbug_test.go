package engine

import (
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// The raw first sub over real smux streams. net.Pipe (tNet) fails a Read
// whose deadline has passed even when bytes wait; a smux stream returns the
// waiting bytes first and times out only when it has none. The tests below
// need smux's behaviour.

type smuxSub struct {
	*smux.Stream
	raw bool
}

func (s smuxSub) mconnRaw() bool { return s.raw }

// smuxNet: like tNet, every open is a fresh smux session over a net.Pipe.
type smuxNet struct {
	t    *testing.T
	exit *mConn
	mu   sync.Mutex
	n    int
	sess []*smux.Session
}

func (n *smuxNet) open(p mPlane, avoid []any) (mSub, any, error) {
	a, b := net.Pipe()
	cs, err := smux.Client(a, nil)
	if err != nil {
		return nil, nil, err
	}
	ss, err := smux.Server(b, nil)
	if err != nil {
		return nil, nil, err
	}
	st, err := cs.OpenStream()
	if err != nil {
		return nil, nil, err
	}
	n.mu.Lock()
	key := n.n
	n.n++
	n.sess = append(n.sess, cs, ss)
	n.mu.Unlock()
	role := mconnRoleJoin
	if key == 0 {
		role = mconnRoleFirstRaw
	}
	// the SYN reaches the server once something is written; the exit side
	// is accepted in the background
	go func() {
		es, err := ss.AcceptStream()
		if err != nil {
			return
		}
		n.exit.attach(es, role)
	}()
	return smuxSub{Stream: st, raw: role == mconnRoleFirstRaw}, key, nil
}

func (n *smuxNet) close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, s := range n.sess {
		s.Close()
	}
}

// The peer's boundary arrives while the peer's raw bytes still wait unread
// in the first sub (the app is slow, or the link delivered a burst at once).
// The raw reader then reaches the boundary without its Read ever timing out,
// and the read deadline set to wake it must not be left on the first sub:
// with it, the first sub's record reader times out as soon as its buffer is
// empty, the sub "dies", and the connection is reset (lab: 2% loss, reel
// fetches and connections failing with HS2_MCONN=lite and the raw first sub).
func TestMConnRawStaleDeadlineAtSwitch(t *testing.T) {
	setLite(t, true)
	resetLiteBudget()
	before := runtime.NumGoroutine()
	for run := 0; run < 4; run++ {
		n := &smuxNet{t: t, exit: newMConnExitRaw(77)}
		iran, err := newMConnIran(77, n.open)
		if err != nil {
			t.Fatal(err)
		}
		if iran.raw1 == nil {
			t.Fatal("the first sub is not raw")
		}
		exit := n.exit
		mWaitFor(t, "exit raw sub", time.Second, func() bool { return liveSubs(exit) == 1 })
		// a request up (Iran has sent bytes: a sub joining now has a hole)
		req := randBytes(1500, 81)
		if _, err := iran.Write(req); err != nil {
			t.Fatal(err)
		}
		if err := readN(exit, req); err != nil {
			t.Fatal(err)
		}
		go io.Copy(io.Discard, exit) // the exit keeps reading
		// a burst down that Iran's app has not read yet
		burst := randBytes(256<<10, uint64(82+run))
		if err := xfer(exit, burst, 32<<10, 0); err != nil {
			t.Fatal(err)
		}
		forceUpgrade(iran)
		mWaitFor(t, "Iran knows the boundary", 3*time.Second, func() bool { return iran.rLimit.Load() >= 0 })
		// now the app reads: every raw Read finds bytes waiting
		if err := readN(iran, burst); err != nil {
			t.Fatalf("run %d: burst: %v", run, err)
		}
		// the app goes on reading (its next Read switches at the boundary)
		more := randBytes(64<<10, uint64(90+run))
		got := make(chan error, 1)
		go func() { got <- readN(iran, more) }()
		for end := time.Now().Add(3 * time.Second); !(rawDone(iran) && rawDone(exit)); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(end) {
				t.Fatalf("run %d: handshake not over: iran %s, exit %s", run, rawStateOf(iran), rawStateOf(exit))
			}
		}
		time.Sleep(300 * time.Millisecond) // the first sub's buffer stays empty a while
		select {
		case <-iran.GetDieCh():
			t.Fatalf("run %d: Iran ended after the switch: %s", run, rawStateOf(iran))
		case <-exit.GetDieCh():
			t.Fatalf("run %d: exit ended after the switch: %s", run, rawStateOf(exit))
		default:
		}
		iran.mu.Lock()
		dead := iran.raw1.dead
		iran.mu.Unlock()
		if dead {
			t.Fatalf("run %d: the first sub died after the switch: %s", run, rawStateOf(iran))
		}
		if err := xfer(exit, more, 8<<10, 0); err != nil {
			t.Fatal(err)
		}
		if err := <-got; err != nil {
			t.Fatalf("run %d: after the switch: %v", run, err)
		}
		closeAndWait(t, iran, exit)
		n.close()
	}
	noLeak(t, before)
}

func rawStateOf(m *mConn) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprintf("err=%v rawW=%v rawR=%v handshaking=%v gated=%v dead=%v lim=%d recvd=%d sent=%d subs=%d",
		m.err, m.rawW.Load(), m.rawR.Load(), m.handshaking, m.raw1.gated, m.raw1.dead, m.rLimit.Load(), m.rawRecvd.Load(), m.rawSent.Load(), len(m.subs))
}
