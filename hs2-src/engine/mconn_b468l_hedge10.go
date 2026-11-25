package engine

import (
	"log"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// b468l HS2_LITE_HEDGE10=1 (both sides, lite; default off; turns on HEDGE8,
// HEDGE7, HEDGE4 and HEDGE3 too): F1l. A hedged conn is records from byte 0,
// so once it is heavy (one copy on the primary) every 16 KiB record pays the
// whole records hand-off that a raw conn does not: the sender waits on a
// two-record queue (mSubQueueLite), the sub writer takes m.mu twice, wakes
// the senders and does one smux Write per record (the batch joins at most
// mRecMax). Bursts (2-4 MB) and the heavy downloads of K4M2 ride this path in
// B and raw in A (burst p50 +5.3, Kharej CPU +4.7 % of a core in b467).
//
// With the key a heavy hedged conn's one-copy primary queue may hold
// hedge10Queue bytes and its writer joins queued records into one smux Write
// of up to hedge10Batch bytes (smux splits it into frames; the receiver reads
// records from a byte stream, so a record across two frames is fine). Calls
// (callKeep) and light conns keep the lite queue and one-frame batches. No
// wire change: the bytes on the stream are the same records.
var liteHedge10 = os.Getenv("HS2_LITE_HEDGE10") == "1"

var (
	hedge10Queue = int(hedge3EnvU64("HS2_B468L_QUEUE", 128<<10))
	hedge10Batch = int(hedge3EnvU64("HS2_B468L_BATCH", 64<<10))
)

var hedge10Stat struct {
	big    atomic.Uint64 // one-copy chunks queued under the big rule
	writes atomic.Uint64 // writer Writes of big-rule subs
	recs   atomic.Uint64 // ... records in them
}

func init() {
	if liteHedge10 {
		log.Printf("b468l hedge10: heavy hedged queue %d batch %d", hedge10Queue, hedge10Batch)
	}
}

// hedge10FitsLocked (sendLocked one-copy path, under m.mu): p has room for
// n more bytes; a heavy hedged conn (not a call) gets the big queue.
func (m *mConn) hedge10FitsLocked(p *msub, n int) bool {
	// dupOff (over mDupMaxRate) short-cuts the heavy check: a burst is
	// mostly dupOff, its h3Heavy not yet set (s02-B: big rule on 1.1k chunks)
	m.h10Big = liteHedge10 && m.lite && m.h3 && (m.h3Heavy || m.dupOff) && !m.callKeepLocked()
	if !m.h10Big {
		return p.fits(n)
	}
	if p.dc.full && p.dc.fullFresh() {
		return false
	}
	if p.qBytes == 0 || p.qBytes+n <= hedge10Queue {
		hedge10Stat.big.Add(1)
		return true
	}
	return false
}

// hedge10BatchMax (writer batch, under m.mu): the batch bound for s.
func (s *msub) hedge10BatchMax() int {
	if s.m.h10Big && s == s.m.primary.Load() {
		return hedge10Batch
	}
	return mRecMax
}

// HS2_B468L_DBG=1 (debug only, no behaviour change; Kharej): per exit conn
// that sends 2 MiB, the time from the conn's start to its first Write (the
// request's arrival + the server's answer), to 2 MiB sent, and the time its
// Writes blocked, split by class: raw, records hedged (first sub created),
// records hedge-created (exit_first), records not hedged. Logged every 10 s.
var b468lDbg = os.Getenv("HS2_B468L_DBG") == "1"

const b468lMark = 2 << 20

type b468lCls struct {
	n, first, t2m, wr atomic.Int64 // count; sums in µs
}

var b468lStat [4]b468lCls

var b468lClsName = [...]string{"raw", "hedged", "made", "rec"}

// write is the records/raw Write with the debug clock around it.
func (m *mConn) Write(p []byte) (int, error) {
	if !b468lDbg || m.b468Born == 0 || m.b468Done.Load() {
		return m.write0(p)
	}
	t0 := monoNano(time.Now())
	m.b468First.CompareAndSwap(0, t0-m.b468Born)
	n, err := m.write0(p)
	t1 := monoNano(time.Now())
	m.b468Wr.Add(t1 - t0)
	if m.b468Sent.Add(int64(n)) >= b468lMark && m.b468Done.CompareAndSwap(false, true) {
		c := 3
		if m.rawW.Load() {
			c = 0
		} else {
			m.mu.Lock()
			if m.h3 {
				c = 1
				if m.h4Made {
					c = 2
				}
			}
			m.mu.Unlock()
		}
		s := &b468lStat[c]
		s.n.Add(1)
		s.first.Add(m.b468First.Load() / 1e3)
		s.t2m.Add((t1 - m.b468Born) / 1e3)
		s.wr.Add(m.b468Wr.Load() / 1e3)
		b468lLog(t1)
	}
	return n, err
}

var b468lLogAt atomic.Int64

func b468lLog(n int64) {
	at := b468lLogAt.Load()
	if n-at < int64(10*time.Second) || !b468lLogAt.CompareAndSwap(at, n) {
		return
	}
	line := "b468l dbg:"
	for i := range b468lStat {
		s := &b468lStat[i]
		k := s.n.Load()
		if k == 0 {
			k = 1
		}
		line += " " + b468lClsName[i] + " n=" + b468lItoa(s.n.Load()) + " first=" + ms1(s.first.Load()/k) + " t2m=" + ms1(s.t2m.Load()/k) + " wr=" + ms1(s.wr.Load()/k)
	}
	line += " h10 big=" + b468lItoa(int64(hedge10Stat.big.Load())) + " writes=" + b468lItoa(int64(hedge10Stat.writes.Load())) + " recs=" + b468lItoa(int64(hedge10Stat.recs.Load()))
	log.Print(line)
}

func b468lItoa(v int64) string { return strconv.FormatInt(v, 10) }

// ms1: µs as "12.3" ms.
func ms1(us int64) string {
	return b468lItoa(us/1000) + "." + b468lItoa((us%1000)/100)
}
