package engine

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// rs145: production rescue (rel61, tunnel 1, ~11k users, 13 paths, two real
// outages 2026-11-13) acked only 58 % of raw rescues (lostjoin 654 of 1644)
// and NAKed 252 of 1819 records rescues (the exit's records rings pinned at
// their 48 MB budget). Three keys, both sides harmless; since rs146 JOINPICK
// (edge) and SCALE (both) are envdef defaults, since rel64 RETRY (edge) too:
//
//   - HS2_MCONN_RESCUE_JOINPICK=1 (Iran): the rescue join goes only to a link
//     with fresh progress (read within rescueFreshWin, or nothing pending)
//     and, when one exists, on another Iran IP than every path the rescue
//     leaves (a whole-IP outage kills every path of that IP; the silent
//     test marks them one by one). Falls back to the old pick.
//   - HS2_MCONN_RESCUE_RETRY=1 (Iran): a rescue join that dies before its
//     answer is replaced once by a join on another path that sends the same
//     RESCUE / RREC again (raw: plus the records written since Bi, from the
//     records ring). Before, the connection ended (no sub left).
//   - HS2_MCONN_RESCUE_SCALE=1 (both): the records and raw ring budgets grow
//     with the number of connections keeping a ring (one full ring each),
//     bounded by HS2_MCONN_RESCUE_RAMPCT (4) % of MemTotal; never below the
//     fixed budgets.

var (
	rescueJoinPick  atomic.Bool
	rescueRetryOn   atomic.Bool
	rescueScaleOn   atomic.Bool
	rescueFreshWin  = time.Duration(envIntDef("HS2_MCONN_RESCUE_FRESH_MS", 1000)) * time.Millisecond
	rescueRAMPct    = int64(envIntDef("HS2_MCONN_RESCUE_RAMPCT", 4))
	rescueMemTotalO sync.Once
	rescueMemTotalV int64
)

func loadRescue145() {
	rescueJoinPick.Store(envOn("HS2_MCONN_RESCUE_JOINPICK"))
	rescueRetryOn.Store(envOn("HS2_MCONN_RESCUE_RETRY"))
	rescueScaleOn.Store(envOn("HS2_MCONN_RESCUE_SCALE"))
}

func init() { loadRescue145() }

var rs145Stat struct {
	pickFresh   atomic.Uint64 // join on a fresh link of another Iran IP
	pickFreshIP atomic.Uint64 // join on a fresh link, same Iran IP (no other)
	pickOld     atomic.Uint64 // no fresh link: the old pick
	retry       atomic.Uint64 // a lost join replaced
	retryFail   atomic.Uint64 // the replacement could not open / not covered
	retrySilent atomic.Uint64 // rs146: of retry, joins replaced because their link went silent
}

func rescue145LogLine() string {
	s := &rs145Stat
	return "mconn rescue145: pick fresh " + u64s(s.pickFresh.Load()) + " freshsameip " + u64s(s.pickFreshIP.Load()) +
		" old " + u64s(s.pickOld.Load()) + " · retry " + u64s(s.retry.Load()) + " retryfail " + u64s(s.retryFail.Load()) + " retrysilent " + u64s(s.retrySilent.Load()) +
		" · recmem cap " + u64s(uint64(rrMemCap()>>20)) + " MB rawmem cap " + u64s(uint64(rescueMemCap()>>20)) + " MB"
}

func u64s(v uint64) string { return strconv.FormatUint(v, 10) }

// --- ring budgets ---

func rescueMemTotal() int64 {
	rescueMemTotalO.Do(func() {
		f, err := os.Open("/proc/meminfo")
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fs := strings.Fields(sc.Text())
			if len(fs) >= 2 && fs[0] == "MemTotal:" {
				if kb, err := strconv.ParseInt(fs[1], 10, 64); err == nil {
					rescueMemTotalV = kb << 10
				}
				return
			}
		}
	})
	return rescueMemTotalV
}

// rescueScaledCap: base, or under the key n full rings of ring bytes,
// bounded by rescueRAMPct of MemTotal (never below base).
func rescueScaledCap(base int64, n int64, ring int) int64 {
	if !rescueScaleOn.Load() {
		return base
	}
	want := n * int64(ring)
	if lim := rescueMemTotal() * rescueRAMPct / 100; want > lim {
		want = lim
	}
	return max(base, want)
}

func rrMemCap() int64 { return rescueScaledCap(rrMemMax, rrStat.conns.Load(), rrRingCap) }

func rescueMemCap() int64 {
	return rescueScaledCap(rescueMemMax, rescueStat.rings.Load(), rescueRingCap())
}

// --- join placement ---

// rescueJoinMark in an avoid list (Iran rescue joins under JOINPICK): the
// paths the rescue leaves.
type rescueJoinMark struct{ paths map[string]bool }

