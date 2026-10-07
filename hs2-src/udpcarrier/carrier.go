package udpcarrier

import (
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/fec"
)

// dgPadDisabled turns off datagram size-bucket padding (B6) when HS2_DG_PAD=0,
// for a user who wants the last few percent of goodput over size shaping. On by
// default; it only pads small/medium frames, never bulk, so the cost is bounded.
var dgPadDisabled = os.Getenv("HS2_DG_PAD") == "0"

// icmpCamo turns on the ICMP traffic-shape camouflage (phase CA1). It changes
// only SEND TIMING, not the wire format, so the two ends need not agree and an
// old peer is unaffected:
//   - the feedback cadence is jittered instead of a fixed 100 ms tick, so the
//     flow carries no sharp ~10 Hz spectral line for a filter to key on;
//   - an idle carrier (nothing received to report) falls quiet instead of
//     beating 10 times a second with nothing to carry — keepalive (jittered,
//     ~5 s, well inside deadAfter) keeps the link alive. This is the case a
//     near-idle tunnel is most exposed in: at idle the only packets WERE the
//     feedback beat.
//
// It does not remove the receiver->sender feedback cadence during an active
// download (that direction is mostly feedback, needed by the rate control); it
// only jitters it. Set HS2_ICMP_CAMO=1 on both ends of an icmp tunnel.
var icmpCamo = os.Getenv("HS2_ICMP_CAMO") == "1"

// camoIdlePoll is how often a quiet carrier wakes to notice traffic resumed
// (it sends nothing while idle). Short enough that feedback resumes promptly
// for the rate control, far below deadAfter.
const camoIdlePoll = 700 * time.Millisecond

// camoJitter returns d scaled by a uniform random factor in [1-frac, 1+frac].
// Used for send timing only (per carrier, local), so a plain PRNG is fine.
func camoJitter(d time.Duration, frac float64) time.Duration {
	return time.Duration(float64(d) * (1 - frac + 2*frac*rand.Float64()))
}

// deadAfter mirrors the engine's own liveness timeout: if no frame arrives for
// this long the link is considered dead and ReadFrame returns an error so the
// engine tears the carrier down and reconnects. Keepalives (both ends ping
// every ~5s) keep it well inside this window on a live link.
const (
	deadAfter     = 15 * time.Second
	feedbackEvery = 100 * time.Millisecond
	flushEvery    = 10 * time.Millisecond
	expireEvery   = 50 * time.Millisecond
)

var (
	errClosed      = errors.New("udpcarrier: carrier closed")
	errDeadLink    = errors.New("udpcarrier: link idle past deadline")
	errConfirmFail = errors.New("udpcarrier: peer failed key confirmation")
)

type frameMsg struct {
	ftype   byte
	payload []byte
}

