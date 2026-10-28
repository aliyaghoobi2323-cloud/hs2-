package smux

import (
	"encoding/binary"
	"io"
	"math/rand"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// R1 red team, first pass (method step 4; docs/architecture/wip/r1/redteam/
// REPORT.fa.md): the group streams and the v3 SYN (level 2) against wrapped
// numbers, a link that dies around a move, and a peer that sends anything.

// A connection whose frame numbers wrap past 2^32 — each end numbers from 0
// and a long one sends more than 4 G frames — moves right where they wrap,
// the old link stalling 300 ms as it moves: each side reads every byte in
// order (the numbers compare as serial numbers, int32 of the difference).
func TestRedGroupSeqWrap(t *testing.T) {
	a := newGlink(t, 10*time.Millisecond, 5*time.Millisecond, 21, groupConfig)
	b := newGlink(t, 10*time.Millisecond, 0, 23, groupConfig)
	x := serveExit(t, a.exit, b.exit)
	g, err := a.edge.OpenGroup(0xABCD, 0, 91)
	if err != nil {
		t.Fatal(err)
	}
	var xg *Group
	select {
	case xg = <-x.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the exit never accepted the group")
	}
	// nothing was sent on the group yet: both ends start their numbers
	// 1000 frames short of the wrap, as if 4 G frames had gone before
	const start = ^uint32(0) - 1000
	for _, gg := range []*Group{g, xg} {
		gg.mu.Lock()
		gg.txNext, gg.rxNext = start, start
		gg.mu.Unlock()
	}
	const total = 3 << 20
	var once sync.Once
	pumps := make(chan error, 2)
	var upN, dnN atomic.Int64
	upEnd, dnEnd := make(chan error, 1), make(chan error, 1)
	go func() {
		pumps <- pump(g, 'u', total, rand.New(rand.NewSource(8)), func(off int64) {
			if off >= 1<<20 {
				once.Do(func() {
					a.up.stall(300 * time.Millisecond)
					a.down.stall(300 * time.Millisecond)
					if _, err := g.Move(b.edge, 0xABCD); err != nil {
						t.Errorf("move: %v", err)
					}
				})
			}
		})
	}()
	go func() { pumps <- pump(xg, 'd', total, rand.New(rand.NewSource(9)), nil) }()
	go func() { upEnd <- drain(xg, 'u', &upN) }()
	go func() { dnEnd <- drain(g, 'd', &dnN) }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-pumps:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("stuck: read up %d, down %d of %d", upN.Load(), dnN.Load(), total)
		}
	}
	waitFor(t, "every byte read", func() bool { return upN.Load() == total && dnN.Load() == total })
	for _, gg := range []*Group{g, xg} {
		gg.mu.Lock()
		tx, rx := gg.txNext, gg.rxNext
		gg.mu.Unlock()
		if tx > start/2 || rx > start/2 {
			t.Fatalf("the numbers did not wrap (tx %d rx %d)", tx, rx)
		}
	}
	g.Close()
	if err := <-upEnd; err != nil {
		t.Fatalf("exit's reader: %v", err)
	}
	xg.Close()
	if err := <-dnEnd; err != nil && err != io.ErrClosedPipe {
		t.Fatalf("edge's reader: %v", err)
	}
}