func takeRescueJoinMark(avoid []any) ([]any, *rescueJoinMark) {
	for i, a := range avoid {
		if c, ok := a.(rescueJoinMark); ok {
			return append(avoid[:i:i], avoid[i+1:]...), &c
		}
	}
	return avoid, nil
}

// pathHost: the local (this side's) IP of a path key "lh>rh".
func pathHost(p string) string {
	if i := strings.IndexByte(p, '>'); i > 0 {
		return p[:i]
	}
	return p
}

// linkFresh: the link heard within rescueFreshWin, or has nothing pending
// (a quiet link that owes no answer).
func linkFresh(mtr *linkMeter, now int64) bool {
	if mtr == nil {
		return false
	}
	rd, wr := mtr.lastRd.Load(), mtr.lastWr.Load()
	if rd == 0 {
		return false
	}
	return time.Duration(now-rd) < rescueFreshWin || wr <= rd
}

// pickRescueJoinLocked (under m.mu): a fresh link off the left paths,
// preferring another Iran IP; nil when there is none.
func (m *LinkManager) pickRescueJoinLocked(avoid []any, pool *peerInfo, mk *rescueJoinMark) *managedLink {
	if b313RJMemo.Load() { // b313 (mconn_b313_rjmemo.go)
		return m.pickRescueJoinMemoLocked(avoid, pool, mk)
	}
	now := ctrlNow()
	hosts := map[string]bool{}
	for p := range mk.paths {
		hosts[pathHost(p)] = true
	}
	base := func(ml *managedLink) bool {
		p := mlPath(ml)
		return !mk.paths[p] && linkFresh(ml.mtr, now)
	}
	if ml := m.pickCallRescueLocked(avoid, pool, func(ml *managedLink) bool {
		return base(ml) && !hosts[pathHost(mlPath(ml))]
	}); ml != nil {
		rs145Stat.pickFresh.Add(1)
		return ml
	}
	if ml := m.pickCallRescueLocked(avoid, pool, base); ml != nil {
		rs145Stat.pickFreshIP.Add(1)
		return ml
	}
	rs145Stat.pickOld.Add(1)
	return nil
}

// rescueJoinAvoid (Iran, under mu): adds the mark for the paths of keys.
func rescueJoinAvoid(avoid []any, keys []any, extra map[any]bool) []any {
	if !rescueJoinPick.Load() {
		return avoid
	}
	paths := map[string]bool{}
	add := func(k any) {
		p := callPathOf(k)
		if l, ok := k.(Link); ok && p == "" {
			if mtr := linkMeterOf(l); mtr != nil {
				p = mtr.path
			}
		}
		if p != "" {
			paths[p] = true
		}
	}
	for _, k := range keys {
		add(k)
	}
	for k := range extra {
		add(k)
	}
	return append(avoid, rescueJoinMark{paths: paths})
}

// --- retry ---

// rescueRetryLocked (Iran, under mu, subGone): the raw rescue join s died
// before the ACK. true: a replacement is under way (s is abandoned: its
// queued records are resent from the records ring on the new join).
func (m *mConn) rescueRetryLocked(s *msub) bool {
	if !rescueRetryOn.Load() || m.rscRetried || m.ended || m.closing {
		return false
	}
	if rs169Fix.Load() && !m.rrCoversLocked(m.rscBi, m.sendOff) {
		// rs169 (M3): the records written since Bi rode s and no records
		// ring holds them: a retry could only reset. Keep the pre-RETRY
		// behaviour (a closed join ends the conn, a silent one is waited for).
		rs169Stat.noRing.Add(1)
		rtrace("%x iran rescue retry skipped: no ring covers [%d,%d)", m.id, m.rscBi, m.sendOff)
		return false
	}
	m.rscRetried = true
	s.rrAband = true
	m.lightOpening = true
	go m.rescueRetry(s)
	return true
}

