package udpcarrier

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// On a listener every carrier shares ONE socket and ONE read loop. A carrier
// whose reader has stalled (its decode queue and frame queue are full) must not
// freeze the others: its datagrams are dropped instead. Before, the read loop
// blocked on the stalled carrier's queue and every other carrier from every
// peer went silent until that carrier was torn down — seen in the lab as a
// ~70 s blackout of a whole reverse icmp pool after a load test.
func TestListenerStalledCarrierDoesNotBlockOthers(t *testing.T) {
	shared := testShared()
	l, err := Listen("127.0.0.1:0", shared, 0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	addr := l.LocalAddr().String()

	dialAccept := func() (cli, srv *Conn) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		accCh := make(chan *Conn, 1)
		go func() {
			c, _ := l.Accept(ctx)
			accCh <- c
		}()
		cli, err := Dial(ctx, addr, shared, 0)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		srv = <-accCh
		if srv == nil {
			t.Fatal("accept failed")
		}
		return cli, srv
	}
	stuckCli, stuckSrv := dialAccept() // stuckSrv is never read
	defer stuckCli.Close()
	defer stuckSrv.Close()
	okCli, okSrv := dialAccept()
	defer okCli.Close()
	defer okSrv.Close()

	// Fill the stalled carrier far past its frame queue (2048) and decode
	// queue (1024), so the listener has to hand it datagrams it cannot take.
	payload := bytes.Repeat([]byte{0xab}, 200)
	floodDone := make(chan struct{})
	go func() {
		defer close(floodDone)
		for i := 0; i < 12000; i++ {
			if stuckCli.SendFrame(core.TypeData, payload) != nil {
				return
			}
		}
	}()
	select {
	case <-floodDone:
	case <-time.After(20 * time.Second):
		t.Fatal("flood did not finish")
	}
	time.Sleep(300 * time.Millisecond) // let the listener work through what arrived

	// The other carrier must still deliver, promptly.
	msg := []byte("still alive")
	got := make(chan []byte, 1)
	go func() {
		for {
			ft, p, err := okSrv.ReadFrame()
			if err != nil {
				return
			}
			if ft == core.TypeData {
				got <- append([]byte(nil), p...)
				return
			}
		}
	}()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		okCli.SendFrame(core.TypeData, msg)
		select {
		case p := <-got:
			if !bytes.Equal(p, msg) {
				t.Fatalf("got %q, want %q", p, msg)
			}
			if d := stuckSrv.Stats().RxDropped; d == 0 {
				t.Fatal("the flood never overflowed the stalled carrier — the test did not exercise the stall")
			} else {
				t.Logf("stalled carrier: %d datagrams dropped, the other carrier unaffected", d)
			}
			return
		case <-tick.C:
		case <-deadline:
			t.Fatal("a stalled carrier blocked the listener: another carrier on the same socket delivered nothing for 3 s")
		}
	}
}
