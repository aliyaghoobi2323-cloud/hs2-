package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// startReverseTunnel brings up the stream tunnel in REVERSE: the iran edge
// LISTENS (TLS server) and the kharej exit DIALS in, while the data path and
// smux roles stay the same as direct (iran originates user streams, kharej
// delivers to the panel).
func startReverseTunnel(t *testing.T, nLinks int) *tunnel {
	return startReverseTunnelLog(t, nLinks, nil)
}

func startReverseTunnelLog(t *testing.T, nLinks int, iranLog func(string, ...any)) *tunnel {
	key := bytes.Repeat([]byte{0x5a}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	panel := echoPanel(t)

	// iran edge: a TLS-carrier server the kharej dials into. Wrap the listener so
	// the test can close the accepted links (the reverse carriers) to force the
	// kharej to redial.
	rawIranLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var lmu = make(chan struct{}, 1)
	lmu <- struct{}{}
	var live []net.Conn
	iranLn := &trackListener{Listener: rawIranLn, onAccept: func(c net.Conn) { <-lmu; live = append(live, c); lmu <- struct{}{} }}
	iranSrv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	port := freePort(t)
	go RunIran(ctx, IranConfig{
		RevServer: iranSrv, RevListener: iranLn,
		PerLink:  50,
		ListenIP: "127.0.0.1", Ports: []string{port}, UDP: true,
		Log: iranLog,
	})

	// kharej exit: dials nLinks carriers to the iran edge.
	iranAddr := iranLn.Addr().String()
	go RunKharej(ctx, KharejConfig{
		Panel:    panel,
		RevLinks: nLinks,
		RevDial: func() (*tlscarrier.Carrier, error) {
			return tlscarrier.DialFrom(iranAddr, "lab.example.com", key, "")
		},
	})

	tn := &tunnel{userAddr: "127.0.0.1:" + port, cancel: cancel, kill: func() {
		<-lmu
		for _, c := range live {
			c.Close()
		}
		live = nil
		lmu <- struct{}{}
	}}
	// Wait until an end-to-end echo actually works (a reverse link is up).
	for i := 0; i < 120; i++ {
		if echoOnce(tn.userAddr, 256) {
			return tn
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("reverse tunnel never carried an echo")
	return nil
}

func echoOnce(addr string, size int) bool {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	msg := make([]byte, size)
	rand.Read(msg)
	if _, err := c.Write(msg); err != nil {
		return false
	}
	got := make([]byte, size)
	if _, err := io.ReadFull(c, got); err != nil {
		return false
	}
	return bytes.Equal(got, msg)
}

func TestReverseStreamTCPAndUDP(t *testing.T) {
	tn := startReverseTunnel(t, 3)
	for i := 0; i < 5; i++ {
		tcpEcho(t, tn.userAddr, 1<<20)
	}
	u, err := net.Dial("udp", tn.userAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	for i := 0; i < 20; i++ {
		msg := []byte("rev-datagram-" + strconv.Itoa(i))
		u.Write(msg)
		u.SetReadDeadline(time.Now().Add(3 * time.Second))
		b := make([]byte, 100)
		n, err := u.Read(b)
		if err != nil || string(b[:n]) != string(msg) {
			t.Fatalf("reverse udp echo %d: %q err=%v", i, b[:n], err)
		}
	}
}

// A reverse link that dies is redialed by the kharej and users get through again.
func TestReverseStreamRedials(t *testing.T) {
	tn := startReverseTunnel(t, 2)
	tcpEcho(t, tn.userAddr, 4096)
	// Kill every accepted reverse link; the kharej's maintainExitLink must redial
	// and the tunnel must carry traffic again.
	tn.kill()
	ok := false
	for i := 0; i < 100; i++ {
		if echoOnce(tn.userAddr, 512) {
			ok = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ok {
		t.Fatal("reverse tunnel did not recover after its links were killed")
	}
	tcpEcho(t, tn.userAddr, 64<<10)
}

// The iran edge logs every reverse link that goes down, with a reason, and its
// "now N" counts only live links: after all links are killed and redialed the
// count must never exceed the number the kharej keeps (the old code counted
// dead links still waiting for the next health tick and printed "now 16" for 8).
func TestReverseLinkLogsAreHonest(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	logf := func(f string, a ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(f, a...))
		mu.Unlock()
	}
	const n = 3
	tn := startReverseTunnelLog(t, n, logf)
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if ok() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("%s; log:\n%s", what, strings.Join(lines, "\n"))
	}
	count := func(sub string) int {
		mu.Lock()
		defer mu.Unlock()
		c := 0
		for _, l := range lines {
			if strings.Contains(l, sub) {
				c++
			}
		}
		return c
	}
	waitFor("links never came up", func() bool { return count("up from 127.0.0.1") >= n })
	tn.kill()
	waitFor("link closures were not logged", func() bool { return count("down: ") >= n })
	waitFor("links were not redialed", func() bool { return count("up from 127.0.0.1") >= 2*n })
	re := regexp.MustCompile(`\(now (\d+)\)`)
	mu.Lock()
	defer mu.Unlock()
	for _, l := range lines {
		if !strings.Contains(l, "reverse link") {
			continue
		}
		if m := re.FindStringSubmatch(l); m != nil {
			if v, _ := strconv.Atoi(m[1]); v > n {
				t.Fatalf("log claims %d links but only %d exist: %q", v, n, l)
			}
		}
		if strings.Contains(l, "down: ") && strings.HasSuffix(strings.TrimSpace(strings.SplitN(l, "down: ", 2)[1]), ":") {
			t.Fatalf("empty reason: %q", l)
		}
	}
}