// Conn is one UDP carrier link. It implements the engine's Carrier interface
// (SendFrame/ReadFrame/Close) structurally, so the engine drives it exactly
// like the TCP and reality carriers, unaware that underneath it is Noise over
// FEC over UDP.
type Conn struct {
	sess     *core.Session
	shared   []byte
	binding  []byte
	kind     string // encap kind ("" / "udp" / "icmp" / ...); lets a pool shape ICMP
	innerMTU int    // tunnel MTU; the ceiling for datagram size-bucket padding (B6)

	enc     *fec.Encoder
	dec     *fec.Decoder
	adapter *fec.Adapter
	rc      *rateControl
	pacer   *pacer

	write   func([]byte) error // one UDP datagram to the peer
	onClose func()             // listener-side deregistration; nil for dialer
	rawConn interface{ Close() error }

	rx     chan rxPkt    // inbound datagrams awaiting FEC decode
	frames chan frameMsg // decoded hs2 frames awaiting ReadFrame
	confCh chan []byte   // received key-confirmation tags (setup only)
	done   chan struct{}

	// Data and control sends are serialised separately: a data send can block
	// on the pacer's full queue while holding sendMu, and a control frame
	// (feedback above all) must not wait behind it — its timing is what the
	// peer's delay measurement reads. Sealing itself is concurrency-safe.
	sendMu   sync.Mutex
	sendScr  []byte // reused data seq||ct scratch, guarded by sendMu
	closeOne sync.Once

	// liveness
	lastRxMono atomic.Int64 // when anything was last received: ns since monoBase

	// inbound datagrams the listener dropped because this carrier's queue was
	// full (tryFeed)
	rxDropped atomic.Uint64

	// the pool's governor (nil outside a pool): shared cap, pool-wide loss
	gov atomic.Pointer[Governor]

	// the FEC adapter's current loss estimate (float bits), for status
	lossEst atomic.Uint64

	// feedback we send (describes what WE receive)
	rxDataBytes atomic.Uint64
	echoMu      sync.Mutex
	peerSend    int64     // last peer feedback sendNanos, for our echo
	peerRecvAt  time.Time // when we received it, for the echo delay

	// decoder stats snapshot, published by the decode goroutine (the only user
	// of the decoder) so the feedback loop and Stats can read them race-free.
	decStats atomic.Pointer[fec.DecoderStats]

	// last peer feedback timestamp seen, for ordering (processLoop only)
	lastFbNanos int64

	// Stamped data (tagDataTS): peerStamps is set once the peer's feedback
	// shows it understands them (the pacer then stamps every data datagram);
	// owdMin is the minimum one-way delay of the peer's stamped data this
	// feedback interval, as 1<<32|ticks (0 = none yet), written by the decode
	// loop and swapped out by the feedback loop.
	peerStamps atomic.Bool
	owdMin     atomic.Uint64

	// wire-loss measurement, reorder-tolerant. The decode loop (trackWire)
	// slides a window [wireBase, wireBase+wireConfirmWin) over the per-datagram
	// wire sequence and resolves each seq exactly once: RECEIVED when it arrives
	// while inside the window, or LOST when the window slides past it still
	// missing. A datagram that is merely REORDERED — late but within the window
	// — is counted received, never lost; only a genuine gap counts as loss. The
	// feedback loop reads the two cumulative counters and takes their delta.
	// (The old estimator read the high-water mark minus the receive count per
	// window, which booked a reordered early arrival as up to 100% phantom loss
	// and never credited the catch-up back.)
	wireRecv                   atomic.Uint64               // distinct datagrams received (resolved)
	wireLost                   atomic.Uint64               // datagrams the window passed, never seen
	wireBytes                  atomic.Uint64               // bytes of data datagrams received
	wirePrimed                 bool                        // decode loop only
	wireBase                   uint32                      // decode loop only: oldest unresolved seq
	wireMax                    uint32                      // decode loop only: highest seq seen
	wireSeen                   [wireReorderWin / 64]uint64 // decode loop only: arrivals
	lastWireRecv, lastWireLost uint64                      // feedback loop only

	wg sync.WaitGroup
}

func newConn(sess *core.Session, write func([]byte) error, shared, binding []byte, innerMTU int, rawConn interface{ Close() error }, onClose func()) *Conn {
	maxPayload := innerMTU + 64
	// K=8 with a 40 ms window: the fec sweep (fec/sweep_test.go) shows this
	// keeps residual loss ~3-4% at ~1000 pps on the 26% bursty profile, because
	// a small group closed over a wider window interleaves a burst across enough
	// groups. A larger K needs a far higher packet rate to interleave at all.
	fcfg := fec.Config{MaxPayload: maxPayload, K: 8, Window: 60 * time.Millisecond, MaxDepth: 64}
	rc := newRateControl()
	c := &Conn{
		sess:     sess,
		shared:   shared,
		binding:  binding,
		innerMTU: innerMTU,
		enc:      fec.NewEncoder(fcfg),
		adapter:  fec.NewAdapter(fec.AdapterConfig{}),
		rc:       rc,
		write:    write,
		onClose:  onClose,
		rawConn:  rawConn,
		rx:       make(chan rxPkt, 1024),
		frames:   make(chan frameMsg, 2048),
		confCh:   make(chan []byte, 4),
		done:     make(chan struct{}),
	}
	// ttl a bit over the window so a group waits long enough for its parity but
	// a permanently lost group is given up on quickly (bounding recovery delay).
	c.dec = fec.NewDecoder(220*time.Millisecond, c.enc.Config().MaxPayload+2)
	// Shallow pacer queue: it holds ~one BDP so backpressure reaches the sender
	// fast and per-packet queueing latency stays small.
	c.pacer = newPacer(rc, write, 64, &c.peerStamps)
	c.pacer.gov = &c.gov
	c.lastRxMono.Store(int64(time.Since(monoBase)))
	c.wg.Add(3)
	go c.processLoop()
	go c.feedbackLoop()
	go c.flushLoop()
	return c
}

