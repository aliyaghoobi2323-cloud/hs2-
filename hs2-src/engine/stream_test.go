package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

func testCert(t *testing.T) tls.Certificate {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "lab.example.com"},
		DNSNames: []string{"lab.example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &k.PublicKey, k)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// echoPanel echoes TCP and UDP on the same address.
func echoPanel(t *testing.T) string {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	pc, _ := net.ListenPacket("udp", ln.Addr().String())
	t.Cleanup(func() { ln.Close(); pc.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	go func() {
		b := make([]byte, 65536)
		for {
			n, a, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			pc.WriteTo(b[:n], a)
		}
	}()
	return ln.Addr().String()
}

type tunnel struct {
	userAddr string
	cancel   context.CancelFunc
	kill     func() // closes every live link on the kharej side
}

func startTunnel(t *testing.T, min, max int) *tunnel {
	key := bytes.Repeat([]byte{0x5a}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	panel := echoPanel(t)

	raw, _ := net.Listen("tcp", "127.0.0.1:0")
	var live []net.Conn
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	ln := &trackListener{Listener: raw, onAccept: func(c net.Conn) { <-mu; live = append(live, c); mu <- struct{}{} }}
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	go RunKharej(ctx, KharejConfig{Listener: ln, Server: srv, Panel: panel})

	port := freePort(t)
	go RunIran(ctx, IranConfig{
		Dialer: NewMTCPDialer(raw.Addr().String(), "lab.example.com", key, ""),
		Min:    min, Max: max, PerLink: 50,
		ListenIP: "127.0.0.1", Ports: []string{port}, UDP: true,
	})
	tn := &tunnel{userAddr: "127.0.0.1:" + port, cancel: cancel, kill: func() {
		<-mu
		for _, c := range live {
			c.Close()
		}
		live = nil
		mu <- struct{}{}
	}}
	// wait for the user port
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", tn.userAddr); err == nil {
			c.Close()
			return tn
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("user port never opened")
	return nil
}

type trackListener struct {
	net.Listener
	onAccept func(net.Conn)
}

func (l *trackListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.onAccept(c)
	}
	return c, err
}

func tcpEcho(t *testing.T, addr string, size int) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	msg := make([]byte, size)
	rand.Read(msg)
	go c.Write(msg)
	got := make([]byte, size)
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("echo corrupted")
	}
}

func TestStreamTCPAndUDP(t *testing.T) {
	tn := startTunnel(t, 3, 3)
	for i := 0; i < 5; i++ {
		tcpEcho(t, tn.userAddr, 1<<20)
	}
	u, err := net.Dial("udp", tn.userAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	for i := 0; i < 20; i++ {
		msg := []byte("datagram-" + strconv.Itoa(i))
		u.Write(msg)
		u.SetReadDeadline(time.Now().Add(3 * time.Second))
		b := make([]byte, 100)
		n, err := u.Read(b)
		if err != nil || string(b[:n]) != string(msg) {
			t.Fatalf("udp echo %d: %q err=%v", i, b[:n], err)
		}
	}
}

// After every link dies, the pool rebuilds and new users get through.
func TestStreamRecoversAfterLinksDie(t *testing.T) {
	tn := startTunnel(t, 2, 2)
	tcpEcho(t, tn.userAddr, 4096)
	tn.kill()
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", tn.userAddr, 2*time.Second)
		if err == nil {
			c.SetDeadline(time.Now().Add(2 * time.Second))
			c.Write([]byte("x"))
			var b [1]byte
			_, err = io.ReadFull(c, b[:])
			c.Close()
			if err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("tunnel did not recover after links died")
		}
		time.Sleep(200 * time.Millisecond)
	}
	tcpEcho(t, tn.userAddr, 1<<20)
}
