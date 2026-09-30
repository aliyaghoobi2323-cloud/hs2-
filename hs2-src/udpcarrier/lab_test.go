package udpcarrier

import (
	"context"
	"encoding/binary"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/lab/netsim"
)

// These are lab measurements from an in-process network simulator (lab/netsim),
// NOT from a real server path. They exercise the real carrier — the same Noise
// core, adaptive FEC and pacer used in production — against genuine bursty loss.

type labResult struct {
	dur             time.Duration
	sent            int
	deliveredUnique int
	deliveredBytes  int64
	goodputMbps     float64
	overheadPct     float64
	residualPct     float64
	wireLossPct     float64
	p50, p95, p99   float64 // ms
	maxMs           float64
	jitterMs        float64 // p95 - p50
	parityRatioEnd  float64
}

func (r labResult) log(t *testing.T, name string) {
	t.Helper()
	t.Logf("[%s] goodput=%.2f Mbit/s  wire-loss=%.1f%%  residual-loss=%.3f%%  FEC-overhead=%.0f%%  "+
		"latency p50=%.1f p95=%.1f p99=%.1f max=%.1f ms  jitter(p95-p50)=%.1f ms  (sent=%d delivered=%d)",
		name, r.goodputMbps, r.wireLossPct, r.residualPct, r.overheadPct,
		r.p50, r.p95, r.p99, r.maxMs, r.jitterMs, r.sent, r.deliveredUnique)
}