// A link dies around a move: the new link (BULK) just after it, with both
// directions busy, or the old one (QUIET) before the main stream was let
// go. Neither end may hang or hand on a wrong byte: each reader and writer
// returns (every byte in order, or an error) within a few seconds, and once
// both ends close, no goroutine of theirs is left.
func TestRedGroupLinkDiesAroundMove(t *testing.T) {
	for _, tc := range []struct {
		name    string
		killOld bool
	}{{"new link dies", false}, {"old link dies", true}} {
		t.Run(tc.name, func(t *testing.T) {
			base := runtime.NumGoroutine()
			a := newGlink(t, 10*time.Millisecond, 3*time.Millisecond, 31, groupConfig)
			b := newGlink(t, 5*time.Millisecond, 0, 33, groupConfig)
			x := serveExit(t, a.exit, b.exit)
			g, err := a.edge.OpenGroup(0xBEEF, 0, 92)
			if err != nil {
				t.Fatal(err)
			}
			var xg *Group
			select {
			case xg = <-x.got:
			case <-time.After(5 * time.Second):
				t.Fatal("the exit never accepted the group")
			}
			const total = 64 << 20 // more than goes in the test's time
			var once sync.Once
			kill := func(l *glink) { l.up.close(); l.down.close(); l.edge.Close(); l.exit.Close() }
			ends := make(chan string, 4)
			var upN, dnN atomic.Int64
			go func() {
				err := pump(g, 'u', total, rand.New(rand.NewSource(3)), func(off int64) {
					if off >= 512<<10 {
						once.Do(func() {
							if tc.killOld {
								a.up.stall(time.Second) // its tail never arrives
							}
							if _, err := g.Move(b.edge, 0xBEEF); err != nil {
								t.Errorf("move: %v", err)
							}
							go func() {
								time.Sleep(150 * time.Millisecond)
								if tc.killOld {
									kill(a)
								} else {
									kill(b)
								}
							}()
						})
					}
				})
				ends <- "edge writer: " + errString(err)
			}()
			go func() {
				err := pump(xg, 'd', total, rand.New(rand.NewSource(4)), nil)
				ends <- "exit writer: " + errString(err)
			}()
			go func() { ends <- "exit reader: " + errString(drain(xg, 'u', &upN)) }()
			go func() { ends <- "edge reader: " + errString(drain(g, 'd', &dnN)) }()
			// a reader that ends closes its group, as hs2's relay does: the
			// writer at its end then ends too
			deadline := time.After(15 * time.Second)
			for i := 0; i < 4; i++ {
				select {
				case e := <-ends:
					t.Logf("%s (read up %d, down %d)", e, upN.Load(), dnN.Load())
					g.Close()
					xg.Close()
				case <-deadline:
					t.Fatalf("%d of 4 ends after the link died: hung (read up %d, down %d)", i, upN.Load(), dnN.Load())
				}
			}
			kill(a)
			kill(b)
			waitFor(t, "the goroutines gone", func() bool { return runtime.NumGoroutine() <= base+8 })
		})
	}
}

func errString(err error) string {
	if err == nil {
		return "done"
	}
	return err.Error()
}

// The refusal of a bad SYN (no v3 payload, a sub-stream role with no group)
// sends its FIN from a goroutine of its own, so the reader never waits on the
// writer. A peer that sends many while it reads nothing — an authenticated
// peer only: a v2 one is refused at the handshake — leaves one goroutine per
// SYN behind the stalled writer. Logged, not failed (the peer holds the
// shared key; REPORT.fa.md).
func TestRedBadSynFINsWait(t *testing.T) {
	c1, c2 := net.Pipe()
	srv, err := Server(c2, schedConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	defer c1.Close()
	base := runtime.NumGoroutine()
	const n = 2000
	var hdr [headerSize]byte
	hdr[0], hdr[1] = 2, cmdSYN
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(hdr[4:], uint32(2*i+1))
		if _, err := c1.Write(hdr[:]); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "every SYN read", func() bool { return srv.BadSyn() == n })
	time.Sleep(50 * time.Millisecond)
	extra := runtime.NumGoroutine() - base
	t.Logf("%d bad SYNs from a peer that reads nothing: %d goroutines wait to send their FINs", n, extra)
}

