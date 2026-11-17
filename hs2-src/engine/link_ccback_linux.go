package engine

import (
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// fx247 (audit246 L1): tcpCCReinit sets "reno" then the socket's own
// algorithm back; if the second setsockopt fails (a module or allowed-list
// change) the link was left on reno for good (readCC sees !bbr and never
// refreshes it again). Such a link is remembered here and put back on its
// algorithm every tick until it takes (a closed socket is forgotten).

const ccBackMaxLinks = 4096

var (
	ccBack     sync.Map // *net.TCPConn -> string (the algorithm to put back)
	ccBackN    atomic.Int64
	ccBackStat struct {
		left  atomic.Uint64 // reinits whose switch back failed
		back  atomic.Uint64 // links put back later
		logAt atomic.Int64  // last log (monoNano): at most one a minute
	}
)

func ccBackNote(tc *net.TCPConn, name string) {
	ccBackStat.left.Add(1)
	if ccBackN.Load() < ccBackMaxLinks {
		if _, loaded := ccBack.LoadOrStore(tc, name); !loaded {
			ccBackN.Add(1)
		}
	}
	now := monoNano(time.Now())
	if at := ccBackStat.logAt.Load(); now-at >= int64(time.Minute) && ccBackStat.logAt.CompareAndSwap(at, now) {
		log.Printf("link cc refresh: a link could not be switched back from reno to %s (setsockopt TCP_CONGESTION failed; %d such so far): retried every tick (fx247)", name, ccBackStat.left.Load())
	}
}

func ccBackTake(tc *net.TCPConn) (string, bool) {
	v, ok := ccBack.LoadAndDelete(tc)
	if !ok {
		return "", false
	}
	ccBackN.Add(-1)
	return v.(string), true
}

// ccBackRetryAll: every remembered link once more back on its algorithm.
func ccBackRetryAll() {
	if ccBackN.Load() == 0 {
		return
	}
	ccBack.Range(func(k, _ any) bool {
		tc := k.(*net.TCPConn)
		want, ok := ccBackTake(tc)
		if !ok {
			return true
		}
		rc, err := tc.SyscallConn()
		if err != nil {
			return true // gone: forgotten
		}
		set := false
		if rc.Control(func(fd uintptr) {
			set = unix.SetsockoptString(int(fd), unix.IPPROTO_TCP, unix.TCP_CONGESTION, want) == nil
		}) != nil {
			return true // closed: forgotten
		}
		if set {
			ccBackStat.back.Add(1)
		} else if _, loaded := ccBack.LoadOrStore(tc, want); !loaded {
			ccBackN.Add(1)
		}
		return true
	})
}