// runLab drives a one-way bulk transfer through the relay for dur and reports
// goodput, FEC overhead, residual loss and latency distribution.
func runLab(t *testing.T, cfg netsim.Config, dur time.Duration, payloadSize int, mid func(*netsim.Relay)) labResult {
	t.Helper()
	shared := testShared()
	l, err := Listen("127.0.0.1:0", shared, 0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	relay, err := netsim.NewRelay(l.LocalAddr().String(), cfg)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	defer relay.Close()

	accCh := make(chan *Conn, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		c, err := l.Accept(ctx)
		if err != nil {
			accCh <- nil
			return
		}
		accCh <- c
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cli, err := Dial(ctx, relay.FrontAddr(), shared, 0)
	if err != nil {
		t.Fatalf("dial through relay: %v", err)
	}
	defer cli.Close()
	srv := <-accCh
	if srv == nil {
		t.Fatal("server did not accept the carrier through the relay")
	}
	defer srv.Close()

	var mu sync.Mutex
	seen := make(map[uint32]bool)
	var lat []float64
	var deliveredBytes int64
	rxDone := make(chan struct{})
	go func() {
		defer close(rxDone)
		for {
			ft, p, err := srv.ReadFrame()
			if err != nil {
				return
			}
			if ft != core.TypeData || len(p) < 12 {
				continue
			}
			seq := binary.BigEndian.Uint32(p[:4])
			sendNanos := int64(binary.BigEndian.Uint64(p[4:12]))
			now := time.Now().UnixNano()
			mu.Lock()
			if !seen[seq] {
				seen[seq] = true
				deliveredBytes += int64(len(p))
				lat = append(lat, float64(now-sendNanos)/1e6)
			}
			mu.Unlock()
		}
	}()

	if payloadSize < 12 {
		payloadSize = 12
	}
	if debugRate {
		go func() {
			tk := time.NewTicker(250 * time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case <-rxDone:
					return
				case <-tk.C:
					s := cli.Stats()
					t.Logf("  [rate] btlBw=%.2f Mbit rtProp=%s loss=%dppm parity=%.2f queue=%.1fms", s.BtlBwBytes*8/1e6, s.RTProp, s.LossPPM, s.ParityRatio, cli.rc.queueSec()*1000)
				}
			}
		}()
	}
	buf := make([]byte, payloadSize)
	start := time.Now()
	deadline := start.Add(dur)
	var seq uint32
	midFired := mid == nil
	for time.Now().Before(deadline) {
		if !midFired && time.Since(start) >= dur/2 {
			mid(relay)
			midFired = true
		}
		binary.BigEndian.PutUint32(buf[:4], seq)
		binary.BigEndian.PutUint64(buf[4:12], uint64(time.Now().UnixNano()))
		if err := cli.SendFrame(core.TypeData, buf); err != nil {
			break
		}
		seq++
	}
	sent := int(seq)

	// Let FEC recover stragglers and groups expire before snapshotting.
	time.Sleep(400 * time.Millisecond)
	cst := cli.Stats()
	sst := srv.Stats()
	encStats := cst.Enc
	parityEnd := cst.ParityRatio
	wsLoss, _ := relay.LossFraction()
	qds, _ := relay.QueueDrops()
	tsPass, _, _, _ := relay.Stats()
	t.Logf("  enc: data=%d parity=%d groups=%d depth-derived  dec: data=%d parity=%d recovered=%d lost=%d dups=%d invalid=%d",
		encStats.Data, encStats.Parity, encStats.Groups,
		sst.Dec.Data, sst.Dec.Parity, sst.Dec.Recovered, sst.Dec.Lost, sst.Dec.Dups, sst.Dec.Invalid)
	t.Logf("  rate: btlBw=%.2f Mbit/s rtProp=%s loss=%dppm  relay qdrops=%d (%.1f%% offered)",
		cst.BtlBwBytes*8/1e6, cst.RTProp, cst.LossPPM, qds, float64(qds)/float64(tsPass+qds+1)*100)

	cli.Close()
	srv.Close()
	<-rxDone

	mu.Lock()
	defer mu.Unlock()
	res := labResult{
		dur:             dur,
		sent:            sent,
		deliveredUnique: len(seen),
		deliveredBytes:  deliveredBytes,
		wireLossPct:     wsLoss * 100,
		parityRatioEnd:  parityEnd,
	}
	if dur > 0 {
		res.goodputMbps = float64(deliveredBytes*8) / dur.Seconds() / 1e6
	}
	if encStats.DataBytes > 0 {
		res.overheadPct = float64(encStats.ParityBytes) / float64(encStats.DataBytes) * 100
	}
	if sent > 0 {
		res.residualPct = float64(sent-len(seen)) / float64(sent) * 100
	}
	res.p50, res.p95, res.p99, res.maxMs = pctl(lat, .5), pctl(lat, .95), pctl(lat, .99), pctl(lat, 1)
	res.jitterMs = res.p95 - res.p50
	return res
}

func pctl(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(p * float64(len(s)-1))
	return s[i]
}

// TestLabBursty26 is the real-path profile: ~26% loss in bursts, low jitter.
// FEC should rebuild almost all of it — residual loss near zero — at the cost
// of significant parity overhead, exactly the tradeoff this carrier is for.
func TestLabBursty26(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping lab throughput test in -short")
	}
	// ~26% mean loss: 88% loss for ~12ms bursts, 2% otherwise, over a 20mbit,
	// 25ms one-way path with 1ms jitter.
	cfg := netsim.Config{
		ToServer:  netsim.NewTimeGE(1, 0.02, 0.88, 30, 12),
		ToClient:  netsim.NewTimeGE(2, 0.02, 0.88, 30, 12),
		DelayBase: 25 * time.Millisecond,
		Jitter:    1 * time.Millisecond,
		RateBits:  20e6,
		Queue:     300 * time.Millisecond,
	}
	res := runLab(t, cfg, 6*time.Second, 1100, nil)
	res.log(t, "bursty-26")
	if res.wireLossPct < 12 {
		t.Fatalf("expected a substantially lossy path, got %.1f%%", res.wireLossPct)
	}
	// FEC should recover most of the burst loss: residual well below the wire
	// loss. The absolute residual on this profile lands around 6-9% with the
	// path kept full (the pre-2026-09 controller left ~30% of this path idle
	// and landed at 4-10%, which made a 7% bar flaky).
	if res.residualPct > res.wireLossPct*0.4 {
		t.Fatalf("FEC recovered too little: residual %.2f%% vs wire %.1f%%", res.residualPct, res.wireLossPct)
	}
	if res.residualPct > 14 {
		t.Fatalf("residual loss too high: %.2f%%", res.residualPct)
	}
	// The rate controller keeps the 20 Mbit/s bottleneck full: with ~140%
	// parity that is ~7.5-8 Mbit/s of goodput (the old controller left the
	// path a third idle and got 4.6-5.8). Filling the path exposes more
	// packets to the bursts, so more ride FEC recovery — the tail latency
	// below is that recovery time, not pacing queue (the sim suite holds the
	// pacing queue near 10 ms), and it is the cost of recovering 26% loss at
	// the full rate rather than leaving bandwidth unused.
	if res.goodputMbps < 6.5 {
		t.Fatalf("goodput %.2f Mbit/s: the bottleneck is not being filled", res.goodputMbps)
	}
	// Tail latency is dominated by FEC recovery of the bursts (a rebuilt
	// packet waits up to the decoder ttl), which grows with how much of the
	// path is used; on this profile p95-p50 runs ~90-260 ms. The pacing queue
	// itself stays near the 10 ms target (the sim suite asserts that directly).
	if res.jitterMs > 320 {
		t.Fatalf("jitter too high even for FEC recovery: %.1f ms", res.jitterMs)
	}
}

