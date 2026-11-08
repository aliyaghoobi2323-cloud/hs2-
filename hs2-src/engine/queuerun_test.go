package engine

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"testing"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// The edge's (upload) lines name their interface and key; the exit's stay
// as they always were.
func TestQUpLog(t *testing.T) {
	var got []string
	logf := func(f string, a ...any) { got = append(got, fmt.Sprintf(f, a...)) }
	up := qUpLog("eth1", logf)
	up("queue: shaper on %s (tc htb + cake), tunnel egress to the edge at %.1f Mbit/s", "eth1", 90.0)
	up("queue: x capped (HS2_QUEUE_CTL)")
	up("queue-dbg delay=1ms")
	up("%s", "queue: the path loses 2.3% of what the links send — cap removed")
	want := []string{
		"queue-up eth1: shaper on eth1 (tc htb + cake), tunnel egress to the exit at 90.0 Mbit/s",
		"queue-up eth1: x capped (HS2_QUEUE_CTL_UP)",
		"queue-up eth1-dbg delay=1ms",
		"queue-up eth1: the path loses 2.3% of what the links send — cap removed",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

// Off (the default), a runner counts no link — the edge's included.
func TestQRunnerOffTracksNothing(t *testing.T) {
	r := &qRunner{links: map[*tlscarrier.Carrier]qPrev{}, byDev: true}
	r.track(&tlscarrier.Carrier{})()
	r.track(&tlscarrier.Carrier{})
	if len(r.links) != 0 {
		t.Fatalf("an off runner tracked %d links", len(r.links))
	}
	if !queueCtlUpOn {
		trackEdgeLink(context.Background(), &mtcpLink{})
		if len(edgeQueue.links) != 0 {
			t.Fatal("HS2_QUEUE_CTL_UP off, yet an edge link was tracked")
		}
	}
	on := &qRunner{links: map[*tlscarrier.Carrier]qPrev{}, on: true}
	forget := on.track(&tlscarrier.Carrier{})
	if len(on.links) != 1 {
		t.Fatal("an on runner did not track the link")
	}
	forget()
	if len(on.links) != 0 {
		t.Fatal("the link was not forgotten")
	}
}

// A link's interface is the route from its own address: loopback here.
func TestQLinkDevLoopback(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			var b [1]byte
			c.Read(b[:])
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("no ip(8)")
	}
	if dev := qLinkDev(c.(*net.TCPConn)); dev != "lo" {
		t.Fatalf("loopback link: interface %q, want lo", dev)
	}
}
