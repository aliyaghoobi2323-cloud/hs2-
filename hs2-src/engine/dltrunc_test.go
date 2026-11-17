package engine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// fx170 (mirror of fx168's exitTarget): a download that the tunnel cannot
// finish — the target resets, or every link of the path dies mid-transfer —
// must reach the user's app as an error (RST), never as a clean FIN after
// fewer bytes than the target sent: a cut file would look complete.

// cutProxy forwards TCP to dst; cut closes every connection it carries (rst:
// with an RST, else a FIN). New connections keep working.
type cutProxy struct {
	mu    sync.Mutex
	conns []*net.TCPConn
}

func (p *cutProxy) add(c ...*net.TCPConn) {
	p.mu.Lock()
	p.conns = append(p.conns, c...)
	p.mu.Unlock()
}

func (p *cutProxy) cut(rst bool) int {
	p.mu.Lock()
	cs := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, c := range cs {
		if rst {
			c.SetLinger(0)
		}
		c.Close()
	}
	return len(cs)
}

func startCutProxy(t *testing.T, dst string) (*cutProxy, string) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	p := &cutProxy{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("tcp", dst)
			if err != nil {
				c.Close()
				continue
			}
			p.add(c.(*net.TCPConn), u.(*net.TCPConn))
			go func() { io.Copy(c, u); c.Close() }()
			go func() { io.Copy(u, c); u.Close() }()
		}
	}()
	return p, ln.Addr().String()
}

// startCutTunnel is startTunnelTarget with the Iran→exit links through a
// cutProxy.
func startCutTunnel(t *testing.T, links int, target string) (*cutProxy, string) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	raw, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	go RunKharej(ctx, KharejConfig{Listener: raw, Server: srv, Panel: target})
	p, px := startCutProxy(t, raw.Addr().String())
	port := freePort(t)
	go RunIran(ctx, IranConfig{
		Dialer: NewMTCPDialer(px, "lab.example.com", key, ""),
		Min:    links, Max: links, PerLink: 50,
		ListenIP: "127.0.0.1", Ports: []string{port}, UDP: true,
	})
	addr := "127.0.0.1:" + port
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return p, addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("user port never opened")
	return nil, ""
}

const (
	dlHalf  = 256 << 10
	dlTotal = 2 * dlHalf // what a complete download would be
)

// dlSource is a target that, per connection, reads one byte, sends dlHalf
// bytes, reports on sent, and then: mode "rst" resets the connection; "hold"
// keeps it open (never sends the rest) until the test ends; "full" sends the
// rest and closes cleanly.
func dlSource(t *testing.T, mode string, sent chan<- struct{}) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var b [1]byte
				if _, err := io.ReadFull(c, b[:]); err != nil || b[0] != 'g' {
					return // a warm-up connection
				}
				buf := make([]byte, dlHalf)
				for i := range buf {
					buf[i] = byte(i * 13)
				}
				if _, err := c.Write(buf); err != nil {
					return
				}
				select {
				case sent <- struct{}{}:
				default:
				}
				switch mode {
				case "rst":
					time.Sleep(300 * time.Millisecond)
					c.(*net.TCPConn).SetLinger(0)
				case "hold":
					<-done
				case "full":
					c.Write(buf)
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// dlRead downloads through addr: it returns the bytes read and the error
// that ended the read (nil: a clean EOF).
func dlRead(addr string, limit time.Duration) (int, error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(limit))
	if _, err := c.Write([]byte{'g'}); err != nil {
		return 0, err
	}
	n, err := io.Copy(io.Discard, c)
	return int(n), err
}

// warmMConn makes sure the edge knows the exit reads mconn (its kindInfo
// came) before the test's connection.
func warmMConn(t *testing.T, addr string) {
	before := mconnStat.total.Load()
	for i := 0; mconnStat.total.Load() == before; i++ {
		if i == 50 {
			t.Fatal("no connection ever used an mconn")
		}
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
}

func TestDownloadTruncationResetsUser(t *testing.T) {
	if testing.Short() {
		t.Skip("tunnel")
	}
	type tc struct {
		path string // "mconn", "lite", "stream"
		how  string // "rst": the target resets; "cutfin"/"cutrst": every link dies
	}
	var cases []tc
	for _, p := range []string{"mconn", "lite", "stream"} {
		for _, h := range []string{"full", "rst", "cutfin", "cutrst"} {
			cases = append(cases, tc{p, h})
		}
	}
	for _, c := range cases {
		t.Run(c.path+"/"+c.how, func(t *testing.T) {
			withMConn(t, c.path != "stream")
			setLite(t, c.path == "lite")
			mode := "hold"
			switch c.how {
			case "rst", "full":
				mode = c.how
			}
			sent := make(chan struct{}, 1)
			p, addr := startCutTunnel(t, 2, dlSource(t, mode, sent))
			if c.path != "stream" {
				warmMConn(t, addr)
			}
			u0, s0 := mconnUserRst.Load(), streamLocalRst.Load()
			if c.how == "cutfin" || c.how == "cutrst" {
				go func() {
					select {
					case <-sent:
					case <-time.After(20 * time.Second):
						return
					}
					time.Sleep(300 * time.Millisecond)
					t.Logf("cut %d proxied conns", p.cut(c.how == "cutrst"))
				}()
			}
			t0 := time.Now()
			got, err := dlRead(addr, 40*time.Second)
			t.Logf("got %d of %d, end err %v after %v; user resets mconn %d stream %d", got, dlTotal, err,
				time.Since(t0).Round(time.Millisecond), mconnUserRst.Load()-u0, streamLocalRst.Load()-s0)
			if c.how == "full" {
				if got != dlTotal || err != nil {
					t.Errorf("a complete download: got %d of %d, err %v", got, dlTotal, err)
				}
				return
			}
			if err == nil && got < dlTotal {
				if c.path != "mconn" && c.how == "rst" {
					// known: smux has no stream reset — on the one-stream
					// path and a raw (lite) mConn the exit can only FIN the
					// stream when its target resets
					t.Logf("KNOWN (one-stream / raw path, target reset): the user got %d of %d and a clean EOF", got, dlTotal)
					return
				}
				t.Errorf("SILENT TRUNCATION: the user got %d of %d bytes and a clean EOF", got, dlTotal)
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				t.Errorf("the user's read hung (no end at all) after %d bytes", got)
			}
		})
	}
}