// Datagram tags demultiplex the two kinds of UDP datagram on the wire: bulk
// data goes through FEC and is paced; small control frames (feedback, key
// confirmation, keepalives) are sent raw and unpaced so their timing is clean —
// feedback that was delayed and reordered by FEC recovery made the rate and RTT
// estimates useless.
const (
	tagData   = 0x00 // [tag][wireSeq:4] then one FEC shard packet
	tagCtrl   = 0x01 // followed by one sealed control frame (seq||ciphertext)
	tagDataTS = 0x02 // [tag][wireSeq:4][sendStamp:4] then one FEC shard packet
)

// The reorder-tolerant loss estimator holds a sliding window over the wire
// sequence. wireConfirmWin is how far the highest seq must advance beyond a
// still-missing seq before that seq is counted lost — i.e. how much reordering
// is tolerated (a datagram that arrives within this many seqs is credited as
// received, not lost) and, equally, the loss-confirmation lag. It is a balance:
// large enough to absorb real path reorder (so jitter/reorder is never booked
// as the phantom loss the old high-water estimator produced), small enough that
// a genuine loss burst is confirmed well inside a loss episode so the adaptive
// FEC still sizes parity for it in time. A real path's reorder is a handful of
// packets; 48 covers that with margin while confirming loss within ~20 ms at
// the rates where parity sizing matters. wireReorderWin sizes the arrival
// bitmap and must be >= wireConfirmWin (a power of two keeps the index cheap).
const (
	wireConfirmWin = 64
	wireReorderWin = 256
)

// SendFrame is the engine's send path. TypeData rides FEC and the pacer;
// everything else (keepalives, and internally feedback/auth) is a raw control
// datagram.
func (c *Conn) SendFrame(ftype byte, payload []byte) error {
	if ftype == core.TypeData {
		return c.sendData(payload)
	}
	return c.sendControl(ftype, payload)
}

// setWriteBatch gives the pacer a several-datagrams-per-syscall sender.
func (c *Conn) setWriteBatch(fn func([][]byte) error) { c.pacer.writeBatch.Store(&fn) }

// LaneMark is how many data shards the pacer's data queue has taken so far;
// LaneDrained reports whether all of the first mark have left it. The pool
// takes a mark after each ordinary packet of a flow and sends the flow's
// next packet urgent only once it is drained (engine/dgfq.go).
func (c *Conn) LaneMark() uint64 { return c.pacer.inQueued.Load() }

// LaneDrained: see LaneMark.
func (c *Conn) LaneDrained(mark uint64) bool { return c.pacer.inLeft.Load() >= mark }

// NoteQueueDrop tells the rate model that the pool's send queue for this
// carrier dropped a packet (full, or aged past its sojourn): a backlog the
// pacer did not drain (see rateControl's stageHeadroom). Any goroutine.
func (c *Conn) NoteQueueDrop() { c.rc.noteStageDrop() }

// SendUrgent sends a data frame whose shards take the pacer's fast lane:
// after FEC parity, ahead of the data queue (see pacer.fast). The pool uses
// it for a packet of an interactive flow (engine/dgfq.go).
func (c *Conn) SendUrgent(payload []byte) error { return c.sendDataLane(payload, true) }

// sendData seals a data payload and feeds it to the FEC encoder; the pacer puts
// the resulting shards on the wire with a tagData prefix.
func (c *Conn) sendData(payload []byte) error { return c.sendDataLane(payload, false) }

