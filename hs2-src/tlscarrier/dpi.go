package tlscarrier

import (
	"math"
	"os"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Low-cost DPI hardening, each behind its own key (all default off; with every
// key off the bytes on the wire are exactly as before):
//
//   - HS2_QUIET_IDLE=1: the link socket's TCP keepalive idles 45 s (Chrome's
//     value) instead of 3 s on the dialing side and Go's 15 s on the accepting
//     side. A black-holed link is still caught by TCP_USER_TIMEOUT (tune_*.go)
//     and the engine's own liveness (engine/quiet.go), which never relied on
//     the 3 s probes: smux keepalives already kept the socket from going idle.
//   - HS2_TLS_RESUME=1: TLS 1.3 session resumption like Chrome's. The dialer
//     keeps the tickets the server sends and offers one (pre_shared_key, last
//     extension) on the next link to the same server; the server shares one
//     tls.Config (and so one set of ticket keys) across its connections, so it
//     can resume them. See resume.go for why the channel-bound auth stays safe.
//   - HS2_UTLS_HELLO: which uTLS ClientHello to parrot (helloFromEnv).
//   - HS2_AUTH_PAD=1: the auth exchange is sized like an HTTP request and a
//     response WITH a body (auth.go), instead of a request and bare headers.
//   - HS2_COVER_NGINX=1: a plain-HTTP request on the TLS port is answered the
//     way nginx answers it, matching the nginx-style cover (cmd/hs2).
var (
	QuietIdle  = os.Getenv("HS2_QUIET_IDLE") == "1"
	Resume     = os.Getenv("HS2_TLS_RESUME") == "1"
	AuthPad    = os.Getenv("HS2_AUTH_PAD") == "1"
	CoverNginx = os.Getenv("HS2_COVER_NGINX") == "1"
)

// dialKeepAlive is the dialing side's TCP keepalive (see QuietIdle).
func dialKeepAlive() time.Duration {
	if QuietIdle {
		return quietKeepAlive
	}
	return 3 * time.Second
}

// quietKeepAlive is Chrome's TCP keepalive (idle and interval).
const quietKeepAlive = 45 * time.Second

// HelloFromEnv returns the ClientHello preset HS2_UTLS_HELLO names, or def when
// it is unset or unknown. "auto" (or "new") is uTLS's newest Chrome
// (HelloChrome_Auto: Chrome 133 with X25519MLKEM768 in the vendored v1.8.0);
// a number picks that Chrome preset, so a rollout can be rolled back by
// setting the old one ("120", "131", "133", ...).
func HelloFromEnv(def utls.ClientHelloID) utls.ClientHelloID {
	return helloByName(os.Getenv("HS2_UTLS_HELLO"), def)
}

func helloByName(v string, def utls.ClientHelloID) utls.ClientHelloID {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return def
	case "auto", "new", "newest":
		return utls.HelloChrome_Auto
	case "133":
		return utls.HelloChrome_133
	case "131":
		return utls.HelloChrome_131
	case "120":
		return utls.HelloChrome_120
	case "120_pq":
		return utls.HelloChrome_120_PQ
	case "115_pq":
		return utls.HelloChrome_115_PQ
	case "106":
		return utls.HelloChrome_106_Shuffle
	case "102":
		return utls.HelloChrome_102
	}
	return def
}

// clientHello is the link dialer's preset: today's HelloChrome_133, which is
// also the newest Chrome (= HelloChrome_Auto) in the vendored uTLS.
var clientHello = HelloFromEnv(utls.HelloChrome_133)

// logUniform draws from [lo, hi] with a log-uniform density: small values
// common, large ones rarer, like response body sizes.
func logUniform(lo, hi int) int {
	if lo < 1 {
		lo = 1
	}
	if hi <= lo {
		return lo
	}
	f := float64(randRange(0, 1<<20)) / float64(1<<20)
	v := int(math.Round(float64(lo) * math.Pow(float64(hi)/float64(lo), f)))
	return min(max(v, lo), hi)
}
