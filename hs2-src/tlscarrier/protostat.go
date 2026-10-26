package tlscarrier

import (
	"fmt"
	"strconv"
	"sync"
	"time"
)

// protoStats is what `hs2 status` shows about the wire protocol (§4.1): the
// level and caps of the latest link, the other server's levels (known only
// where this side is the TLS server: a dialer learns just the level the other
// server chose), and the peers refused for their version or levels. One per
// process, like the tunnel it serves.
type protoStats struct {
	mu               sync.Mutex
	known            bool // a link has been negotiated
	level            byte
	caps             uint32
	peerMin, peerMax byte // 0: not known
	refusedV2        uint64
	refusedLevel     uint64
	lastRefused      time.Time
	logged           map[string]time.Time // a refusal line per peer: when it was last logged
}

var proto protoStats

// logEvery is how often one peer's refusal line may be logged.
const logEvery = time.Minute

func (p *protoStats) linked(level byte, caps uint32) {
	p.mu.Lock()
	p.known, p.level, p.caps = true, level, caps
	p.mu.Unlock()
}

func (p *protoStats) peerLevels(lo, hi byte) {
	p.mu.Lock()
	p.peerMin, p.peerMax = lo, hi
	p.mu.Unlock()
}

// refused counts a peer refused for running v2 (v2) or for its levels.
func (p *protoStats) refused(v2 bool) {
	p.mu.Lock()
	if v2 {
		p.refusedV2++
	} else {
		p.refusedLevel++
	}
	p.lastRefused = time.Now()
	p.mu.Unlock()
}

// mayLog reports whether key (a peer and its line) may be logged at now, and
// if so notes it: at most once every logEvery. Old keys are forgotten, so a
// flood of peers cannot grow the map without bound.
func (p *protoStats) mayLog(key string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if last, ok := p.logged[key]; ok && now.Sub(last) < logEvery {
		return false
	}
	if p.logged == nil {
		p.logged = map[string]time.Time{}
	}
	if len(p.logged) >= 1024 {
		for k, t := range p.logged {
			if now.Sub(t) >= logEvery {
				delete(p.logged, k)
			}
		}
		if len(p.logged) >= 1024 {
			return false // a minute of 1024 different peers: enough said
		}
	}
	p.logged[key] = now
	return true
}

// ProtocolLine is the `hs2 status` line about the wire protocol, in the
// design's words (§4.1); "-" stands for what is not known yet.
func ProtocolLine() string {
	p := &proto
	p.mu.Lock()
	defer p.mu.Unlock()
	level, peers, caps := "-", "-", "-"
	if p.known {
		level, caps = strconv.Itoa(int(p.level)), fmt.Sprintf("0x%X", p.caps)
	}
	if p.peerMin != 0 {
		peers = fmt.Sprintf("%d-%d", p.peerMin, p.peerMax)
	}
	s := fmt.Sprintf("protocol: v3 level %s (peer levels %s, caps %s); refused v2 peers: %d, refused peers (level): %d",
		level, peers, caps, p.refusedV2, p.refusedLevel)
	if p.refusedV2+p.refusedLevel > 0 {
		s += fmt.Sprintf(" (last %d s ago)", int(time.Since(p.lastRefused).Seconds()))
	}
	return s
}
