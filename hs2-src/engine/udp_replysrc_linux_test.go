//go:build linux

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

// A user port on every address (user_listen_ip "": the installer's "all")
// answered UDP from the address the routing table prefers, not from the one
// the user sent to. On a server with several addresses a user of a second
// one — whose socket is connected, as game clients' and xray's are — then
// got no reply at all: its kernel drops datagrams from any other source
// (r1-udp: the rig's "secondip" case). 127.0.0.2 stands in for the second
// address: the routing-preferred source toward 127.0.0.1 is 127.0.0.1.
func TestUDPUserPortRepliesFromTheAddressUsed(t *testing.T) {
	key := bytes.Repeat([]byte{0x5a}, 32)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	panel := echoPanel(t)
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	go RunKharej(ctx, KharejConfig{Listener: raw, Server: srv, Panel: panel})
	port := freePort(t)
	go RunIran(ctx, IranConfig{
		Dialer: NewMTCPDialer(raw.Addr().String(), "lab.example.com", key, ""),
		Min:    2, Max: 2, PerLink: 50,
		ListenIP: "", Ports: []string{port}, UDP: true,
	})
	deadline := time.Now().Add(15 * time.Second)
	for !echoOnce("127.0.0.1:"+port, 64) {
		if time.Now().After(deadline) {
			t.Fatal("the tunnel never came up")
		}
		time.Sleep(100 * time.Millisecond)
	}
	n, _ := strconv.Atoi(port)
	// the port's UDP must serve before the reply source is judged: a port
	// whose UDP another program holds gets no reply on any address (the
	// edge keeps TCP and waits for the UDP port), which is not this bug
	deadline = time.Now().Add(5 * time.Second)
	for !udpEchoOnce(t, "127.0.0.1:"+port) {
		if time.Now().After(deadline) {
			t.Fatalf("the user port's UDP never answered on 127.0.0.1 (another program holds UDP %s?)", port)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, dst := range []string{"127.0.0.1", "127.0.0.2"} {
		c, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")}, &net.UDPAddr{IP: net.ParseIP(dst), Port: n})
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		b := make([]byte, 64)
		for i := 0; i < 10; i++ {
			msg := []byte("to-" + dst + "-" + strconv.Itoa(i))
			c.Write(msg)
			c.SetReadDeadline(time.Now().Add(time.Second))
			if k, err := c.Read(b); err == nil && string(b[:k]) == string(msg) {
				got++
			}
		}
		c.Close()
		if got < 9 {
			t.Errorf("a connected user of %s got %d of 10 echoes: the replies do not come from the address it sent to", dst, got)
		}
	}
}

// udpEchoOnce sends one datagram to addr and waits up to 500 ms for its echo.
func udpEchoOnce(t *testing.T, addr string) bool {
	t.Helper()
	c, err := net.Dial("udp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := []byte("up?")
	c.Write(msg)
	c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	b := make([]byte, 64)
	k, err := c.Read(b)
	return err == nil && string(b[:k]) == string(msg)
}
