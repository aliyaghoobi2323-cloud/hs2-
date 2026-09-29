package udpcarrier

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// A wildcard listener must answer from the address the peer targeted. On
// loopback, a reply to 127.0.0.2 would otherwise leave from 127.0.0.1 (the
// routing-preferred source) and the dialer's connected socket drops it — the
// same failure as a multi-IP server whose primary address differs from the one
// the tunnel is configured on.
func TestWildcardListenerRepliesFromTargetedIP(t *testing.T) {
	shared := testShared()
	l, err := Listen("0.0.0.0:0", shared, 0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	port := l.LocalAddr().(*net.UDPAddr).Port
	addr := fmt.Sprintf("127.0.0.2:%d", port)

	pctx, pcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pcancel()
	pr, err := ProbeFrom(pctx, addr, "127.0.0.2", shared, 5, 20*time.Millisecond, time.Second)
	if err != nil || !pr.Reachable {
		t.Fatalf("probe to %s: reachable=%v err=%v", addr, pr.Reachable, err)
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		if c, err := l.Accept(ctx); err == nil {
			defer c.Close()
			if ft, p, err := c.ReadFrame(); err == nil {
				c.SendFrame(ft, p)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cli, err := DialFrom(ctx, addr, "127.0.0.2", shared, 0)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer cli.Close()
	if err := cli.SendFrame(core.TypeData, []byte("ping")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, p := readOne(t, cli, 3*time.Second); string(p) != "ping" {
		t.Fatalf("echo = %q", p)
	}
}

func TestDialFromRejectsInvalidSourceIP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := DialFrom(ctx, "127.0.0.1:9", "10.20.0.1533", testShared(), 0); err == nil {
		t.Fatal("DialFrom accepted an invalid source IP")
	}
	if _, err := ProbeFrom(ctx, "127.0.0.1:9", "not-an-ip", testShared(), 1, time.Millisecond, 10*time.Millisecond); err == nil {
		t.Fatal("ProbeFrom accepted an invalid source IP")
	}
}