// sendDataLane is sendData; urgent puts the frame's data shard in the fast lane.
func (c *Conn) sendDataLane(payload []byte, urgent bool) error {
	select {
	case <-c.done:
		return errClosed
	default:
	}
	if err := c.pacer.err(); err != nil {
		return err
	}
	c.sendMu.Lock()
	wire, err := c.sess.AppendDatagram(c.sendScr[:0], core.TypeData, 0, payload, c.dgPadTo(len(payload)))
	if err != nil {
		c.sendMu.Unlock()
		return err
	}
	c.sendScr = wire
	now := time.Now()
	err = c.enc.Encode(wire, now, func(pkt []byte) { c.pacer.enqueueLane(pkt, urgent) })
	c.sendMu.Unlock()
	return err
}

// sendControl seals a control frame and writes it straight to the socket with a
// tagCtrl prefix, bypassing FEC and the pacer so it is timely.
func (c *Conn) sendControl(ftype byte, payload []byte) error {
	select {
	case <-c.done:
		return errClosed
	default:
	}
	// A fresh buffer: the frame is written after the lock is released.
	pad := c.dgPadTo(len(payload))
	dg := make([]byte, 1, 1+core.DatagramLen(len(payload), pad))
	dg[0] = tagCtrl
	dg, err := c.sess.AppendDatagram(dg, ftype, 0, payload, pad)
	if err != nil {
		return err
	}
	return c.write(dg)
}

// ReadFrame returns the next decoded hs2 frame, or an error when the link is
// dead or closed. Feedback and key-confirmation frames are consumed internally
// and never surface here.
func (c *Conn) ReadFrame() (byte, []byte, error) {
	timer := time.NewTimer(deadAfter)
	defer timer.Stop()
	for {
		select {
		case <-c.done:
			return 0, nil, errClosed
		case f := <-c.frames:
			return f.ftype, f.payload, nil
		case <-timer.C:
			idle := time.Since(monoBase) - time.Duration(c.lastRxMono.Load())
			if idle >= deadAfter {
				return 0, nil, errDeadLink
			}
			timer.Reset(deadAfter - idle)
		}
	}
}

// TryReadFrame returns the next decoded frame if one is already waiting (ok
// false otherwise): the pool takes what arrived together in one go and writes
// it to the TUN as a batch.
func (c *Conn) TryReadFrame() (byte, []byte, bool) {
	select {
	case f := <-c.frames:
		return f.ftype, f.payload, true
	default:
		return 0, nil, false
	}
}

// LastRx is when the carrier last received anything from its peer. The peer
// sends feedback every feedbackEvery (100 ms) on a live carrier, so a few
// seconds of silence means the carrier is dead (its peer restarted, or the
// path is gone) long before deadAfter says so.
//
// It carries a monotonic reading (monoBase plus an offset), so now.Sub(LastRx())
// with now from time.Now() is immune to wall-clock steps: a stored unix time
// made an NTP step of a few seconds look like every carrier going silent at
// once.
func (c *Conn) LastRx() time.Time {
	return monoBase.Add(time.Duration(c.lastRxMono.Load()))
}

// monoBase anchors the receive stamps on the monotonic clock.
var monoBase = time.Now()

// Close tears the carrier down. On the dialer side it closes the socket; on the
// listener side it only deregisters from the shared socket's demux.
func (c *Conn) Close() error {
	c.closeOne.Do(func() {
		close(c.done)
		c.pacer.close()
		if g := c.gov.Load(); g != nil {
			g.detach(c)
		}
		if c.onClose != nil {
			c.onClose()
		}
		if c.rawConn != nil {
			c.rawConn.Close()
		}
	})
	return nil
}

// rxPkt is one received datagram and when it came off the socket.
type rxPkt struct {
	b  []byte
	at time.Time
}

