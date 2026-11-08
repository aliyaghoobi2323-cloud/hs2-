package main

// ucall: a two-way UDP video-call-like stream (WebRTC shape): size-byte
// datagrams at a constant rate to the server's UDP echo, each carrying its
// sequence number and scheduled send time. A datagram that never comes back
// (within 2 s after the end) is a lost frame; one back after -latems is late.
//
// -ucalladdr points it somewhere other than -addr: the downloads through the
// forwarded port, the call through the TUN (run.sh UCALL).

import (
	"encoding/binary"
	"math"
	"net"
	"sync"
	"time"
)

const ucallHdr = 12 // u32 seq, i64 scheduled send time (ns since probe start)

func runUDPCall(sp streamSpec, stop <-chan struct{}) *streamResult {
	res := &streamResult{Mbps: sp.mbps}
	select {
	case <-stop:
		res.Error = "stopped before start"
		return res
	case <-time.After(sp.delay):
	}
	c, err := net.Dial("udp", sp.addr)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer c.Close()
	var mu sync.Mutex
	got := map[uint32]float64{} // seq -> rtt ms (counted frames only)
	var order []float64         // rtts in arrival order, for the jitter
	done := make(chan struct{})
	go func() {
		defer close(done)
		b := make([]byte, 65536)
		for {
			n, err := c.Read(b)
			if err != nil {
				return
			}
			if n < ucallHdr {
				continue
			}
			ts := time.Duration(int64(binary.BigEndian.Uint64(b[4:12])))
			if ts < sp.warm {
				continue
			}
			seq := binary.BigEndian.Uint32(b[:4])
			rtt := ms(time.Since(sp.start) - ts)
			mu.Lock()
			if _, dup := got[seq]; !dup {
				got[seq] = rtt
				order = append(order, rtt)
			}
			mu.Unlock()
		}
	}()
	buf := make([]byte, 65536)
	next := time.Now()
	tm := time.NewTimer(0)
	defer tm.Stop()
	sent := 0
	var seq uint32
loop:
	for {
		if d := time.Until(next); d > 0 {
			tm.Reset(d)
			select {
			case <-stop:
				break loop
			case <-tm.C:
			}
		} else {
			select {
			case <-stop:
				break loop
			default:
			}
		}
		n := min(max(sp.size(), ucallHdr), len(buf))
		ts := next.Sub(sp.start)
		binary.BigEndian.PutUint32(buf[0:4], seq)
		binary.BigEndian.PutUint64(buf[4:12], uint64(ts))
		seq++
		if ts >= sp.warm {
			sent++
		}
		c.Write(buf[:n])
		next = next.Add(sp.gap())
	}
	time.Sleep(2 * time.Second)
	c.Close()
	<-done
	mu.Lock()
	defer mu.Unlock()
	rtts := make([]float64, 0, len(got))
	for _, v := range got {
		rtts = append(rtts, v)
	}
	res.Sent = sent
	res.N = len(rtts)
	res.rtts = rtts
	res.Lost = max(res.Sent-res.N, 0)
	res.P50, res.P95, res.P99, res.Max = pct(rtts, .5), pct(rtts, .95), pct(rtts, .99), pct(rtts, 1)
	var jsum float64
	for i := 1; i < len(order); i++ {
		jsum += math.Abs(order[i] - order[i-1])
	}
	if len(order) > 1 {
		res.Jitter = jsum / float64(len(order)-1)
	}
	late := res.Lost
	for _, v := range rtts {
		if v > sp.lateMs {
			late++
		}
	}
	if res.Sent > 0 {
		res.LatePct = 100 * float64(late) / float64(res.Sent)
	}
	return res
}
