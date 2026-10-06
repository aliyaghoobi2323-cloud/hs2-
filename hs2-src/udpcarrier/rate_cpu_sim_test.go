package udpcarrier

// A pool simulator whose SENDER is the bottleneck (its CPU, the socket all
// carriers share): a send stage of limited capacity serves the carriers'
// pacers, round robin or a time slice at a time. It has
//   - TCP-like (Reno) flows inside the tunnel, so the fair queue's drops
//     shape the offered load as they do for real (not an always-full queue);
//   - the engine's per-carrier fair queue: per-flow FIFOs served round robin,
//     a sparse flow (the ping) first, head drop from the fattest flow when
//     full, the sojourn drop at dequeue;
//   - the single writer per carrier, which blocks on a bulk packet until the
//     pacer has room (a ping queued behind it waits for that);
//   - the pacer's three lanes (parity, fast, data), its byte budget and the
//     64-slot channel, the 2 ms token cap;
//   - design options: lw (late-wake credit), pt (the data lane bounded by its
//     measured sojourn), the fair queue's sojourn bound, fq off.

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// cpuStep: from at on, the sender's send stage carries bps (all carriers
// together; 0: unlimited).
type cpuStep struct {
	at  time.Duration
	bps float64
}

// pct is the p-quantile of v (sorts v).
func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	return v[int(p*float64(len(v)-1))]
}

type cpuPoolOpts struct {
	lw      float64 // ms: a backlogged pacer keeps credit for up to this long (0: today's 2 ms cap)
	pt      float64 // ms: > 0: the writer also waits while the data lane's last dequeued item waited longer than this
	sojourn float64 // fair-queue sojourn bound, ms (0: 50)
	fqOff   bool    // HS2_DG_FQ=0: one FIFO, no fast lane
	wb      bool    // the writer waits for pacer room BEFORE it pops (a sparse packet is never stuck behind a bulk one the writer holds)
	pb      float64 // > 0: the pacer's byte budget uses min(rate, pb x the carrier's recent send rate) instead of the rate
}

type cpuPoolCarrier struct {
	start   time.Duration
	flows   int // TCP flows on it
	sat     bool
	ping    bool
	fbPhase float64
	stopAt  time.Duration // flows stop then
}

type cpuPoolCfg struct {
	rules     bool
	capBps    float64
	oneWay    time.Duration
	buffer    time.Duration
	cpu       []cpuStep
	slice     float64
	parity    int
	pingEvery float64 // ms
	dur, warm time.Duration
	win       [2]float64 // a second window (ms)
	winA      [2]float64 // the first window (ms); 0: [warm, dur)
	opt       cpuPoolOpts
	rwnd      float64 // packets
	// stalls: every stallEvery ms the whole sender (writer and pacer) stops
	// for stallMs (the process descheduled): 0 = none.
	stallEvery, stallMs float64
	// bursty cross traffic on the path (the other tunnel)
	crossBurstBps, crossBurstMs, crossEveryMs float64
	trace                                     func(i int, nowMs float64, rc *rateControl)
}

type cpuPoolPhase struct {
	mbit       []float64 // per carrier
	tot        float64
	jainFlows  float64
	bulkQ50    float64 // fq push -> sent, ms
	bulkQ99    float64
	pacerQ50   float64 // pacer enqueue -> sent, ms
	pacerQ99   float64
	pingS50    float64 // ping: fq push -> sent (sender internal), ms
	pingS99    float64
	pingSMax   float64
	pingE99    float64 // ping: fq push -> arrival at the peer minus one way, ms
	pathQ99    float64
	pathQMax   float64
	pathDrops  int
	fqAged     int
	fqFull     int
	tcpRTT     float64 // mean smoothed RTT of the flows at the window's end, ms
	inStartup  int
	rOverAct   []float64
	rate, btl  []float64
	exitMs     []float64
	carriers   int
	stageExits int
	pingLost   int
}