// feed hands one received datagram to the decode loop. Used by the listener's
// demux; the dialer's own read goroutine calls it too. The datagram is
// stamped here, at the socket, so the delay measurements (RTT, one-way delay)
// do not include time spent queued behind FEC decoding.
func (c *Conn) feed(pkt []byte) {
	select {
	case c.rx <- rxPkt{b: pkt, at: time.Now()}:
	case <-c.done:
	}
}

// tryFeed is feed for the listener, where every carrier shares ONE socket and
// ONE read loop: it never blocks. A carrier whose reader has stalled (decode
// and frame queues full) loses the datagram — plain loss, which FEC and the
// inner transport already handle — instead of freezing every other carrier on
// the socket until the stalled one is torn down. (A dialer owns its socket, so
// feed may block there: the backpressure stays on that one carrier.)
func (c *Conn) tryFeed(pkt []byte) {
	select {
	case c.rx <- rxPkt{b: pkt, at: time.Now()}:
	case <-c.done:
	default:
		c.rxDropped.Add(1)
	}
}

// processLoop is the only user of the FEC decoder and the session's replay
// window, so decode stays single-threaded as the package requires.
func (c *Conn) processLoop() {
	defer c.wg.Done()
	lastExpire := time.Now()
	for {
		select {
		case <-c.done:
			return
		case rp := <-c.rx:
			pkt, now := rp.b, rp.at
			c.lastRxMono.Store(int64(now.Sub(monoBase)))
			if len(pkt) < 1 {
				continue
			}
			switch pkt[0] {
			case tagData, tagDataTS:
				hdr := 5
				if pkt[0] == tagDataTS {
					hdr = 9
				}
				if len(pkt) < hdr {
					continue
				}
				c.trackWire(binary.BigEndian.Uint32(pkt[1:5]))
				c.wireBytes.Add(uint64(len(pkt)))
				if hdr == 9 {
					c.noteOWD(stampOf(now) - binary.BigEndian.Uint32(pkt[5:9]))
				}
				c.dec.Decode(pkt[hdr:], now, func(payload []byte) { c.onPayload(payload, now) })
				if now.Sub(lastExpire) >= expireEvery {
					c.dec.Expire(now)
					lastExpire = now
				}
				s := c.dec.Stats()
				c.decStats.Store(&s)
			case tagCtrl:
				c.onPayload(pkt[1:], now)
			}
		}
	}
}

// onPayload routes one decrypted hs2 frame recovered from the wire.
func (c *Conn) onPayload(wire []byte, now time.Time) {
	seq, ct, err := unpackDatagram(wire)
	if err != nil {
		return
	}
	ftype, _, payload, err := c.sess.OpenDatagram(seq, ct)
	if err != nil {
		return // bad AEAD, replay, or stale: drop
	}
	switch ftype {
	case core.TypeFeedback:
		c.onFeedback(payload, now)
	case core.TypeAuth:
		select {
		case c.confCh <- append([]byte(nil), payload...):
		default:
		}
	case core.TypeData:
		c.rxDataBytes.Add(uint64(len(payload)))
		c.deliver(core.TypeData, payload)
	case core.TypePing, core.TypePong, core.TypeClose, core.TypePoolCtl, core.TypeLinkStats:
		c.deliver(ftype, payload)
	}
}

func (c *Conn) deliver(ftype byte, payload []byte) {
	msg := frameMsg{ftype: ftype, payload: append([]byte(nil), payload...)}
	select {
	case c.frames <- msg:
	case <-c.done:
	}
}

