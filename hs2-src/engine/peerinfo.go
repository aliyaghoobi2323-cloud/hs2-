package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"sync"
	"time"
)

// Peer info (kindInfo): on every link the two servers tell each other their
// link-pool ceiling (the max_links each one actually runs with), so BOTH can
// show the effective ceiling exactly — in direct mode the Iran server's (the
// exit accepts whatever the edge dials), in reverse the lower of the two (the
// exit clamps the edge's target to its own max) — and name the side that
// limits it. Version 2 adds per-port routing (routes.go): whether a side can
// tag user connections with their user port, and which ports it has, so each
// server can show where every Iran user port goes.
//
// Wire (after the kind byte), one exchange per link, then the stream closes:
//
//	edge -> exit  [ver][n u8][n bytes]
//	exit -> edge  [ver][n u8][n bytes]
//
//	v1 payload: maxLinks u16 (0 = none).
//	v2 payload: maxLinks u16, caps u8, flags u8, count u8, count × port u16
//	  caps  bit0: port tags — the edge tags its user connections / the exit
//	        routes tagged ones (kindTCPPort / kindUDPPort)
//	        bit1: quiet L3 — its TUN side-channel reader allows 30 s without
//	        a frame, so the other side may send idle keepalives every ~10 s
//	  flags bit0: edge: it forwards UDP too · exit: it has a default panel
//	        (expose)
//	        bit1: the port list was cut to fit
//	  ports: edge: the user ports it opens · exit: the ports with their own
//	        target (port_map)
//
// A reader takes the fields it knows from the first n bytes and skips the rest,
// so the payload can grow compatibly (a v1 reader sees only maxLinks).
//
// The edge opens the stream (it is the smux client in both directions). An
// older exit closes the unknown kind at once: the edge reads EOF and leaves the
// exit's info unknown. An older edge never opens it, so a newer exit leaves the
// edge's info unknown. Either way nothing else changes.
//
// The edge uses a link for user connections only once this exchange is over
// (answered, refused or given up — LinkManager.gateInfo): it must know whether
// the exit understands port tags BEFORE the first user connection rides it.
const (
	infoVer     = 2
	infoTimeout = 5 * time.Second

	capPortTags  = 1 << 0
	capL3Quiet   = 1 << 1
	flagUDP      = 1 << 0 // edge
	flagDefault  = 1 << 0 // exit
	flagCut      = 1 << 1
	infoFixedLen = 5                        // maxLinks, caps, flags, count
	infoMaxPorts = (255 - infoFixedLen) / 2 // what fits the u8 length
)

// peerInfo is one side's kindInfo message.
type peerInfo struct {
	MaxLinks int
	Caps     uint8
	Flags    uint8
	Ports    []int
	V2       bool // the message carried the v2 fields (a newer hs2)
}

func (pi *peerInfo) tags() bool { return pi != nil && pi.V2 && pi.Caps&capPortTags != 0 }

// l3Quiet reports whether the other server allows quiet L3 keepalives.
func (pi *peerInfo) l3Quiet() bool { return pi != nil && pi.V2 && pi.Caps&capL3Quiet != 0 }

// peerL3Quiet reads it from a link's meter (false until the info arrives).
func peerL3Quiet(m *linkMeter) func() bool {
	return func() bool { return m != nil && m.peerInfo.Load().l3Quiet() }
}

// infoRetry / infoTries: a reply that did not come in time (a congested link)
// is retried a few times while the link is up. Variables so tests can shorten.
var (
	infoRetry = 10 * time.Second
	infoTries = 3
	// infoSlowRetry: the edge asks again this often after a link went through
	// all infoTries without an answer of its own.
	infoSlowRetry = time.Minute
)

var errInfoVersion = errors.New("peer info: bad version")

// linkPeers is the set of an exit's live links, by meter. Each link stores the
// ceiling its edge reported (meter.peerMax), and the exit shows the max over
// the links that are up NOW — so a value from a link that has gone is never
// shown (e.g. after the Iran server was rolled back to a release that does not
// report, every new link carries 0 and the old number disappears with its
// links). The edge side reads its own LinkManager's live links the same way.
type linkPeers struct {
	mu sync.Mutex
	m  map[*linkMeter]struct{}
}

func (p *linkPeers) add(m *linkMeter) {
	if p == nil || m == nil {
		return
	}
	p.mu.Lock()
	if p.m == nil {
		p.m = map[*linkMeter]struct{}{}
	}
	p.m[m] = struct{}{}
	p.mu.Unlock()
}

func (p *linkPeers) remove(m *linkMeter) {
	if p == nil || m == nil {
		return
	}
	p.mu.Lock()
	delete(p.m, m)
	p.mu.Unlock()
}

// max is the edge's ceiling as the live links report it (0 = none reported).
func (p *linkPeers) max() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	best := 0
	for m := range p.m {
		if v := int(m.peerMax.Load()); v > best {
			best = v
		}
	}
	return best
}

// PeerRoutes is what the other server reported about per-port routing
// (routes.go), for display only.
type PeerRoutes struct {
	Known bool // reported by a live link (the other server runs a newer hs2)
	Older bool // edge: a link finished its exchange without it — an older exit
	// Filtered (dgtun edge): the exit's tun answers on DgTunPort but not on
	// DgTagPort, so per-port routing is off although the exit may support it.
	Filtered bool
	Tags     bool  // edge: the exit routes connections by their user port
	Ports    []int // edge: the exit's ports with their own target; exit: the edge's user ports
	Default  bool  // edge: the exit has a default panel (expose)
	UDP      bool  // exit: the edge forwards UDP on its user ports too
	Cut      bool  // the list was cut to fit the message
}

