package smux

import (
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Lab only (branch r1-diag, docs/architecture/wip/r1/round2-diagnosis.fa.md;
// not a candidate): the engine sets LabTrace when HS2_LAB_TRACE is set, and
// nil leaves every hook below off. One line per small data frame (a probe, a
// TUN ping) at each step it takes through a session, so a probe's delay can
// be cut into its parts, and the bulk streams' window waits every
// labStatEvery.
//
//	Q sess sid len key t_push t_take lane      the writer took it
//	R sess sid len key t                       the reader parsed it
//	S sess t sid tx_delta peer_window inflight window_wait_ns_delta
//
// Times are wall-clock ns (both hs2 processes and the capture share a host).
var LabTrace func(line string)

// LabNoFrames (HS2_LAB_TRACE_NOFRAMES=1): the S lines only.
var LabNoFrames bool

const (
	labMax       = 200 // data frames of at most this many bytes are traced
	labStatEvery = 250 * time.Millisecond
)

func labSmall(cmd byte, n int) bool {
	return LabTrace != nil && !LabNoFrames && cmd == cmdPSH && n > 0 && n <= labMax
}

func labWall() int64 { return time.Now().UnixNano() }

// LabKey is a traced frame's key: its first 128 data bytes, hex (probes
// lead with their sequence numbers; a TUN frame carries the IP header).
func LabKey(b []byte) string {
	if len(b) > 128 {
		b = b[:128]
	}
	return hex.EncodeToString(b)
}

// LabSmallFrame says whether p, one frame as the session writes it to its
// conn ([ver][cmd][len u16 LE][sid u32 LE][data]), is traced.
func LabSmallFrame(p []byte) bool {
	if LabTrace == nil || LabNoFrames || len(p) < headerSize+1 || p[1] != cmdPSH {
		return false
	}
	n := int(p[2]) | int(p[3])<<8
	return n > 0 && n <= labMax && len(p) >= headerSize+n
}

var labWin sync.Map // *Stream -> *atomic.Int64: ns spent waiting for the peer window

func labWinAdd(s *Stream, d int64) {
	v, _ := labWin.LoadOrStore(s, new(atomic.Int64))
	v.(*atomic.Int64).Add(d)
}

func labWinGet(s *Stream) int64 {
	if v, ok := labWin.Load(s); ok {
		return v.(*atomic.Int64).Load()
	}
	return 0
}

// labStats writes an S line for each stream that sent at least 64 KiB, or
// waited for its window, since the last one.
func (s *Session) labStats() {
	t := time.NewTicker(labStatEvery)
	defer t.Stop()
	last := map[*Stream][2]int64{}
	for {
		select {
		case <-s.die:
			for st := range last {
				labWin.Delete(st)
			}
			return
		case <-t.C:
		}
		now := labWall()
		seen := map[*Stream][2]int64{}
		s.streamLock.Lock()
		for _, st := range s.streams {
			tx, ww := int64(st.txBytes.Load()), labWinGet(st)
			p := last[st]
			if tx-p[0] >= 64<<10 || ww > p[1] {
				inflight := int32(atomic.LoadUint32(&st.numWritten) - atomic.LoadUint32(&st.peerConsumed))
				LabTrace(fmt.Sprintf("S %p %d %d %d %d %d %d\n", s, now, st.id, tx-p[0], atomic.LoadUint32(&st.peerWindow), inflight, ww-p[1]))
			}
			seen[st] = [2]int64{tx, ww}
		}
		s.streamLock.Unlock()
		for st := range last {
			if _, ok := seen[st]; !ok {
				labWin.Delete(st)
			}
		}
		last = seen
	}
}