// onFeedback applies a report the peer sent about what it received from us: it
// paces our sending (bandwidth/RTT) and sizes our FEC parity (loss).
func (c *Conn) onFeedback(b []byte, now time.Time) {
	fb, ok := decodeFeedback(b)
	if !ok {
		return
	}
	// Drop reordered/stale feedback so the rate model only advances on fresh
	// reports (feedback can still arrive out of order despite riding raw).
	if fb.sendNanos <= c.lastFbNanos {
		return
	}
	c.lastFbNanos = fb.sendNanos
	// RTT: (now - echoNanos) minus the peer's processing hold.
	var rttSec float64
	if fb.echoNanos != 0 {
		rtt := now.UnixNano() - fb.echoNanos - fb.echoDelayNanos
		if rtt > 0 {
			rttSec = float64(rtt) / 1e9
		}
	}
	if fb.flags&fbStamps != 0 && !c.peerStamps.Load() {
		c.peerStamps.Store(true)
	}
	g := c.gov.Load()
	c.rc.setShare(g.Share(), g.Mean(), g.BusyQueue())
	c.rc.govCapped.Store(g.Capped())
	c.rc.onFeedback(now, fb.rxDataBytes, rttSec, fb.lossPPM, fb.echoNanos, fb.owdTicks, fb.flags&fbOWD != 0)
	loss := float64(fb.lossPPM) / 1e6
	if g != nil {
		g.report(c, loss, c.rc.queueSec())
	}

	// Loss sizes parity, never rate. But once the pool's governor has
	// confirmed a policer, its drops are ours to avoid by rate, not to repair:
	// parity sizes for the path's own loss (seen between episodes). More
	// parity would only put more bytes into the policer.
	if g.Confirmed() {
		loss = math.Min(loss, g.CleanLoss()+0.01)
	}
	est := c.adapter.Observe(loss, now)
	c.enc.SetLoss(est)
	c.lossEst.Store(math.Float64bits(est))

	// Remember this report so our next feedback can echo it for the peer's RTT.
	c.echoMu.Lock()
	c.peerSend = fb.sendNanos
	c.peerRecvAt = now
	c.echoMu.Unlock()
}

// feedbackLoop periodically tells the peer what we have received, so its rate
// model and parity sizing stay current.
func (c *Conn) feedbackLoop() {
	defer c.wg.Done()
	if !icmpCamo {
		t := time.NewTicker(feedbackEvery)
		defer t.Stop()
		for {
			select {
			case <-c.done:
				return
			case now := <-t.C:
				c.sendFeedback(now)
			}
		}
	}
	// Camouflage: jitter the interval (no fixed spectral line) and stay quiet
	// while nothing has been received to report (keepalive keeps the link
	// alive). When data flows the peer still gets ~feedbackEvery reports for
	// the rate control, only jittered; when it stops, the carrier goes silent
	// like an idle host instead of beating at 10 Hz.
	var lastRx uint64
	timer := time.NewTimer(camoJitter(feedbackEvery, 0.4))
	defer timer.Stop()
	for {
		select {
		case <-c.done:
			return
		case now := <-timer.C:
			rx := c.wireBytes.Load()
			active := rx != lastRx
			lastRx = rx
			if active {
				c.sendFeedback(now)
				timer.Reset(camoJitter(feedbackEvery, 0.4))
			} else {
				timer.Reset(camoJitter(camoIdlePoll, 0.5))
			}
		}
	}
}

func (c *Conn) sendFeedback(now time.Time) {
	c.echoMu.Lock()
	echo := c.peerSend
	var echoDelay int64
	if echo != 0 {
		echoDelay = now.Sub(c.peerRecvAt).Nanoseconds()
	}
	c.echoMu.Unlock()

	fb := feedback{
		sendNanos:      now.UnixNano(),
		echoNanos:      echo,
		echoDelayNanos: echoDelay,
		// Rate control paces to the WIRE delivery rate (all data datagrams that
		// arrive), not the post-FEC goodput, so a spell of unrecovered loss
		// cannot spiral the pacing rate down and starve the interleaving.
		rxDataBytes: c.wireBytes.Load(),
		lossPPM:     c.wireLossPPM(),
		flags:       fbStamps,
	}
	if m := c.owdMin.Swap(0); m != 0 {
		fb.owdTicks, fb.flags = uint32(m), fb.flags|fbOWD
	}
	_ = c.sendControl(core.TypeFeedback, fb.encode())
}

