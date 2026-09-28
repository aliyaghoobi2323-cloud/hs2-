// Package netsim is an in-process network simulator for the hs2 lab. The lab's
// full path emulator (lab/netem) bridges real interfaces at layer 2 and needs
// root, veth pairs and AF_PACKET; this one needs none of that. It is a UDP
// relay that sits between a client and a server on loopback and, per direction,
// drops and delays datagrams according to a loss model, so the UDP carrier can
// be tested against genuine bursty loss and jitter from an ordinary `go test`.
//
// It models loss the same way lab/netem and the fec sweep do — including a
// time-based Gilbert-Elliott channel whose bad state lasts for a duration, not
// a packet count, which is how a real path's bursts behave — so numbers from
// here line up with the fec package's own simulations. All numbers it produces
// are lab numbers, not measurements from a real server path.
package netsim

import (
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// LossModel decides, per datagram, whether the path drops it. Drop takes the
// current time so a model can produce time-based bursts.
type LossModel interface {
	Drop(now time.Time) bool
}

// None never drops.
type None struct{}

func (None) Drop(time.Time) bool { return false }

// AllDrop drops everything — a fully blocked path (the isolation test's
// "iptables -j DROP" without needing iptables).
type AllDrop struct{}

func (AllDrop) Drop(time.Time) bool { return true }

// IID drops each datagram independently with probability p.
type IID struct {
	mu  sync.Mutex
	rng *rand.Rand
	p   float64
}

func NewIID(seed uint64, p float64) *IID {
	return &IID{rng: rand.New(rand.NewPCG(seed, seed+7)), p: p}
}

func (l *IID) Drop(time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rng.Float64() < l.p
}

// TimeGE is a Gilbert-Elliott channel whose good/bad states last for
// exponentially distributed TIMES: bursts are "15 ms of 90% loss" whatever the
// packet rate, which is how real bursty loss behaves. Mean loss ≈
// badMs/(goodMs+badMs)·pb + goodMs/(goodMs+badMs)·pg.
type TimeGE struct {
	mu            sync.Mutex
	rng           *rand.Rand
	bad           bool
	until         int64 // ns
	pg, pb        float64
	goodMs, badMs float64
	started       bool
	origin        int64
}

func NewTimeGE(seed uint64, pg, pb, goodMs, badMs float64) *TimeGE {
	return &TimeGE{
		rng:    rand.New(rand.NewPCG(seed, seed^55)),
		pg:     pg,
		pb:     pb,
		goodMs: goodMs,
		badMs:  badMs,
	}
}

func (g *TimeGE) Drop(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	nowNs := now.UnixNano()
	if !g.started {
		g.started = true
		g.origin = nowNs
		g.until = nowNs
	}
	for nowNs >= g.until {
		g.bad = !g.bad
		m := g.goodMs
		if g.bad {
			m = g.badMs
		}
		g.until += int64(g.rng.ExpFloat64() * m * 1e6)
	}
	if g.bad {
		return g.rng.Float64() < g.pb
	}
	return g.rng.Float64() < g.pg
}

// Config shapes a Relay.
type Config struct {
	ToServer  LossModel     // loss on client->server
	ToClient  LossModel     // loss on server->client
	DelayBase time.Duration // one-way base delay each direction
	Jitter    time.Duration // uniform extra delay in [0, Jitter)
	RateBits  float64       // bottleneck bit/s each direction (0 = unlimited)
	Queue     time.Duration // max queueing delay before tail drop (default 200ms)
	Seed      uint64        // jitter RNG seed
}

// Relay is a UDP proxy between clients and one server, applying Config's loss
// and delay per direction. A client dials FrontAddr; the relay forwards to the
// server and relays replies back.
type Relay struct {
	front  *net.UDPConn
	server *net.UDPAddr

	mu       sync.Mutex
	cfg      Config
	jitRng   *rand.Rand
	backs    map[string]*backleg
	closed   bool
	done     chan struct{}
	wg       sync.WaitGroup
	dropped  [2]uint64    // [toServer, toClient] loss-model drops
	qdropped [2]uint64    // tail drops from the bottleneck queue
	passed   [2]uint64    // forwarded
	nextFree [2]time.Time // bottleneck serialisation clock per direction
}

type backleg struct {
	conn   *net.UDPConn
	client *net.UDPAddr
}

// NewRelay binds a front socket and prepares to forward to serverAddr.
func NewRelay(serverAddr string, cfg Config) (*Relay, error) {
	if cfg.ToServer == nil {
		cfg.ToServer = None{}
	}
	if cfg.ToClient == nil {
		cfg.ToClient = None{}
	}
	sa, err := net.ResolveUDPAddr("udp", serverAddr)
	if err != nil {
		return nil, err
	}
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		return nil, err
	}
	r := &Relay{
		front:  front,
		server: sa,
		cfg:    cfg,
		jitRng: rand.New(rand.NewPCG(cfg.Seed^0xabcdef, cfg.Seed+1)),
		backs:  make(map[string]*backleg),
		done:   make(chan struct{}),
	}
	r.wg.Add(1)
	go r.frontLoop()
	return r, nil
}

// FrontAddr is the address a client dials to reach the server through the relay.
func (r *Relay) FrontAddr() string { return r.front.LocalAddr().String() }

