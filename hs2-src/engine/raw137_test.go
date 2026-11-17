package engine

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// raw137Conn: an in-memory stream with smux's read semantics: Read returns
// what is buffered even past its deadline; with nothing buffered it waits,
// and fails with a timeout once the deadline has passed.
type raw137Conn struct {
	mu     sync.Mutex
	buf    []byte
	dl     time.Time
	closed bool
}

type raw137Timeout struct{}

func (raw137Timeout) Error() string { return "raw137: timeout" }
func (raw137Timeout) Timeout() bool { return true }

func (c *raw137Conn) push(b []byte) {
	c.mu.Lock()
	c.buf = append(c.buf, b...)
	c.mu.Unlock()
}

func (c *raw137Conn) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if len(c.buf) > 0 {
			n := copy(p, c.buf)
			c.buf = c.buf[n:]
			c.mu.Unlock()
			return n, nil
		}
		if c.closed {
			c.mu.Unlock()
			return 0, io.EOF
		}
		if !c.dl.IsZero() && !time.Now().Before(c.dl) {
			c.mu.Unlock()
			return 0, raw137Timeout{}
		}
		c.mu.Unlock()
		time.Sleep(200 * time.Microsecond)
	}
}

func (c *raw137Conn) Write(p []byte) (int, error) { return len(p), nil }
func (c *raw137Conn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}
func (c *raw137Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.dl = t
	c.mu.Unlock()
	return nil
}

// TestRawReaderNeverReadsPastLimit (raw137): the raw reader reads an unknown
// limit, then - before its Read - the boundary comes (setReadLimit), the side
// acknowledges it and the peer's first records land on the first sub. The
// Read (woken by the deadline, but with bytes buffered) must not return the
// records as raw bytes. Before the fix it returned raw + records (the 16 KB
// over-read the rc134 stress test found); now setReadLimit waits for the
// uncapped Read, so the peer's records come only after it.
func TestRawReaderNeverReadsPastLimit(t *testing.T) {
	for _, fix := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadlinefix=%v", fix), func(t *testing.T) {
			setDeadlineFix(t, fix)
			m := newMConnExitRaw(137)
			c := &raw137Conn{}
			m.attachRaw(c, nil)
			const lim = 1000
			raw := randBytes(lim, 137)
			rec := bytes.Repeat([]byte{0xEE}, 500)
			c.push(raw)
			acked := make(chan struct{})
			var once sync.Once
			h := func(mm *mConn) {
				if mm != m {
					return
				}
				once.Do(func() {
					go func() {
						m.setReadLimit(lim) // onUpgrade: before UPACK / our UPGRADE
						c.push(rec)         // the peer's first records on the first sub
						close(acked)
					}()
					select { // without the fix the ack is out before our Read
					case <-acked:
					case <-time.After(100 * time.Millisecond):
					}
				})
			}
			rawReadTestHook.Store(&h)
			defer rawReadTestHook.Store(nil)
			p := make([]byte, 16<<10)
			n, err, ok := m.readRaw(p)
			if err != nil || !ok {
				t.Fatalf("readRaw: %d %v %v", n, err, ok)
			}
			if n > lim {
				t.Fatalf("over-read: the raw reader returned %d bytes, limit %d (%d record bytes taken as raw)", n, lim, n-lim)
			}
			if !bytes.Equal(p[:n], raw[:n]) {
				t.Fatal("raw bytes differ")
			}
			<-acked
			if got := m.rawRecvd.Load(); got > lim {
				t.Fatalf("rawRecvd %d > limit %d", got, lim)
			}
			m.rawR.Store(false) // leave the first sub alone (no records loop)
			c.Close()
		})
	}
}

// raw137Upgraded: pairs that switched both ways and passed the checksums.
var raw137Upgraded atomic.Int64