func (p cpuPoolPhase) String() string {
	s := fmt.Sprintf("%.1f Mbit/s jainF %.2f | bulk q p50 %.0f p99 %.0f (pacer p50 %.0f p99 %.0f) | ping send p50 %.1f p99 %.1f max %.1f, e2e-extra p99 %.1f | path q p99 %.0f max %.0f drops %d | fq aged %d full %d ping lost %d | tcp srtt %.0f | startup %d/%d r/act",
		p.tot, p.jainFlows, p.bulkQ50, p.bulkQ99, p.pacerQ50, p.pacerQ99, p.pingS50, p.pingS99, p.pingSMax, p.pingE99, p.pathQ99, p.pathQMax, p.pathDrops, p.fqAged, p.fqFull, p.pingLost, p.tcpRTT, p.inStartup, p.carriers)
	for _, r := range p.rOverAct {
		s += fmt.Sprintf(" %.1f", r)
	}
	return s
}

type cpuPoolRes struct{ a, b cpuPoolPhase } // a: [warm, dur) ; b: win

func runCPUPool(cfg cpuPoolCfg, cs []cpuPoolCarrier) cpuPoolRes {
	const pkt = 1200.0
	const pingSize = 150.0
	const step = 0.25 // ms
	const fqLen = 256
	const pqSlots = 64
	sojourn := cfg.opt.sojourn
	if sojourn == 0 {
		sojourn = 50
	}
	rwnd := cfg.rwnd
	if rwnd == 0 {
		rwnd = 3000
	}
	pingEvery := cfg.pingEvery
	if pingEvery == 0 {
		pingEvery = 20
	}
	peerOffset := 123456.789
	base := time.Unix(1_700_000_000, 0)
	at := func(ms float64) time.Time { return base.Add(time.Duration(ms * float64(time.Millisecond))) }
	ow := float64(cfg.oneWay / time.Millisecond)
	capB := cfg.capBps / 8 / 1000 // bytes per ms
	endMs, warmMs := float64(cfg.dur/time.Millisecond), float64(cfg.warm/time.Millisecond)

	type qp struct {
		t, enqT  float64
		flow     int // -1: ping, -2: parity, -3: saturating source
		seq      int
		size     float64
		ping     bool
		parity   bool
		pingSent float64
	}
	type flow struct {
		car                 int
		cwnd, ssthresh      float64
		nextSeq, inflight   int
		out                 map[int]bool
		lost                []int
		recover             int
		lastAck             float64
		srtt                float64
		sendT               map[int]float64
		deliveredA, delivWB float64
		active              bool
		wmax, epoch, k      float64 // CUBIC
	}
	type fqFlow struct {
		q []qp
	}
	type report struct {
		arrive  float64
		rtt     float64
		loss    uint32
		echo    int64
		rx      uint64
		owd     uint32
		haveOWD bool
	}
	type car struct {
		rc            *rateControl
		fq            map[int]*fqFlow
		rr            []int // flows with a backlog, round robin
		sparse        []qp
		n             int
		pending       *qp
		in, pri, fast []qp
		queued        float64
		tokens        float64
		lastSend      float64
		inAge         float64
		dataN         int
		rx            uint64
		owdMin        float64
		haveOWD       bool
		nextFb        float64
		toUs          []report
		sentPrev      uint64
		lost, got     float64
		qSum          float64
		qN            int
		pushedT       bool
		fqDrops       int
		exitMs        float64
		wasStartup    bool
		delA, delB    float64
		nextPing      float64
		stageExit     bool
		satApp        float64
	}
	ps := make([]*car, len(cs))
	var flows []*flow
	simNow := 0.0 // the loop's clock, for stageLimited
	for i, c := range cs {
		ps[i] = &car{rc: newRateControl(), fq: map[int]*fqFlow{}, nextFb: float64(c.start/time.Millisecond) + 50 + c.fbPhase, exitMs: -1, wasStartup: true, nextPing: float64(c.start/time.Millisecond) + 7}
		ps[i].rc.fair = cfg.rules
		ps[i].rc.clock = func() time.Time { return at(simNow) }
		ps[i].rc.epoch = base
		for k := 0; k < c.flows; k++ {
			flows = append(flows, &flow{car: i, cwnd: 10, ssthresh: 1e9, out: map[int]bool{}, sendT: map[int]float64{}, srtt: 2 * ow})
		}
	}
	type inflight struct {
		from     int
		arrive   float64
		bytes    float64
		stamp    float64
		flow     int
		seq      int
		ping     bool
		pingPush float64
		sentAt   float64
	}
	type ack struct {
		flow   int
		seq    int
		arrive float64
	}
	var toPeer []inflight
	var acks []ack
	busyUntil := 0.0
	var pa, pb cpuPoolPhase
	var bulkQA, bulkQB, pacerQA, pacerQB, pingSA, pingSB, pingEA, pingEB, pathQA, pathQB []float64
	inA := func(now float64) bool { return now >= warmMs }
	spanA := endMs - warmMs
	if cfg.winA[1] > 0 {
		inA = func(now float64) bool { return now >= cfg.winA[0] && now < cfg.winA[1] }
		spanA = cfg.winA[1] - cfg.winA[0]
	}
	inB := func(now float64) bool { return cfg.win[1] > 0 && now >= cfg.win[0] && now < cfg.win[1] }
	shareB, meanB, busyQ, nextShare := 0.0, 0.0, 0.0, 500.0
	idleBusy := 0
	cpuBudget := 0.0
	crossAcc := 0.0
	nextSample := 0.0
	rrC := 0
	sliceEnd := 0.0

	loseFlow := func(p qp, now float64) {
		if p.flow < 0 {
			return
		}
		f := flows[p.flow]
		if f.out[p.seq] {
			f.lost = append(f.lost, p.seq)
		}
	}
	push := func(c *car, p qp, now float64) {
		if p.ping && !cfg.opt.fqOff {
			c.sparse = append(c.sparse, p)
			c.n++
			return
		}
		id := p.flow
		if cfg.opt.fqOff {
			id = 0
		}
		if c.n >= fqLen {
			if cfg.opt.fqOff {
				c.fqDrops++
				if inA(now) {
					pa.fqFull++
				}
				if inB(now) {
					pb.fqFull++
				}
				loseFlow(p, now)
				return
			}
			// head of the fattest flow
			var fat *fqFlow
			for _, g := range c.fq {
				if fat == nil || len(g.q) > len(fat.q) {
					fat = g
				}
			}
			if fat == nil || len(fat.q) == 0 {
				c.fqDrops++
				loseFlow(p, now)
				return
			}
			d := fat.q[0]
			fat.q = fat.q[1:]
			c.n--
			c.fqDrops++
			if inA(now) {
				pa.fqFull++
			}
			if inB(now) {
				pb.fqFull++
			}
			loseFlow(d, now)
		}
		g := c.fq[id]
		if g == nil {
			g = &fqFlow{}
			c.fq[id] = g
		}
		if len(g.q) == 0 {
			c.rr = append(c.rr, id)
		}
		g.q = append(g.q, p)
		c.n++
	}
	pop := func(c *car) *qp {
		if len(c.sparse) > 0 {
			p := c.sparse[0]
			c.sparse = c.sparse[1:]
			c.n--
			return &p
		}
		for len(c.rr) > 0 {
			id := c.rr[0]
			c.rr = c.rr[1:]
			g := c.fq[id]
			if g == nil || len(g.q) == 0 {
				continue
			}
			p := g.q[0]
			g.q = g.q[1:]
			c.n--
			if len(g.q) > 0 {
				c.rr = append(c.rr, id)
			}
			return &p
		}
		return nil
	}

	type snap struct {
		rate, btl []float64
		startup   []bool
		done      bool
	}
	var snA, snB snap
	takeSnap := func(sn *snap, now float64) {
		if sn.done {
			return
		}
		sn.done = true
		for _, c := range ps {
			sn.rate = append(sn.rate, c.rc.pacingRate(at(now))*8/1e6)
			sn.btl = append(sn.btl, c.rc.btlBw*8/1e6)
			sn.startup = append(sn.startup, c.rc.startup)
		}
	}
	for now := 0.0; now < endMs; now += step {
		simNow = now
		if cfg.winA[1] > 0 && now >= cfg.winA[1]-step {
			takeSnap(&snA, now)
		}
		if cfg.win[1] > 0 && now >= cfg.win[1]-step {
			takeSnap(&snB, now)
		}
		cpuBps := 0.0
		for _, s := range cfg.cpu {
			if now >= float64(s.at/time.Millisecond) {
				cpuBps = s.bps
			}
		}
		if cfg.crossEveryMs > 0 && math.Mod(now, cfg.crossEveryMs) < cfg.crossBurstMs {
			for crossAcc += cfg.crossBurstBps / 8 / 1000 * step; crossAcc >= pkt; crossAcc -= pkt {
				if s := math.Max(now, busyUntil); s-now <= float64(cfg.buffer/time.Millisecond) {
					busyUntil = s + pkt/capB
				}
			}
		}
		stalled := cfg.stallEvery > 0 && math.Mod(now, cfg.stallEvery) < cfg.stallMs
		if cpuBps > 0 {
			cpuBudget = math.Min(cpuBudget+cpuBps/8/1000*step, cpuBps/8/1000*step+pkt)
		}
		// ACKs reach the TCP senders.
		for len(acks) > 0 && acks[0].arrive <= now {
			a := acks[0]
			acks = acks[1:]
			f := flows[a.flow]
			if !f.out[a.seq] {
				continue
			}
			delete(f.out, a.seq)
			f.inflight--
			if st, ok := f.sendT[a.seq]; ok {
				f.srtt += 0.125 * ((now - st) - f.srtt)
				delete(f.sendT, a.seq)
			}
			f.lastAck = now
			// loss detection: a lost seq 3 below this one
			keep := f.lost[:0]
			for _, l := range f.lost {
				if l < a.seq-3 {
					if f.out[l] {
						delete(f.out, l)
						delete(f.sendT, l)
						f.inflight--
					}
					if l >= f.recover {
						// CUBIC: beta 0.7, C 0.4
						f.wmax = f.cwnd
						f.cwnd = math.Max(f.cwnd*0.7, 2)
						f.ssthresh = f.cwnd
						f.epoch = now
						f.k = math.Cbrt(f.wmax * 0.3 / 0.4)
						f.recover = f.nextSeq
					}
				} else {
					keep = append(keep, l)
				}
			}
			f.lost = keep
			if f.cwnd < f.ssthresh {
				f.cwnd++
			} else {
				if f.epoch == 0 {
					f.epoch, f.wmax, f.k = now, f.cwnd, 0
				}
				tt := (now - f.epoch + f.srtt) / 1000
				target := 0.4*math.Pow(tt-f.k, 3) + f.wmax
				// TCP-friendly region
				west := f.wmax*0.7 + 3*0.3/1.7*((now-f.epoch)/math.Max(f.srtt, 1))
				target = math.Max(target, west)
				if target > f.cwnd {
					f.cwnd += math.Min(target-f.cwnd, f.cwnd) / f.cwnd
				} else {
					f.cwnd += 0.01 / f.cwnd
				}
			}
			f.cwnd = math.Min(f.cwnd, rwnd)
		}
		// TCP senders and the ping push into their carrier's fair queue.
		for fi, f := range flows {
			c := ps[f.car]
			sc := cs[f.car]
			f.active = now >= float64(sc.start/time.Millisecond) && (sc.stopAt == 0 || now < float64(sc.stopAt/time.Millisecond))
			if !f.active {
				continue
			}
			if f.inflight > 0 && now-f.lastAck > math.Max(1000, 3*f.srtt) {
				// RTO: everything outstanding is forgotten
				f.ssthresh = math.Max(f.cwnd*0.7, 2)
				f.wmax, f.epoch, f.k = f.cwnd, 0, 0
				f.cwnd = 1
				f.out = map[int]bool{}
				f.sendT = map[int]float64{}
				f.lost = nil
				f.inflight = 0
				f.recover = f.nextSeq
				f.lastAck = now
			}
			for float64(f.inflight) < math.Floor(f.cwnd) {
				seq := f.nextSeq
				f.nextSeq++
				f.out[seq] = true
				f.sendT[seq] = now
				if f.inflight == 0 {
					f.lastAck = now
				}
				f.inflight++
				push(c, qp{t: now, flow: fi, seq: seq, size: pkt}, now)
			}
		}
		for i, sc := range cs {
			c := ps[i]
			if now < float64(sc.start/time.Millisecond) || (sc.stopAt > 0 && now >= float64(sc.stopAt/time.Millisecond)) {
				continue
			}
			if sc.sat {
				for c.n < fqLen {
					push(c, qp{t: now, flow: -3, size: pkt}, now)
				}
			}
			if sc.ping && now >= c.nextPing {
				c.nextPing += pingEvery
				push(c, qp{t: now, flow: -1, size: pingSize, ping: true}, now)
			}
		}
		// Writers (one per carrier): pop, sojourn check, hand to the pacer.
		for i := range cs {
			c := ps[i]
			if stalled {
				break
			}
			for {
				if c.pending == nil {
					if cfg.opt.wb && len(c.sparse) == 0 {
						// pop a bulk packet only once the pacer has room for it
						budget := math.Max(budgetRate(c.rc, at(now), cfg.opt.pb)*pacerQueueTime.Seconds(), pacerQueueMin)
						room := len(c.in) < pqSlots && c.queued <= budget
						if cfg.opt.pt > 0 && len(c.in) > 0 && c.inAge > cfg.opt.pt {
							room = false
						}
						if !room {
							break
						}
					}
					p := pop(c)
					if p == nil {
						break
					}
					if now-p.t > sojourn {
						if p.ping {
							if inA(now) {
								pa.pingLost++
							}
							if inB(now) {
								pb.pingLost++
							}
							continue
						}
						c.fqDrops++
						if inA(now) {
							pa.fqAged++
						}
						if inB(now) {
							pb.fqAged++
						}
						loseFlow(*p, now)
						continue
					}
					c.pending = p
				}
				p := c.pending
				p.enqT = now
				if p.ping && !cfg.opt.fqOff {
					c.fast = append(c.fast, *p)
					c.queued += p.size + 9
					c.pending = nil
					continue
				}
				budget := math.Max(budgetRate(c.rc, at(now), cfg.opt.pb)*pacerQueueTime.Seconds(), pacerQueueMin)
				room := len(c.in) < pqSlots && c.queued <= budget
				if cfg.opt.pt > 0 && len(c.in) > 0 && c.inAge > cfg.opt.pt {
					room = false
				}
				if !room {
					break
				}
				c.in = append(c.in, *p)
				c.queued += p.size + 9
				c.pending = nil
				if cfg.parity > 0 {
					if c.dataN++; c.dataN%cfg.parity == 0 {
						c.pri = append(c.pri, qp{t: now, enqT: now, size: pkt, parity: true, flow: -2})
						c.queued += pkt + 9
					}
				}
			}
			// tokens
			rate := c.rc.pacingRate(at(now)) / 1000
			cp := math.Max(2*(pkt+64), rate*pacerQuantum.Seconds()*1000)
			backlog := len(c.in)+len(c.pri)+len(c.fast) > 0
			if cfg.opt.lw > 0 && backlog && c.rc.stageLimited() { // as the pacer: only a carrier its send stage holds back
				cp = math.Max(cp, rate*math.Min(now-c.lastSend, cfg.opt.lw))
			}
			c.tokens = math.Min(c.tokens+rate*step, cp)
			if !backlog {
				c.lastSend = now
			}
		}
		// The send stage.
		send := func(i int) bool {
			c := ps[i]
			var it qp
			lane := 0
			switch {
			case len(c.pri) > 0:
				it, lane = c.pri[0], 0
			case len(c.fast) > 0:
				it, lane = c.fast[0], 1
			case len(c.in) > 0:
				it, lane = c.in[0], 2
			default:
				return false
			}
			if c.tokens < it.size || (cpuBps > 0 && cpuBudget < it.size) {
				return false
			}
			switch lane {
			case 0:
				c.pri = c.pri[1:]
			case 1:
				c.fast = c.fast[1:]
			case 2:
				c.in = c.in[1:]
				c.inAge = now - it.enqT
				if len(c.in) == 0 {
					c.inAge = 0
				}
			}
			c.queued -= it.size + 9
			c.lastSend = now
			if !it.parity && !it.ping {
				if inA(now) {
					bulkQA = append(bulkQA, now-it.t)
					pacerQA = append(pacerQA, now-it.enqT)
				}
				if inB(now) {
					bulkQB = append(bulkQB, now-it.t)
					pacerQB = append(pacerQB, now-it.enqT)
				}
			}
			if it.ping {
				if inA(now) {
					pingSA = append(pingSA, now-it.t)
				}
				if inB(now) {
					pingSB = append(pingSB, now-it.t)
				}
			}
			c.tokens -= it.size
			if cpuBps > 0 {
				cpuBudget -= it.size
			}
			c.rc.onSent(int(it.size))
			start := math.Max(now, busyUntil)
			if start-now > float64(cfg.buffer/time.Millisecond) {
				if inA(now) {
					pa.pathDrops++
				}
				if inB(now) {
					pb.pathDrops++
				}
				c.lost += it.size
				loseFlow(it, now)
				return true
			}
			busyUntil = start + it.size/capB
			toPeer = append(toPeer, inflight{from: i, arrive: busyUntil + ow, bytes: it.size, stamp: now, flow: it.flow, seq: it.seq, ping: it.ping, pingPush: it.t, sentAt: now})
			return true
		}
		if !stalled {
			if cfg.slice > 0 && cpuBps > 0 {
				for tries := 0; tries <= len(cs); {
					i := rrC % len(cs)
					if now < sliceEnd && send(i) {
						tries = 0
						continue
					}
					if cpuBudget < pkt {
						break
					}
					rrC++
					sliceEnd = now + cfg.slice
					tries++
				}
			} else {
				for progress := true; progress; {
					progress = false
					for k := 0; k < len(cs); k++ {
						if send((rrC + k) % len(cs)) {
							progress = true
						}
					}
					rrC++
				}
			}
		}
		if now >= nextSample {
			nextSample = now + 1
			q := math.Max(0, busyUntil-now)
			if inA(now) {
				pathQA = append(pathQA, q)
			}
			if inB(now) {
				pathQB = append(pathQB, q)
			}
		}
		// Arrivals at the peer.
		for len(toPeer) > 0 && toPeer[0].arrive <= now {
			d := toPeer[0]
			toPeer = toPeer[1:]
			c := ps[d.from]
			if o := now + peerOffset - d.stamp; !c.haveOWD || o < c.owdMin {
				c.owdMin, c.haveOWD = o, true
			}
			c.rx += uint64(d.bytes)
			c.got += d.bytes
			if d.ping {
				if inA(d.sentAt) {
					pingEA = append(pingEA, now-d.pingPush-ow)
				}
				if inB(d.sentAt) {
					pingEB = append(pingEB, now-d.pingPush-ow)
				}
			}
			if d.flow >= 0 {
				acks = append(acks, ack{flow: d.flow, seq: d.seq, arrive: now + ow})
			}
			if d.flow != -2 && !d.ping {
				if inA(now) {
					c.delA += d.bytes
					if d.flow >= 0 {
						flows[d.flow].deliveredA += d.bytes
					}
				}
				if inB(now) {
					c.delB += d.bytes
					if d.flow >= 0 {
						flows[d.flow].delivWB += d.bytes
					}
				}
			}
		}
		// The Governor (as in cpu_sim_test.go).
		if now >= nextShare {
			nextShare += 500
			n, sum, na, asum := 0, 0.0, 0, 0.0
			var bq []float64
			for _, c := range ps {
				sent := c.rc.sent.Load()
				r := float64(sent-c.sentPrev) / 0.5
				c.sentPrev = sent
				q, qn, pushed := c.qSum, c.qN, c.pushedT
				c.qSum, c.qN, c.pushedT = 0, 0, false
				if qn == 0 {
					continue
				}
				if r >= govActiveRate {
					na++
					asum += r
				}
				if r >= govActiveRate && pushed {
					n++
					sum += r
					bq = append(bq, q/float64(qn))
				}
			}
			if na > 0 {
				if meanB == 0 {
					meanB = asum / float64(na)
				}
				meanB += 0.5 * (asum/float64(na) - meanB)
			} else {
				meanB = 0
			}
			if n >= 2 {
				if shareB == 0 {
					shareB = sum / float64(n)
				}
				shareB += 0.5 * (sum/float64(n) - shareB)
				idleBusy = 0
			} else if idleBusy++; idleBusy > govShareHold {
				shareB = 0
			}
			if len(bq) > 0 {
				sort.Float64s(bq)
				busyQ = bq[len(bq)/2]
			} else if idleBusy > govShareHold {
				busyQ = 0
			}
		}
		// Feedback.
		for i, sc := range cs {
			c := ps[i]
			if now < float64(sc.start/time.Millisecond) {
				continue
			}
			if now >= c.nextFb {
				c.nextFb += 100
				r := report{arrive: now + ow, rx: c.rx, rtt: (2*ow + math.Max(0, busyUntil-now)) / 1000, echo: int64(now*1000)*64 + int64(i) + 1}
				if c.lost+c.got > 0 {
					r.loss = uint32(c.lost / (c.lost + c.got) * 1e6)
				}
				c.lost, c.got = 0, 0
				if c.haveOWD {
					r.owd, r.haveOWD = uint32(int64(c.owdMin*8)), true
					c.haveOWD = false
				}
				c.toUs = append(c.toUs, r)
			}
			for len(c.toUs) > 0 && c.toUs[0].arrive <= now {
				r := c.toUs[0]
				c.toUs = c.toUs[1:]
				c.rc.setShare(shareB, meanB, busyQ)
				// the A1 signal: the carrier's own fair queue dropped since the last report
				// the pool notes each drop of the carrier's send queue (dgLink.noteQueueDrop)
				for ; c.fqDrops > 0; c.fqDrops-- {
					c.rc.noteStageDrop()
				}
				wasS := c.rc.startup
				c.rc.onFeedback(at(now), r.rx, r.rtt, r.loss, r.echo, r.owd, r.haveOWD)
				if wasS && !c.rc.startup && c.rc.stageRate > 0 {
					c.stageExit = true
				}
				c.qSum += c.rc.queueSec()
				c.qN++
				c.pushedT = c.pushedT || c.rc.pushing.Load()
				if c.wasStartup && !c.rc.startup {
					c.wasStartup, c.exitMs = false, now
				}
				if cfg.trace != nil {
					cfg.trace(i, now, c.rc)
				}
			}
		}
	}
	takeSnap(&snA, endMs)
	takeSnap(&snB, endMs)
	fill := func(ph *cpuPoolPhase, sn snap, span float64, del func(c *car) float64, fdel func(f *flow) float64, bq, pq, ping, pingE, pathQ []float64) {
		for i, c := range ps {
			m := del(c) * 8 / span / 1000
			ph.mbit = append(ph.mbit, m)
			ph.tot += m
			ph.rate = append(ph.rate, sn.rate[i])
			ph.btl = append(ph.btl, sn.btl[i])
			ph.rOverAct = append(ph.rOverAct, sn.rate[i]/math.Max(m, 0.01))
			if sn.startup[i] && (cs[i].flows > 0 || cs[i].sat) {
				ph.inStartup++
			}
			if cs[i].flows > 0 || cs[i].sat {
				ph.carriers++
			}
			ph.exitMs = append(ph.exitMs, c.exitMs)
			if c.stageExit {
				ph.stageExits++
			}
		}
		s, s2, n := 0.0, 0.0, 0
		rtt := 0.0
		for _, f := range flows {
			if !f.active {
				continue
			}
			x := fdel(f)
			s += x
			s2 += x * x
			n++
			rtt += f.srtt
		}
		if s2 > 0 {
			ph.jainFlows = s * s / (float64(n) * s2)
		}
		if n > 0 {
			ph.tcpRTT = rtt / float64(n)
		}
		ph.bulkQ50, ph.bulkQ99 = pct(append([]float64(nil), bq...), 0.5), pct(bq, 0.99)
		ph.pacerQ50, ph.pacerQ99 = pct(append([]float64(nil), pq...), 0.5), pct(pq, 0.99)
		ph.pingS50, ph.pingS99, ph.pingSMax = pct(append([]float64(nil), ping...), 0.5), pct(append([]float64(nil), ping...), 0.99), pct(ping, 1)
		ph.pingE99 = pct(pingE, 0.99)
		ph.pathQ99, ph.pathQMax = pct(append([]float64(nil), pathQ...), 0.99), pct(pathQ, 1)
	}
	fill(&pa, snA, spanA, func(c *car) float64 { return c.delA }, func(f *flow) float64 { return f.deliveredA }, bulkQA, pacerQA, pingSA, pingEA, pathQA)
	if cfg.win[1] > 0 {
		fill(&pb, snB, cfg.win[1]-cfg.win[0], func(c *car) float64 { return c.delB }, func(f *flow) float64 { return f.delivWB }, bulkQB, pacerQB, pingSB, pingEB, pathQB)
	}
	return cpuPoolRes{pa, pb}
}

func budgetRate(rc *rateControl, now time.Time, pb float64) float64 {
	r := rc.pacingRate(now)
	if pb > 0 && rc.sendAvg > 0 && !cpuPoolPBAlways && rc.fair && !rc.startup && !rc.pushing.Load() {
		// out of startup, not using its allowance: bound the queue by what it sends
		r = math.Min(r, math.Max(pb*rc.sendAvg, rc.minRate))
	} else if pb > 0 && rc.sendAvg > 0 && cpuPoolPBAlways {
		r = math.Min(r, math.Max(pb*rc.sendAvg, rc.btlBw))
	}
	return r
}

var cpuPoolPBAlways bool
