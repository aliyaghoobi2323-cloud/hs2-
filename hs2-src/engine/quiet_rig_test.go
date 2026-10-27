package engine

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// β2 over real TLS on the loopback, direct and reverse: the edge's pool with
// its QUIET links (as RunIran builds it) and the real exit (RunKharej).

type quietPool struct {
	t        *testing.T
	lm       *LinkManager
	q        *quietSet
	userAddr string
	edge     *logSink
	exit     *logSink
}

func startQuietPool(t *testing.T, direct bool, maxLinks int) *quietPool {
	t.Helper()
	if QuietOff() || SchedOff() {
		t.Skip("β2 off (HS2_TUNE_QUIET=0 or HS2_TUNE_SCHED=0)")
	}
	key := bytes.Repeat([]byte{0x6b}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	p := &quietPool{t: t, edge: &logSink{}, exit: &logSink{}}
	t.Cleanup(cancel)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("edge log:\n%s\nexit log:\n%s", p.edge, p.exit)
		}
	})
	panel := echoPanel(t)
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	var lm *LinkManager
	if direct {
		go RunKharej(ctx, KharejConfig{Listener: ln, Server: srv, Panel: panel, Log: p.exit.logf})
		lm = NewLinkManager(NewMTCPDialer(addr, "lab.example.com", key, ""), maxLinks, maxLinks, 8, p.edge.logf)
	} else {
		lm = NewLinkManager(nil, maxLinks, maxLinks, 8, p.edge.logf)
		lm.accept = true
	}
	p.lm = lm
	p.q = lm.enableQuiet()
	mine := peerInfo{MaxLinks: QuietTotal(maxLinks), Caps: capPortTags}
	lm.OnLink = func(l Link) {
		go openControl(ctx, l, p.edge.logf)
		go openStats(ctx, l, p.edge.logf)
		go openInfo(ctx, l, mine)
		if !direct {
			go openPoolCtl(ctx, l, lm, p.edge.logf, func() { lm.markPoolRefused(l) })
		}
	}
	go lm.Run(ctx)
	if !direct {
		srv.OfferCaps = p.q.offer
		go acceptReverseLinks(ctx, ln, srv, lm, p.edge.logf)
		dial := func() (*tlscarrier.Carrier, error) {
			return tlscarrier.DialFromTimeoutCaps(addr, "lab.example.com", key, "", 8*time.Second, tlscarrier.RoleCaps)
		}
		go RunKharej(ctx, KharejConfig{Panel: panel, RevLinks: maxLinks, RevMin: maxLinks, RevMax: maxLinks,
			RevDial: dial, Log: p.exit.logf})
	}
	uln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.userAddr = uln.Addr().String()
	go func() { <-ctx.Done(); uln.Close() }()
	go func() {
		for {
			c, err := uln.Accept()
			if err != nil {
				return
			}
			go serveUserTCP(ctx, c, lm, 0)
		}
	}()
	return p
}

func (p *quietPool) waitFor(what string, d time.Duration, ok func() bool) {
	p.t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			p.t.Fatalf("timed out after %s: %s (%s)", d, what, p.q.line())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// serving is the BULK links serving now.
func (p *quietPool) serving() int {
	s, _ := func() (int, int) {
		p.lm.mu.RLock()
		defer p.lm.mu.RUnlock()
		return p.lm.countsLocked()
	}()
	return s
}

// echoBulk sends n bytes through a user connection and reads their echo,
// checking each byte; it returns once all came back.
func (p *quietPool) echoBulk(n int) {
	p.t.Helper()
	c, err := net.Dial("tcp", p.userAddr)
	if err != nil {
		p.t.Fatal(err)
	}
	defer c.Close()
	src := make([]byte, n)
	for i := range src {
		src[i] = byte(i*31 + i>>11)
	}
	errs := make(chan error, 1)
	go func() {
		// paced, 32 KiB each 20 ms (1.6 MB/s): a transfer of seconds even
		// on the loopback, as one over a real path
		for off := 0; off < n; off += 32 << 10 {
			end := min(off+32<<10, n)
			if _, err := c.Write(src[off:end]); err != nil {
				errs <- err
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		errs <- nil
	}()
	got := make([]byte, n)
	c.SetReadDeadline(time.Now().Add(60 * time.Second))
	if _, err := io.ReadFull(c, got); err != nil {
		p.t.Fatalf("echo: %v", err)
	}
	if err := <-errs; err != nil {
		p.t.Fatal(err)
	}
	if !bytes.Equal(got, src) {
		for i := range got {
			if got[i] != src[i] {
				p.t.Fatalf("echo differs from byte %d of %d", i, n)
			}
		}
	}
}

// The pool's classes and their upkeep, both directions of the tunnel:
// max_links 1 runs one QUIET and one BULK link, 4 two and two; a bulk
// connection opens on QUIET, moves once to BULK, its bytes whole and in
// order; a light one stays; a QUIET link that dies is replaced; resizing
// the BULK pool keeps the QUIET count.
func TestQuietPoolClasses(t *testing.T) {
	for _, direct := range []bool{true, false} {
		for _, mx := range []int{1, 4} {
			name := "reverse"
			if direct {
				name = "direct"
			}
			t.Run(name+"/"+string(rune('0'+mx)), func(t *testing.T) {
				p := startQuietPool(t, direct, mx)
				wantQ, wantB := QuietCount(mx), max(1, mx-QuietCount(mx))
				if QuietExtra != 0 {
					wantB = mx // the QUIET links next to max_links BULK ones (T19)
				}
				p.waitFor("the classes up", 20*time.Second, func() bool {
					return p.q.count() == wantQ && p.serving() == wantB
				})
				moved0, att0 := p.q.moved.Load(), exitGroups.attached.Load()
				// a light connection: a few bytes, echoed
				p.echoBulk(200)
				if p.q.moved.Load() != moved0 {
					t.Fatal("a light connection moved")
				}
				// a bulk one: 3 MiB through the echo both ways
				p.echoBulk(3 << 20)
				p.waitFor("the bulk connection moved", 5*time.Second, func() bool {
					return p.q.moved.Load() == moved0+1 && exitGroups.attached.Load() == att0+1
				})
				// a QUIET link dies: replaced, as many as before
				ql := p.q.quietLinks()
				ql[0].link.Close()
				p.waitFor("the dead QUIET link gone", 10*time.Second, func() bool {
					for _, l := range p.q.quietLinks() {
						if l == ql[0] {
							return false
						}
					}
					return true
				})
				p.waitFor("the QUIET link replaced", 20*time.Second, func() bool { return p.q.count() == wantQ })
				p.echoBulk(1 << 10)
				// the BULK pool shrinks to one: the QUIET links stay
				if wantB > 1 {
					p.lm.pin.Store(1)
					p.waitFor("one BULK link serving", 20*time.Second, func() bool { return p.serving() == 1 })
					if p.q.count() != wantQ {
						t.Fatalf("%d QUIET links after the resize, want %d", p.q.count(), wantQ)
					}
				}
				t.Log(p.q.line())
			})
		}
	}
}