func (m *mConn) rescueRetry(s *msub) {
	dead := s.key
	m.mu.Lock()
	avoid := append([]any{avoidKey(dead), avoidKey(m.rscOld.key)}, m.deadAvoidLocked()...)
	var silent map[any]bool
	if sp := rescueSilentSet.Load(); sp != nil {
		silent = *sp
		for l := range silent {
			avoid = append(avoid, avoidKey(l))
		}
	}
	avoid = rescueJoinAvoid(avoid, []any{dead, m.rscOld.key}, silent)
	if p := callPathOf(dead); p != "" {
		avoid = append(avoid, callStuckMark{path: p})
	} else if l, ok := dead.(Link); ok {
		if mtr := linkMeterOf(l); mtr != nil && mtr.path != "" {
			avoid = append(avoid, callStuckMark{path: mtr.path})
		}
	}
	if m.rscFreshI && rs169Fix.Load() {
		// rs169: the exit may never have seen the first rescue join (it
		// opened the conn there): this one opens it then (a plain join if
		// the conn is there)
		avoid = append(avoid, rescueFreshMark{})
	}
	m.mu.Unlock()
	sub, key, err := m.open(planeLight, avoid)
	m.mu.Lock()
	if err != nil || m.ended || m.closing || m.rscPhase != 1 {
		m.lightOpening = false
		// rs169: a join still alive (silent, or it answered meanwhile)
		// carries on as before the retry; only a dead one ends the conn
		keep := rs169Fix.Load() && !s.dead && !m.ended
		if keep {
			s.rrAband = false
		}
		m.mu.Unlock()
		if err == nil {
			sub.Close()
		}
		rs145Stat.retryFail.Add(1)
		rtrace("%x iran rescue retry failed err=%v keep=%v", m.id, err, keep)
		if err != nil && !keep {
			m.end(errMConnSubsDied, false)
		}
		return
	}
	// the records written since Bi rode the dead join: resend them
	m.rkMu.Lock()
	var data []byte
	ok := true
	if m.sendOff > m.rscBi {
		data, ok = m.rrRangeLocked(m.rscBi, m.sendOff)
	}
	m.rkMu.Unlock()
	if !ok {
		m.lightOpening = false
		keep := rs169Fix.Load() && !s.dead // rs169 (M3): as before the retry
		if keep {
			s.rrAband = false
		}
		m.mu.Unlock()
		sub.Close()
		rs145Stat.retryFail.Add(1)
		if !keep {
			m.end(errMConnLost, true)
		}
		return
	}
	m.lightOpening = false
	s2 := m.addSubLocked(sub, key, false)
	s2.push(rescueRec(mRecRescue, m.rscBi, m.rscRi))
	pushResendLocked(s2, m.rscBi, data)
	m.rscSub = s2
	if rs169Fix.Load() {
		// rs169 (M1): the replaced join carries nothing more (its records
		// are resent above): off the primary, still read (a late answer)
		if !s.retired {
			s.retired = true
			m.removeSubLocked(s)
		}
		m.rscAband = append(m.rscAband, s)
		m.prefPrimary = s2
		m.setPrimaryLocked()
	}
	m.tr.ev("rescue: retry RESCUE(%d,%d) on sub %d, resent %d", m.rscBi, m.rscRi, s2.idx, len(data))
	m.sendCond.Broadcast()
	m.mu.Unlock()
	rs145Stat.retry.Add(1)
	rtrace("%x iran rescue retry join %v resent %d", m.id, linkName(key), len(data))
	m.startSub(s2)
	m.kickMgr()
}

// rescueRetrySilentLocked (Iran, under mu, rescue watch): rs146. A rescue
// join whose own link went silent (its path died right after the join was
// sent: a second outage, or a whole Iran IP whose paths the silent test marks
// one by one) is as lost as one whose link closed, but that link closes only
// at its health timeout, long after the user gave up (rs145/rs146 lab: every
// lostjoin ended as endwait, RETRY never fired). Under HS2_MCONN_RESCUE_RETRY
// the silent join is abandoned (its death ends nothing) and replaced once,
// exactly as a closed one.
func (m *mConn) rescueRetrySilentLocked(set map[any]bool) {
	if !rescueRetryOn.Load() || !m.iran || m.ended || m.closing {
		return
	}
	if m.rscPhase == 1 && m.rscSub != nil && !m.rscRetried && set[m.rscSub.key] {
		if m.rescueRetryLocked(m.rscSub) {
			rs145Stat.retrySilent.Add(1)
			rtrace("%x iran rescue join silent: retry", m.id)
		}
	}
	if m.rrPhase == 1 && m.rrSub != nil && !m.rrRetried && set[m.rrSub.key] {
		if m.rrRetryLocked(m.rrSub) {
			rs145Stat.retrySilent.Add(1)
			rtrace("%x iran rrec join silent: retry", m.id)
		}
	}
}

// rrRetryLocked (Iran, under mu, subGone): the records rescue join s died
// before the answer: start the records rescue again on another path.
func (m *mConn) rrRetryLocked(s *msub) bool {
	if !rescueRetryOn.Load() || m.rrRetried || m.ended || m.closing {
		return false
	}
	m.rrRetried = true
	s.rrAband = true
	m.rrPhase = 0
	m.rrWait.Store(false)
	if rs169Fix.Load() { // rs169: the retry's snapshot extends this one (writes since may roll the ring past Rk)
		m.b375CoverLocked(^uint64(0)) // b375: the first rrStart's hold into the snapshot (rare path)
		m.rrSnapP, m.rrSnapP0 = m.rrSnap, m.rrSnap0
	}
	m.rrSnap = nil
	m.b375ReleaseLocked()
	m.rrDead = s.key
	m.lightOpening = true
	rs145Stat.retry.Add(1)
	go m.rrStart()
	return true
}
