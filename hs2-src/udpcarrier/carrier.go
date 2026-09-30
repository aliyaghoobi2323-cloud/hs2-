package udpcarrier

import (
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/fec"
)

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
	sess    *core.Session
	shared  []byte
	binding []byte

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
	ctrlMu   sync.Mutex
	ctrlScr  []byte // reused control seq||ct scratch, guarded by ctrlMu
	closeOne sync.Once

	// liveness
	lastRxNanos atomic.Int64

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

	// wire-loss measurement: wireHigh/wireRecv are written by the decode loop
	// (trackWire) and read by the feedback loop (wireLossPPM).
	wireHigh                   atomic.Uint32
	wireRecv                   atomic.Uint32
	wireBytes                  atomic.Uint64 // bytes of data datagrams received
	wirePrimed                 bool          // decode loop only
	lastWireHigh, lastWireRecv uint32        // feedback loop only

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
		sess:    sess,
		shared:  shared,
		binding: binding,
		enc:     fec.NewEncoder(fcfg),
		adapter: fec.NewAdapter(fec.AdapterConfig{}),
		rc:      rc,
		write:   write,
		onClose: onClose,
		rawConn: rawConn,
		rx:      make(chan rxPkt, 1024),
		frames:  make(chan frameMsg, 2048),
		confCh:  make(chan []byte, 4),
		done:    make(chan struct{}),
	}
	// ttl a bit over the window so a group waits long enough for its parity but
	// a permanently lost group is given up on quickly (bounding recovery delay).
	c.dec = fec.NewDecoder(220*time.Millisecond, c.enc.Config().MaxPayload+2)
	// Shallow pacer queue: it holds ~one BDP so backpressure reaches the sender
	// fast and per-packet queueing latency stays small.
	c.pacer = newPacer(rc, write, 64, &c.peerStamps)
	c.lastRxNanos.Store(time.Now().UnixNano())
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

// SendFrame is the engine's send path. TypeData rides FEC and the pacer;
// everything else (keepalives, and internally feedback/auth) is a raw control
// datagram.
func (c *Conn) SendFrame(ftype byte, payload []byte) error {
	if ftype == core.TypeData {
		return c.sendData(payload)
	}
	return c.sendControl(ftype, payload)
}

// sendData seals a data payload and feeds it to the FEC encoder; the pacer puts
// the resulting shards on the wire with a tagData prefix.
func (c *Conn) sendData(payload []byte) error {
	select {
	case <-c.done:
		return errClosed
	default:
	}
	if err := c.pacer.err(); err != nil {
		return err
	}
	c.sendMu.Lock()
	seq, sealed, err := c.sess.SealDatagram(core.TypeData, 0, payload, 0)
	if err != nil {
		c.sendMu.Unlock()
		return err
	}
	c.sendScr = packDatagram(c.sendScr, seq, sealed)
	wire := c.sendScr
	now := time.Now()
	err = c.enc.Encode(wire, now, func(pkt []byte) { c.pacer.enqueue(pkt) })
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
	c.ctrlMu.Lock()
	seq, sealed, err := c.sess.SealDatagram(ftype, 0, payload, 0)
	if err != nil {
		c.ctrlMu.Unlock()
		return err
	}
	c.ctrlScr = packDatagram(c.ctrlScr[:0:cap(c.ctrlScr)], seq, sealed)
	dg := append([]byte{tagCtrl}, c.ctrlScr...)
	c.ctrlMu.Unlock()
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
			idle := time.Since(time.Unix(0, c.lastRxNanos.Load()))
			if idle >= deadAfter {
				return 0, nil, errDeadLink
			}
			timer.Reset(deadAfter - idle)
		}
	}
}

// Close tears the carrier down. On the dialer side it closes the socket; on the
// listener side it only deregisters from the shared socket's demux.
func (c *Conn) Close() error {
	c.closeOne.Do(func() {
		close(c.done)
		c.pacer.close()
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
			c.lastRxNanos.Store(now.UnixNano())
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
	c.rc.onFeedback(now, fb.rxDataBytes, rttSec, fb.lossPPM, fb.echoNanos, fb.owdTicks, fb.flags&fbOWD != 0)

	// Loss sizes parity, never rate.
	est := c.adapter.Observe(float64(fb.lossPPM)/1e6, now)
	c.enc.SetLoss(est)

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
// report, measured directly from the per-datagram wire sequence: the highest
// sequence advances by roughly the number of datagrams the sender put on the
// wire, and the received count by how many arrived, so their difference is the
// wire loss — immediate and accurate, which is what the encoder needs to size
// parity for a burst as it happens (decoder stats lag a group's expiry).
func (c *Conn) wireLossPPM() uint32 {
	high := c.wireHigh.Load()
	recv := c.wireRecv.Load()
	spanDelta := high - c.lastWireHigh
	recvDelta := recv - c.lastWireRecv
	c.lastWireHigh, c.lastWireRecv = high, recv
	if spanDelta == 0 {
		return 0
	}
	var lost uint32
	if spanDelta > recvDelta {
		lost = spanDelta - recvDelta
	}
	frac := float64(lost) / float64(spanDelta)
	if frac > 1 {
		frac = 1
	}
	return uint32(frac * 1e6)
}

// trackWire records one data datagram's wire sequence for loss measurement.
// Called only from the decode loop.
func (c *Conn) trackWire(seq uint32) {
	if !c.wirePrimed {
		c.wirePrimed = true
		c.wireHigh.Store(seq)
		c.wireRecv.Store(0)
		c.lastWireHigh, c.lastWireRecv = seq, 0
	}
	cur := c.wireHigh.Load()
	if int32(seq-cur) > 0 {
		c.wireHigh.Store(seq)
	}
	c.wireRecv.Add(1)
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
	PacerDropped uint64
	BtlBwBytes   float64
	RTProp       time.Duration
	LossPPM      uint32
	ParityRatio  float64
}

func (c *Conn) Stats() Stats {
	_, dropped, _ := c.pacer.stats()
	bw, rtt, loss, _ := c.rc.snapshot()
	var dec fec.DecoderStats
	if sp := c.decStats.Load(); sp != nil {
		dec = *sp
	}
	return Stats{
		Enc:          c.enc.Stats(),
		Dec:          dec,
		PacerDropped: dropped,
		BtlBwBytes:   bw,
		RTProp:       time.Duration(rtt * float64(time.Second)),
		LossPPM:      loss,
		ParityRatio:  c.enc.ParityRatio(),
	}
}
