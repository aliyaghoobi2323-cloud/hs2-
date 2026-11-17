package engine

import (
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/envdef"
)

// jm271 (HS2_JM271_RST, default on; "0" = the rel69j behaviour): an exit
// that gets a plain join for a connection it does not know (it restarted,
// or the connection ended) answers with an RST record before closing the
// sub, as the rescue-rst join already does (mconn_link.go), instead of a
// bare close. A bare close left the Iran side's connection waiting for
// subs that can never carry its data (prod 07:25: kharej-only restart,
// joinmissed +3500 / 15 min, users hanging until their app timed out);
// the RST ends it at once (readLoop: case mRecRst, errMConnReset).
//
// Wire: mRecRst (0x05) is a base record; every mconn readLoop handles it
// since mconn part A (0b492969), so rel69i/rel69j/r54c Iran peers end the
// connection on it. A join sub is always record-framed.
//
// The RSTs are rate-limited (a restart turns every Iran conn's next join
// into a miss): over the bucket the sub is closed bare, as before.
var jm271On atomic.Bool

func init() { jm271On.Store(jm271Key()) }

func jm271Key() bool {
	if envdef.Legacy() {
		return os.Getenv("HS2_JM271_RST") == "1"
	}
	return envBoolDefault("HS2_JM271_RST", true)
}

const (
	jm271Rate  = 500  // RSTs per second
	jm271Burst = 2000 // bucket size
)

var jm271Stat struct {
	sent    atomic.Uint64 // missed joins answered with an RST
	limited atomic.Uint64 // missed joins closed bare: over the rate
}

// jm271Bucket: a token bucket (rate per second, burst tokens).
type jm271Bucket struct {
	mu     sync.Mutex
	tokens float64
	at     time.Time
	rate   float64
	burst  float64
}

func (b *jm271Bucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.at.IsZero() {
		b.tokens = b.burst
	} else if d := now.Sub(b.at).Seconds(); d > 0 {
		b.tokens += d * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
	}
	b.at = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

var jm271Limit = &jm271Bucket{rate: jm271Rate, burst: jm271Burst}

// jm271MissedJoin (exit, a plain join found no connection): writes the RST
// record on st when the key is on and the bucket allows; the caller closes
// st either way.
func jm271MissedJoin(st interface {
	SetWriteDeadline(time.Time) error
	Write([]byte) (int, error)
}) {
	if !jm271On.Load() {
		return
	}
	now := time.Now()
	if !jm271Limit.allow(now) {
		jm271Stat.limited.Add(1)
		return
	}
	jm271Stat.sent.Add(1)
	st.SetWriteDeadline(now.Add(kindTimeout))
	st.Write([]byte{mRecRst})
}