// FuzzRedSessionFrames feeds a session with a frame scheduler (hs2's level 2)
// whatever an authenticated peer could send: SYNs of any payload (groups,
// roles, numbers), FINs, data frames of any length with any number, window
// updates, NOPs, and garbage; its streams are served as hs2's exit serves
// them (main streams as groups, move streams attached to their group, every
// group read). Nothing may panic, and once the peer's connection ends, the
// session and its readers end.
func FuzzRedSessionFrames(f *testing.F) {
	f.Add([]byte{0, 1, 0, 0, 0, 0, 1, 2, 2, 5, 0x41, 0x42, 1, 1})
	f.Add([]byte{0, 1, 0, 0, 0, 0, 1, 0, 3, 4, 0, 0, 0, 1, 2, 3, 0, 1, 9, 0, 0, 0, 2, 2, 3, 7})
	f.Add([]byte{5, 0xff, 0xff, 0, 2, 1, 4})
	f.Add([]byte{0, 3, 0, 0, 0, 9, 4, 0, 0, 0, 9, 0, 0, 0, 5, 2, 3, 3, 0, 0, 0, 5})
	f.Fuzz(func(t *testing.T, in []byte) {
		c1, c2 := net.Pipe()
		srv, err := Server(c2, groupConfig())
		if err != nil {
			t.Fatal(err)
		}
		served := make(chan struct{})
		go func() {
			defer close(served)
			var mu sync.Mutex
			groups := map[uint32]*Group{}
			var wg sync.WaitGroup
			defer wg.Wait()
			for {
				st, err := srv.AcceptStream()
				if err != nil {
					return
				}
				id, role, _ := st.GroupInfo()
				var rd io.ReadCloser = st
				switch {
				case id != 0 && role == RoleMove:
					mu.Lock()
					g := groups[id]
					mu.Unlock()
					if g == nil || g.Attach(st) != nil {
						st.Close()
					}
					continue
				case id != 0:
					g := AcceptGroup(st)
					if g == nil {
						st.Close()
						continue
					}
					mu.Lock()
					groups[id] = g
					mu.Unlock()
					g.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
					rd = g
				default:
					st.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					io.Copy(io.Discard, rd)
					rd.Close()
				}()
			}
		}()
		go io.Copy(io.Discard, c1) // what the session sends back
		at := 0
		next := func() byte {
			if at >= len(in) {
				at++
				return byte(at * 37)
			}
			at++
			return in[at-1]
		}
		var out []byte
		frame := func(ver, cmd byte, sid uint32, body []byte) {
			var h [headerSize]byte
			h[0], h[1] = ver, cmd
			binary.LittleEndian.PutUint16(h[2:], uint16(len(body)))
			binary.LittleEndian.PutUint32(h[4:], sid)
			out = append(append(out, h[:]...), body...)
		}
		for steps := 0; at < len(in) && steps < 64; steps++ {
			op, sid := next()%6, uint32(next()%6)
			switch op {
			case 0: // SYN: a v3 payload of some group and role, or another length
				var p [SynPayloadLen]byte
				binary.BigEndian.PutUint32(p[0:], 7)
				p[4] = next()
				binary.BigEndian.PutUint32(p[5:], uint32(next()%3))
				binary.BigEndian.PutUint32(p[9:], uint32(next())<<24|uint32(next()))
				body := p[:]
				if k := next(); k%5 == 0 {
					body = body[:k%SynPayloadLen]
				}
				frame(2, cmdSYN, sid, body)
			case 1:
				frame(2, cmdFIN, sid, nil)
			case 2: // data: a number near the stream's, then bytes
				n := int(next()) % 40
				body := make([]byte, n)
				if n >= 4 {
					binary.BigEndian.PutUint32(body, uint32(next()%8)-uint32(next()%2)*0x80000000)
				}
				frame(2, cmdPSH, sid, body)
			case 3:
				var u [szCmdUPD]byte
				binary.LittleEndian.PutUint32(u[:], uint32(next())<<12)
				binary.LittleEndian.PutUint32(u[4:], uint32(next())<<16)
				frame(2, cmdUPD, sid, u[:])
			case 4:
				frame(2, cmdNOP, 0, nil)
			case 5: // garbage: a bad version or command ends the session
				frame(next()%4, next()%8, sid, nil)
			}
		}
		go func() {
			c1.Write(out)
			c1.Close()
		}()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Fatal("the session's acceptor did not end after the peer's connection did")
		}
		srv.Close()
	})
}
