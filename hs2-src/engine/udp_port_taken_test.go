package engine

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// The owner's report (R1 round 3): with "udp": true the Iran server's tunnel
// did not come up. RunIran opened each user port's UDP socket right after its
// TCP listener and returned the bind error when another program held that UDP
// port; hs2 run exits on that error (must), so systemd restarted it every 3 s
// and the TCP user port — the one the panel's users need — never served. UDP
// on the user ports is an addition: a port it cannot have must not take the
// tunnel down. The edge keeps serving TCP, says why UDP is not forwarded, and
// forwards UDP as soon as the port is free, without a restart.
func TestUDPPortTakenKeepsTheTunnel(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	panel := echoPanel(t)

	// a user port whose TCP is free and whose UDP another program holds
	ln, held := tcpUDPPair(t)
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	defer held.Close()
	userAddr := "127.0.0.1:" + port

	// the production topology: reverse (the edge listens for the exit's links)
	iranLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var logs logSink
	ran := make(chan error, 1)
	go func() {
		ran <- RunIran(ctx, IranConfig{
			RevServer:   &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"},
			RevListener: iranLn,
			Min:         2, Max: 2, PerLink: 50,
			ListenIP: "127.0.0.1", Ports: []string{port}, UDP: true,
			Log: logs.logf,
		})
	}()
	iranAddr := iranLn.Addr().String()
	go RunKharej(ctx, KharejConfig{
		Panel: panel, RevLinks: 2, RevMin: 2, RevMax: 2,
		RevDial: func() (*tlscarrier.Carrier, error) {
			return tlscarrier.DialFrom(iranAddr, "lab.example.com", key, "")
		},
	})
	stopped := func() {
		t.Helper()
		select {
		case err := <-ran:
			t.Fatalf("the edge stopped, and hs2 run exits on that (the tunnel never comes up): %v", err)
		default:
		}
	}

	deadline := time.Now().Add(15 * time.Second)
	for !echoOnce(userAddr, 256) {
		stopped()
		if time.Now().After(deadline) {
			t.Fatal("the TCP user port never carried a connection")
		}
		time.Sleep(100 * time.Millisecond)
	}
	stopped()
	if logs.count("UDP is NOT forwarded", 0) == 0 {
		t.Errorf("no log line says why UDP is not forwarded on the port: %q", logs.snapshot())
	}

	// the other program lets the port go: UDP is forwarded, no restart
	held.Close()
	u, err := net.Dial("udp", userAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	deadline = time.Now().Add(15 * time.Second)
	b := make([]byte, 64)
	for i := 0; ; i++ {
		msg := []byte("after-" + strconv.Itoa(i))
		u.Write(msg)
		u.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if n, err := u.Read(b); err == nil && string(b[:n]) == string(msg) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("UDP never forwarded after the port was freed: %q", logs.snapshot())
		}
	}
	if logs.count("UDP forwarded now", 0) != 1 {
		t.Errorf("want one line saying UDP is forwarded now: %q", logs.snapshot())
	}
	stopped()
	tcpEcho(t, userAddr, 64<<10)
}