// wireLossPPM estimates the wire loss on the path INTO us since the last
// report, as the share of resolved datagrams the window passed without ever
// seeing them. Because trackWire resolves a reordered datagram as received (it
// arrives within the window), only a genuine gap counts, so reordering and
// jitter no longer read as loss. A loss is confirmed within ~wireReorderWin
// datagrams of the gap — still prompt enough for the encoder to size parity for
// a real burst, and far more accurate than the old high-water estimator.
func (c *Conn) wireLossPPM() uint32 {
	recv := c.wireRecv.Load()
	lost := c.wireLost.Load()
	dRecv := recv - c.lastWireRecv
	dLost := lost - c.lastWireLost
	c.lastWireRecv, c.lastWireLost = recv, lost
	denom := dRecv + dLost
	if denom == 0 {
		return 0
	}
	frac := float64(dLost) / float64(denom)
	if frac > 1 {
		frac = 1
	}
	return uint32(frac * 1e6)
}

// trackWire records one data datagram's wire sequence for loss measurement,
// reorder-tolerant. Called only from the decode loop (single goroutine per
// Conn), so wireBase/wireMax/wireSeen need no locking; only the two counters
// the feedback loop reads are atomic.
func (c *Conn) trackWire(seq uint32) {
	if !c.wirePrimed {
		c.wirePrimed = true
		c.wireBase, c.wireMax = seq, seq
		c.wireSetSeen(seq)
		c.wireRecv.Add(1)
		return
	}
	if int32(seq-c.wireMax) > 0 {
		c.wireMax = seq
	}
	// Slide the window forward: every seq it now passes is resolved — received
	// if its bit is set (it arrived earlier, reordered), otherwise lost.
	for int32(c.wireMax-c.wireBase) >= wireConfirmWin {
		if !c.wireTestSeen(c.wireBase) {
			c.wireLost.Add(1)
		}
		c.wireClearSeen(c.wireBase)
		c.wireBase++
	}
	switch {
	case int32(seq-c.wireBase) < 0:
		// Older than the window: already resolved (a very late reorder, or a
		// duplicate). Ignore, so no seq is ever counted twice.
	case !c.wireTestSeen(seq):
		c.wireSetSeen(seq)
		c.wireRecv.Add(1)
	default:
		// A duplicate within the window. Ignore.
	}
}

// wireSeen is a ring bitmap over wireReorderWin consecutive seqs; within any
// such window seq%wireReorderWin is unique, so there is no aliasing.
func (c *Conn) wireSetSeen(seq uint32) {
	i := seq % wireReorderWin
	c.wireSeen[i/64] |= 1 << (i % 64)
}

func (c *Conn) wireClearSeen(seq uint32) {
	i := seq % wireReorderWin
	c.wireSeen[i/64] &^= 1 << (i % 64)
}

func (c *Conn) wireTestSeen(seq uint32) bool {
	i := seq % wireReorderWin
	return c.wireSeen[i/64]&(1<<(i%64)) != 0
}

// noteOWD folds one stamped datagram's one-way delay (receive stamp minus
// send stamp, modulo 2^32) into this interval's minimum. Decode loop only
// writes; the feedback loop swaps the value out.
func (c *Conn) noteOWD(d uint32) {
	for {
		cur := c.owdMin.Load()
		if cur != 0 && int32(d-uint32(cur)) >= 0 {
			return
		}
		if c.owdMin.CompareAndSwap(cur, 1<<32|uint64(d)) {
			return
		}
	}
}

// flushLoop closes FEC groups that have been open for their window, emitting
// their parity, and does nothing else (the encoder is safe for concurrent use).
func (c *Conn) flushLoop() {
	defer c.wg.Done()
	t := time.NewTicker(flushEvery)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case now := <-t.C:
			c.enc.Flush(now, func(pkt []byte) { c.pacer.enqueue(pkt) })
		}
	}
}