// edgeRoutes is what the edge's live links report about its user ports (exit
// side): the union of the ports, and whether it forwards UDP. Known is false
// when no live link carries a v2 report (no link up, or an older edge).
func (p *linkPeers) edgeRoutes() *PeerRoutes {
	r := &PeerRoutes{}
	if p == nil {
		return r
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[int]bool{}
	for m := range p.m {
		pi := m.peerInfo.Load()
		if pi == nil || !pi.V2 {
			continue
		}
		r.Known = true
		r.UDP = r.UDP || pi.Flags&flagUDP != 0
		r.Cut = r.Cut || pi.Flags&flagCut != 0
		for _, pt := range pi.Ports {
			if !seen[pt] {
				seen[pt] = true
				r.Ports = append(r.Ports, pt)
			}
		}
	}
	sort.Ints(r.Ports)
	return r
}

// exitRoutes turns the exit's message (as the edge's links received it) into
// what the edge shows.
func exitRoutes(pi *peerInfo) *PeerRoutes {
	if pi == nil || !pi.V2 {
		return &PeerRoutes{}
	}
	return &PeerRoutes{Known: true, Tags: pi.tags(), Ports: append([]int(nil), pi.Ports...),
		Default: pi.Flags&flagDefault != 0, Cut: pi.Flags&flagCut != 0}
}

// encodeInfo is one side's message: [ver][n][maxLinks u16][caps][flags][count][ports].
func encodeInfo(pi peerInfo) []byte {
	clamp := func(v int) uint16 {
		if v < 0 {
			return 0
		}
		if v > 0xffff {
			return 0xffff
		}
		return uint16(v)
	}
	ports := pi.Ports
	flags := pi.Flags
	if len(ports) > infoMaxPorts {
		ports, flags = ports[:infoMaxPorts], flags|flagCut
	}
	b := make([]byte, 2+infoFixedLen+2*len(ports))
	b[0], b[1] = infoVer, byte(infoFixedLen+2*len(ports))
	binary.BigEndian.PutUint16(b[2:], clamp(pi.MaxLinks))
	b[4], b[5], b[6] = pi.Caps, flags, byte(len(ports))
	for i, p := range ports {
		binary.BigEndian.PutUint16(b[7+2*i:], clamp(p))
	}
	return b
}

// readInfo reads one [ver][n][n bytes] message. Fields the payload is too short
// to hold are zero (a v1 message carries only the ceiling); bytes past the
// fields this version knows are skipped. Any version >= 1 is read the same way:
// a later version only appends fields.
func readInfo(r io.Reader) (peerInfo, error) {
	var pi peerInfo
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return pi, err
	}
	if h[0] < 1 {
		return pi, errInfoVersion
	}
	p := make([]byte, int(h[1]))
	if _, err := io.ReadFull(r, p); err != nil {
		return pi, err
	}
	if len(p) >= 2 {
		pi.MaxLinks = int(binary.BigEndian.Uint16(p))
	}
	if len(p) >= infoFixedLen {
		pi.V2 = true
		pi.Caps, pi.Flags = p[2], p[3]
		n := int(p[4])
		if have := (len(p) - infoFixedLen) / 2; n > have {
			n, pi.Flags = have, pi.Flags|flagCut
		}
		for i := 0; i < n; i++ {
			if v := int(binary.BigEndian.Uint16(p[infoFixedLen+2*i:])); v > 0 {
				pi.Ports = append(pi.Ports, v)
			}
		}
	}
	return pi, nil
}

// openInfo runs the EDGE side for one link: send this side's info, read the
// exit's, and store it on the link's meter (LinkManager.Stats reports it, and
// openStream reads from it whether to tag a user connection). The link may
// carry user connections (infoDone) once the FIRST attempt is over, however it
// ended: answered, refused by an older exit (infoRefused: definitive), or no
// answer in time — then the link uses what the pool's other links learned from
// the same exit (LinkManager.exitInfo) while this one keeps asking, so a slow
// answer never holds a link back for long nor leaves it untagged for its life.
func openInfo(ctx context.Context, l Link, mine peerInfo) {
	mtr := linkMeterOf(l)
	if mtr == nil {
		return
	}
	defer mtr.infoDone.Store(true)
	ro, ok := l.(rawStreamOpener)
	if !ok {
		return
	}
	for try := 0; try < infoTries && ctx.Err() == nil && l.Alive(); try++ {
		if try > 0 && !sleepCtx(ctx, infoRetry) {
			return
		}
		peer, err := exchangeInfo(ro, mine)
		if err == nil {
			mtr.peerMax.Store(uint32(peer.MaxLinks))
			mtr.peerInfo.Store(&peer)
			return
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errInfoVersion) {
			mtr.infoRefused.Store(true)
			return // an older exit closed the unknown kind (or the link died)
		}
		mtr.infoDone.Store(true) // no answer in time: carry users meanwhile
	}
}

// exchangeInfo opens one kindInfo stream, sends ours and reads theirs.
func exchangeInfo(ro rawStreamOpener, mine peerInfo) (peerInfo, error) {
	st, err := ro.OpenRawStream()
	if err != nil {
		return peerInfo{}, err
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(infoTimeout))
	if _, err := st.Write(append([]byte{kindInfo}, encodeInfo(mine)...)); err != nil {
		return peerInfo{}, err
	}
	return readInfo(st)
}

// serveInfo runs the EXIT side: read the edge's info, store it, answer with
// this side's.
func serveInfo(st io.ReadWriteCloser, mine peerInfo, store func(peerInfo)) {
	defer st.Close()
	if d, ok := st.(interface{ SetDeadline(time.Time) error }); ok {
		d.SetDeadline(time.Now().Add(infoTimeout))
	}
	peer, err := readInfo(st)
	if err != nil {
		return
	}
	if store != nil {
		store(peer)
	}
	st.Write(encodeInfo(mine))
}
