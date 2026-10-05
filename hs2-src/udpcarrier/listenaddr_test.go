package udpcarrier

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// Every listen address form carries frames both ways, with batches on (the
// default): an IPv6 listener's peers are named with sockaddr_in6, which the
// batch reader must not drop.
func TestRoundTripListenAddrs(t *testing.T) {
	for _, c := range []struct{ listen, dial string }{
		{"127.0.0.1:0", "127.0.0.1"},
		{"0.0.0.0:0", "127.0.0.1"},
		{"[::1]:0", "::1"},
		{"[::]:0", "::1"},
	} {
		t.Run(c.listen, func(t *testing.T) {
			if net.ParseIP(c.dial).To4() == nil {
				if pc, err := net.ListenPacket("udp6", "[::1]:0"); err != nil {
					t.Skip("no IPv6 loopback")
				} else {
					pc.Close()
				}
			}
			shared := testShared()
			l, err := Listen(c.listen, shared, 0)
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer l.Close()
			_, port, _ := net.SplitHostPort(l.LocalAddr().String())
			accCh := make(chan *Conn, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				s, _ := l.Accept(ctx)
				accCh <- s
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cli, err := Dial(ctx, net.JoinHostPort(c.dial, port), shared, 0)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer cli.Close()
			srv := <-accCh
			if srv == nil {
				t.Fatal("accept failed")
			}
			defer srv.Close()
			for i := 0; i < 50; i++ {
				msg := []byte(fmt.Sprintf("frame %d to the listener", i))
				if err := cli.SendFrame(core.TypeData, msg); err != nil {
					t.Fatal(err)
				}
				if _, got := readOne(t, srv, 3*time.Second); !bytes.Equal(got, msg) {
					t.Fatalf("listener got %q, want %q", got, msg)
				}
				back := []byte(fmt.Sprintf("frame %d back", i))
				if err := srv.SendFrame(core.TypeData, back); err != nil {
					t.Fatal(err)
				}
				if _, got := readOne(t, cli, 3*time.Second); !bytes.Equal(got, back) {
					t.Fatalf("dialer got %q, want %q", got, back)
				}
			}
		})
	}
}