// Stats is a snapshot used by tests, logs and the transport selector.
type Stats struct {
	Enc          fec.EncoderStats
	Dec          fec.DecoderStats
	RxDropped    uint64 // listener only: inbound datagrams dropped, this carrier's queue full
	BtlBwBytes   float64
	RateBytes    float64 // current pacing rate, bytes/s (what the pacer is allowed to send)
	RTProp       time.Duration
	LossPPM      uint32
	ParityRatio  float64 // parity shards per data shard now (0.5 = +50% bytes)
	FECAtCeiling bool    // parity is at its maximum: loss beyond what FEC is sized for
	Startup      bool    // the rate model is still ramping (no capacity estimate yet)
	Pushing      bool    // offered most of its allowance at the last feedback
	StageLimited bool    // its send stage (CPU, socket), not the path, held it back lately

	// The send stage (status): what the pacer sent over the last report
	// (bytes/s, next to RateBytes: the share of its allowance the carrier
	// could use), the standing queue its controller sees and its smoothed
	// RTT (seconds), the bytes waiting in the pacer now, and since start the
	// time enqueue waited for pacer room, the time inside socket writes (the
	// syscall, and the shared socket's lock where there is one), the write
	// calls, and the datagrams and bytes written.
	SendBytes, QueueSec, SRTT float64
	PacerQueued               int64
	PacerHeld, PacerWrite     time.Duration
	PacerWrites, PacerSent    uint64
	PacerSentBytes            uint64
}

// Pushing reports whether the carrier was offering at least most of its
// allowance at the last feedback — so a loss it saw then could be the path
// capping the rate, rather than the carrier simply being lightly loaded.
func (c *Conn) Pushing() bool { return c.rc.pushing.Load() }

// AttachGovernor puts the carrier under its pool's governor (engine/dgpool).
func (c *Conn) AttachGovernor(g *Governor) {
	if g == nil {
		return
	}
	c.gov.Store(g)
	g.attach(c)
}

// Encap returns the carrier's encapsulation kind ("" or "udp" for plain UDP,
// "icmp", "gre", "ipip", "ipx"). A pool reads it to decide whether a carrier
// needs ICMP echo-shaping (its wire packets are echo requests/replies that a
// stateful classifier expects to be ~1:1). Fixed at construction, so safe to
// read without locking.
func (c *Conn) Encap() string { return c.kind }

// dgPadTo returns the padTo for a datagram frame of n payload bytes: n rounded
// up to the next size bucket (B6), capped at the inner MTU so the sealed frame
// cannot push the packet past the path MTU and fragment, or 0 (no padding) when
// shaping is disabled. A full-size data frame is already above the largest
// bucket, so bulk traffic carries no padding overhead.
func (c *Conn) dgPadTo(n int) int {
	if dgPadDisabled {
		return 0
	}
	return core.PadDatagramTarget(n, c.innerMTU)
}

// Warm reports whether the carrier's rate model has a capacity estimate (it
// has left startup), so a pool can tell path backpressure from a carrier that
// is merely still ramping. Concurrency-safe.
func (c *Conn) Warm() bool {
	_, _, _, startup := c.rc.snapshot()
	return !startup
}

func (c *Conn) Stats() Stats {
	bw, rtt, loss, startup := c.rc.snapshot()
	q, srtt, send := c.rc.diag()
	pd := c.pacer.diag()
	var dec fec.DecoderStats
	if sp := c.decStats.Load(); sp != nil {
		dec = *sp
	}
	return Stats{
		Enc:          c.enc.Stats(),
		Dec:          dec,
		RxDropped:    c.rxDropped.Load(),
		BtlBwBytes:   bw,
		RateBytes:    c.rc.rateSnapshot(),
		RTProp:       time.Duration(rtt * float64(time.Second)),
		LossPPM:      loss,
		ParityRatio:  c.enc.ParityRatio(),
		FECAtCeiling: math.Float64frombits(c.lossEst.Load()) >= fec.DefaultAdapterConfig().Max-1e-9,
		Startup:      startup,
		Pushing:      c.rc.pushing.Load(),
		StageLimited: c.rc.stageLimited(),
		SendBytes:    send, QueueSec: q, SRTT: srtt,
		PacerQueued: pd.Queued, PacerHeld: pd.Held, PacerWrite: pd.Write,
		PacerWrites: pd.Writes, PacerSent: pd.Sent, PacerSentBytes: pd.SentBytes,
	}
}
