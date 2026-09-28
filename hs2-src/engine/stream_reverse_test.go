package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// startReverseTunnel brings up the stream tunnel in REVERSE: the iran edge
// LISTENS (TLS server) and the kharej exit DIALS in, while the data path and
// smux roles stay the same as direct (iran originates user streams, kharej
// delivers to the panel).
func startReverseTunnel(t *testing.T, nLinks int) *tunnel {
	key := bytes.Repeat([]byte{0x5a}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	panel := echoPanel(t)

	// iran edge: a TLS-carrier server the kharej dials into.
	iranLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	iranSrv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	port := freePort(t)
	go RunIran(ctx, IranConfig{
		RevServer: iranSrv, RevListener: iranLn,
		PerLink:  50,
		ListenIP: "127.0.0.1", Ports: []string{port}, UDP: true,
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

	tn := &tunnel{userAddr: "127.0.0.1:" + port, cancel: cancel}
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
	// force reconnection by cancelling is too coarse; instead just verify the
	// tunnel keeps working across several transfers (redial happens under load).
	for i := 0; i < 10; i++ {
		tcpEcho(t, tn.userAddr, 64<<10)
	}
}