// TestMConnRawSwitchChecksumStress (rc134, ported to raw137): 6 parallel
// pairs, 6 runs each: bytes both ways while Iran upgrades a raw connection,
// sha256-checked across the raw->records switch, then again on records.
// Opt-in: HS2_RC134_STRESS=1 (about 10 s).
func TestMConnRawSwitchChecksumStress(t *testing.T) {
	if os.Getenv("HS2_RC134_STRESS") != "1" {
		t.Skip("rc134 stress: set HS2_RC134_STRESS=1")
	}
	for _, fast := range []bool{false, true} {
		name := "dlfast-off"
		if fast {
			name = "dlfast-on"
		}
		t.Run(name, func(t *testing.T) {
			setLite(t, true)
			setDeadlineFix(t, true)
			raw137DlFast(t, fast)
			resetLiteBudget()
			var wg sync.WaitGroup
			errs := make(chan error, 64)
			for p := 0; p < 6; p++ {
				wg.Add(1)
				go func(p int) {
					defer wg.Done()
					for run := 0; run < 6; run++ {
						if err := raw137Pair(t, uint64(1000*p+10*run), fast); err != nil {
							errs <- err
							return
						}
					}
				}(p)
			}
			wg.Wait()
			close(errs)
			t.Logf("pairs upgraded and checked after the switch: %d of 36", raw137Upgraded.Swap(0))
			for err := range errs {
				t.Error(err)
			}
		})
	}
}

func raw137Pair(t *testing.T, seed uint64, fast bool) error {
	n := &smuxNet{t: t, exit: newMConnExitRaw(77 + seed)}
	defer n.close()
	iran, err := newMConnIran(77+seed, n.open)
	if err != nil {
		return err
	}
	exit := n.exit
	defer func() {
		iran.Close()
		exit.Close()
	}()
	up := randBytes(96<<10, seed+1)
	down := randBytes(512<<10, seed+2)
	sum := func(b []byte) [32]byte { return sha256.Sum256(b) }
	read := func(m *mConn, want []byte) error {
		got := make([]byte, len(want))
		if _, err := io.ReadFull(m, got); err != nil {
			return err
		}
		if sum(got) != sum(want) {
			i := 0
			for got[i] == want[i] {
				i++
			}
			j := len(got) - 1
			for got[j] == want[j] {
				j--
			}
			return fmt.Errorf("seed %d fast %v: checksum mismatch, %d bytes, differs in [%d,%d], iran %s, exit %s", seed, fast, len(want), i, j, rawStateOf(iran), rawStateOf(exit))
		}
		return nil
	}
	res := make(chan error, 4)
	go func() { res <- xfer(iran, up, 7<<10, 0) }()
	// as relayStream on the exit (mconn_link.go: attachRaw, then the relay):
	// the exit reads and writes once the raw sub is attached. A Read before
	// attachRaw waits on reassembly and is never woken - the "stall" rc134
	// saw (exit rawR=true recvd=0), a test artifact.
	attached := func() error {
		for t0 := time.Now(); liveSubs(exit) == 0; time.Sleep(time.Millisecond) {
			if time.Since(t0) > 5*time.Second {
				return fmt.Errorf("seed %d: exit raw sub not attached", seed)
			}
		}
		return nil
	}
	go func() {
		if err := attached(); err != nil {
			res <- err
			return
		}
		res <- read(exit, up)
	}()
	go func() {
		if err := attached(); err != nil {
			res <- err
			return
		}
		if err := xfer(exit, down[:128<<10], 16<<10, 0); err != nil {
			res <- err
			return
		}
		forceUpgrade(iran) // the switch while bytes flow both ways
		res <- xfer(exit, down[128<<10:], 16<<10, time.Millisecond)
	}()
	go func() { res <- read(iran, down) }()
	for i := 0; i < 4; i++ {
		select {
		case err := <-res:
			if err != nil {
				return err
			}
		case <-time.After(20 * time.Second):
			return fmt.Errorf("seed %d fast %v: stuck: iran %s, exit %s", seed, fast, rawStateOf(iran), rawStateOf(exit))
		}
	}
	up2, down2 := randBytes(64<<10, seed+3), randBytes(256<<10, seed+4)
	go func() { res <- xfer(iran, up2, 5<<10, 0) }()
	go func() { res <- read(exit, up2) }()
	go func() { res <- xfer(exit, down2, 16<<10, 0) }()
	go func() { res <- read(iran, down2) }()
	for i := 0; i < 4; i++ {
		select {
		case err := <-res:
			if err != nil {
				return err
			}
		case <-time.After(20 * time.Second):
			return fmt.Errorf("seed %d fast %v: stuck after the switch: iran %s, exit %s", seed, fast, rawStateOf(iran), rawStateOf(exit))
		}
	}
	if rawDone(iran) && rawDone(exit) {
		raw137Upgraded.Add(1)
	}
	return nil
}