// SetLoss hot-swaps the loss models mid-test (e.g. to blackhole UDP under load
// and watch the transport fall back).
func (r *Relay) SetLoss(toServer, toClient LossModel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if toServer != nil {
		r.cfg.ToServer = toServer
	}
	if toClient != nil {
		r.cfg.ToClient = toClient
	}
}

// Stats returns forwarded and dropped datagram counts per direction.
func (r *Relay) Stats() (toServerPass, toServerDrop, toClientPass, toClientDrop uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.passed[0], r.dropped[0], r.passed[1], r.dropped[1]
}

// QueueDrops returns bottleneck tail-drop counts per direction.
func (r *Relay) QueueDrops() (toServer, toClient uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.qdropped[0], r.qdropped[1]
}

// LossFraction returns the realised loss-model drop fraction per direction
// (excludes bottleneck queue drops), for reporting the actual burst loss seen.
func (r *Relay) LossFraction() (toServer, toClient float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ts := r.passed[0] + r.dropped[0]
	tc := r.passed[1] + r.dropped[1]
	if ts > 0 {
		toServer = float64(r.dropped[0]) / float64(ts)
	}
	if tc > 0 {
		toClient = float64(r.dropped[1]) / float64(tc)
	}
	return
}

func (r *Relay) frontLoop() {
	defer r.wg.Done()
	buf := make([]byte, 2048)
	for {
		n, caddr, err := r.front.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-r.done:
				return
			default:
				continue
			}
		}
		pkt := append([]byte(nil), buf[:n]...)
		r.toServer(pkt, caddr)
	}
}

func (r *Relay) toServer(pkt []byte, caddr *net.UDPAddr) {
	now := time.Now()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	model := r.cfg.ToServer
	bl := r.backs[caddr.String()]
	if bl == nil {
		conn, err := net.DialUDP("udp", nil, r.server)
		if err != nil {
			r.mu.Unlock()
			return
		}
		bl = &backleg{conn: conn, client: caddr}
		r.backs[caddr.String()] = bl
		r.wg.Add(1)
		go r.backLoop(bl)
	}
	if model.Drop(now) {
		r.dropped[0]++
		r.mu.Unlock()
		return
	}
	delay, drop := r.scheduleLocked(0, len(pkt), now)
	if drop {
		r.qdropped[0]++
		r.mu.Unlock()
		return
	}
	r.passed[0]++
	r.mu.Unlock()
	r.sendAfter(delay, func() { bl.conn.Write(pkt) })
}

func (r *Relay) backLoop(bl *backleg) {
	defer r.wg.Done()
	buf := make([]byte, 2048)
	for {
		bl.conn.SetReadDeadline(time.Now().Add(time.Second))
		n, err := bl.conn.Read(buf)
		if err != nil {
			select {
			case <-r.done:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		pkt := append([]byte(nil), buf[:n]...)
		r.toClient(pkt, bl.client)
	}
}

func (r *Relay) toClient(pkt []byte, caddr *net.UDPAddr) {
	now := time.Now()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	model := r.cfg.ToClient
	if model.Drop(now) {
		r.dropped[1]++
		r.mu.Unlock()
		return
	}
	delay, drop := r.scheduleLocked(1, len(pkt), now)
	if drop {
		r.qdropped[1]++
		r.mu.Unlock()
		return
	}
	r.passed[1]++
	r.mu.Unlock()
	r.sendAfter(delay, func() { r.front.WriteToUDP(pkt, caddr) })
}

// scheduleLocked returns how long to hold a datagram of n bytes before
// delivering it: base delay + jitter, plus any wait behind the bottleneck's
// serialisation clock. If the bottleneck queue would exceed cfg.Queue the
// datagram is tail-dropped, exactly like lab/netem.
func (r *Relay) scheduleLocked(dir, n int, now time.Time) (time.Duration, bool) {
	prop := r.cfg.DelayBase
	if r.cfg.Jitter > 0 {
		prop += time.Duration(r.jitRng.Int64N(int64(r.cfg.Jitter)))
	}
	if r.cfg.RateBits <= 0 {
		return prop, false
	}
	q := r.cfg.Queue
	if q <= 0 {
		q = 200 * time.Millisecond
	}
	depart := now
	if r.nextFree[dir].After(now) {
		depart = r.nextFree[dir]
	}
	if depart.Sub(now) > q {
		return 0, true // queue full: tail drop
	}
	serialize := time.Duration(float64(n*8) / r.cfg.RateBits * float64(time.Second))
	r.nextFree[dir] = depart.Add(serialize)
	return depart.Sub(now) + prop, false
}

func (r *Relay) sendAfter(d time.Duration, fn func()) {
	if d <= 0 {
		fn()
		return
	}
	t := time.AfterFunc(d, fn)
	// leak-safe: on close the timer is left to fire into a closed socket, which
	// simply errors and is ignored; tests are short-lived.
	_ = t
}

// Close stops the relay.
func (r *Relay) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.done)
	backs := r.backs
	r.backs = map[string]*backleg{}
	r.mu.Unlock()
	r.front.Close()
	for _, bl := range backs {
		bl.conn.Close()
	}
	r.wg.Wait()
}