// TestLabAdaptiveStep sends into a path whose loss jumps from ~5% to ~50%
// halfway through, and checks the adaptive FEC raises its parity in response
// and still keeps residual loss bounded.
func TestLabAdaptiveStep(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping lab throughput test in -short")
	}
	cfg := netsim.Config{
		ToServer:  netsim.NewTimeGE(3, 0.01, 0.30, 60, 6), // ~5% mean
		ToClient:  netsim.NewTimeGE(4, 0.01, 0.30, 60, 6),
		DelayBase: 25 * time.Millisecond,
		Jitter:    1 * time.Millisecond,
		RateBits:  20e6,
		Queue:     300 * time.Millisecond,
	}
	res := runLab(t, cfg, 8*time.Second, 1100, func(r *netsim.Relay) {
		// jump to ~50% bursty loss on the data direction
		r.SetLoss(netsim.NewTimeGE(5, 0.05, 0.95, 20, 20), nil)
	})
	res.log(t, "adaptive-5to50")
	if res.parityRatioEnd < 0.3 {
		t.Fatalf("FEC did not raise parity after the loss jump: r/k=%.2f", res.parityRatioEnd)
	}
	// The run spans the transition, so residual is higher than steady state;
	// at ~50% bursty loss the parity ceiling (1.5·k) leaves residual loss
	// whatever the pacing, and a controller that keeps sending through the
	// lossy half (more goodput) weights it more. FEC must still recover at
	// least half of the wire loss.
	if res.residualPct > res.wireLossPct*0.6 || res.residualPct > 20 {
		t.Fatalf("residual loss too high after adaptation: %.3f%% (wire %.1f%%)", res.residualPct, res.wireLossPct)
	}
}

// TestLabOverheadLowVsHigh reports the FEC overhead at low and high loss to
// show it tracks the loss (adaptive), not a fixed cost.
func TestLabOverheadLowVsHigh(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping lab throughput test in -short")
	}
	low := runLab(t, netsim.Config{
		ToServer:  netsim.NewTimeGE(11, 0.005, 0.20, 80, 4), // ~1-2% mean
		ToClient:  netsim.NewTimeGE(12, 0.005, 0.20, 80, 4),
		DelayBase: 20 * time.Millisecond, Jitter: 1 * time.Millisecond,
		RateBits: 20e6, Queue: 300 * time.Millisecond,
	}, 5*time.Second, 1100, nil)
	low.log(t, "overhead-low-loss")

	high := runLab(t, netsim.Config{
		ToServer:  netsim.NewTimeGE(13, 0.05, 0.9, 25, 18), // ~40% mean
		ToClient:  netsim.NewTimeGE(14, 0.05, 0.9, 25, 18),
		DelayBase: 20 * time.Millisecond, Jitter: 1 * time.Millisecond,
		RateBits: 20e6, Queue: 300 * time.Millisecond,
	}, 5*time.Second, 1100, nil)
	high.log(t, "overhead-high-loss")

	if !(high.overheadPct > low.overheadPct) {
		t.Fatalf("overhead did not rise with loss: low=%.0f%% high=%.0f%%", low.overheadPct, high.overheadPct)
	}
	t.Logf("FEC overhead adapts: %.0f%% at %.1f%% loss -> %.0f%% at %.1f%% loss",
		low.overheadPct, low.wireLossPct, high.overheadPct, high.wireLossPct)
}
