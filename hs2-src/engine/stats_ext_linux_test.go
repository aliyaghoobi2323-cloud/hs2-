//go:build linux

package engine

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// serveStats fills the fields past 64 bytes from the same TCP_INFO read as
// the rest: the exit socket's smoothed, least and deviation round trips,
// its resends and the data segments it sent. And tcpStats reads, on the
// receiving end, the segments in, the bytes received and those behind a
// hole: few or none on the loopback, but not always none (a sender that
// moves to another CPU mid-write can queue a segment behind the next one,
// 3–4 runs in 200), so only a count past the segments in fails.
func TestServeStatsExtendedFromSocket(t *testing.T) {
	edge, exit := loopbackTCP(t)
	go io.Copy(io.Discard, edge)
	payload := make([]byte, 64<<10)
	for i := 0; i < 64; i++ {
		if _, err := exit.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	a, b := net.Pipe()
	defer a.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go serveStats(ctx, b, tlscarrier.NewCarrier(exit), nil)
	a.SetDeadline(time.Now().Add(5 * time.Second))
	a.Write([]byte{statsVer})
	var hdr [3]byte
	if _, err := io.ReadFull(a, hdr[:]); err != nil || hdr[1] != statsRecLen || hdr[2]&1 == 0 {
		t.Fatalf("handshake % x, %v: want recLen %d with tcp_info", hdr, err, statsRecLen)
	}
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], 1)
	a.Write(p[:])
	rec := make([]byte, statsRecLen)
	if _, err := io.ReadFull(a, rec); err != nil {
		t.Fatal(err)
	}
	r := parseStatsRec(rec)
	ts, ok := tcpStats(exit)
	if !ok {
		t.Skip("no TCP_INFO")
	}
	if r.flags&statsFlagTCPInfo == 0 || r.srtt == 0 || r.minRTT == 0 || r.minRTT > r.srtt+r.rttVar*4 || r.segsOut == 0 || r.segsOut > uint64(ts.segsOut) {
		t.Fatalf("record %+v next to the socket's %+v", r, ts)
	}
	if ts.minRTT == 0 || ts.minRTT > ts.rtt+4*ts.rttVar {
		t.Fatalf("socket min round trip %d µs, smoothed %d µs", ts.minRTT, ts.rtt)
	}
	time.Sleep(50 * time.Millisecond)
	in, ok := tcpStats(edge)
	if !ok || in.segsIn == 0 || in.bytesReceived < 64*64<<10 || in.rcvOOO > in.segsIn {
		t.Fatalf("receiving end: %+v", in)
	}
}
