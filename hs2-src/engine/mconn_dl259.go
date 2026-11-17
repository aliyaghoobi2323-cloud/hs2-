package engine

import (
	"cmp"
	"log"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// dl259: the download stripe's queue gate judged PATH BY PATH
// (HS2_DL259_PATHQ, the exit; default on through the envdef table since
// dr259, OFF by default since the prod same-hour A/B of 2026-11-17 halved
// single-stream DL with it on: HS2_DL259_PATHQ=1 turns it on).
//
// The exit's gate (stripeWidthLocked) takes one median queue over the busy
// links that carry none of the download's subs. In lab3 dr257 (10 x 1 Gbit
// paths, 74 ms, 0.1-0.8 % loss, crowd 1000, 3 light bg flows) a single
// download's primary filled its own path (srtt 81 -> 103-108 ms): the busy
// links of other users on THAT path showed the queue, the median went over
// the 3 ms target, and the width sat at 0 for the whole test (35-77 cuts),
// so everything stayed on that one path (550-700 Mbit/s) while 2 joined
// subs on unloaded paths stayed idle. Tests where the gate opened (w 3-4)
// reached 830-1480.
//
// With the key the same queue is taken per path (the link meter's path key,
// "local>remote" IPs): a path whose busy links (not this connection's)
// queue over the target is cut for this connection (its stripe subs take no
// chunk and no slot of the width) until its queue is under half the target
// and stripeCutHold has passed; the width itself is judged only on the
// paths that carry a stripe sub and are not cut. The primary's own path no
// longer holds the width at 0 by itself, so the stripe may spread onto
// unloaded paths, while no chunk is added to a path whose queue is over the
// target (the light users there keep it). Paths with no busy link of
// others show no queue (as the global gate: nobody to yield to).
var dl259On atomic.Bool

func init() { dl259On.Store(os.Getenv("HS2_DL259_PATHQ") == "1") }

var dl259Stat struct {
	cuts, opens, gates atomic.Uint64
	once               sync.Once
}

// dl259LogStart: one line every 10 s while the key is used (lab evidence).
func dl259LogStart() {
	dl259Stat.once.Do(func() {
		go func() {
			for range time.Tick(10 * time.Second) {
				if g := dl259Stat.gates.Swap(0); g > 0 {
					log.Printf("dl259 pathq 10s: gates=%d path-cuts=%d path-opens=%d", g, dl259Stat.cuts.Swap(0), dl259Stat.opens.Swap(0))
				}
			}
		}()
	})
}

// dl259Ent / dl259Path (b264 L4): the busy links of one path with their
// queue, sorted by queue, built once per measurement (p.at) and per change
// of the tracked set (p.gen, which delay() also bumps per measurement) and shared by every gate decision until the
// next one; a decision only walks each path once to skip its own links.
type dl259Ent struct {
	mtr *linkMeter
	q   time.Duration
}

type dl259Path struct {
	path string
	ent  []dl259Ent
}

// dl259Uncached (tests): the gate uses pathQUncachedLocked (the pre-b264
// code) instead of the cache, to prove both give the same decisions.
var dl259Uncached atomic.Bool

// pathQCacheLocked (under p.mu): (re)builds the per-path sorted queues when
// a measurement or a track/untrack happened since the last build.
func (p *stripeQ) pathQCacheLocked() []dl259Path {
	if p.pqOK && p.pqAt.Equal(p.at) && p.pqGen == p.gen {
		return p.pq
	}
	p.pqOK, p.pqAt, p.pqGen = true, p.at, p.gen
	if p.pqIdx == nil {
		p.pqIdx = map[string]int{}
	}
	clear(p.pqIdx)
	for i := range p.pq {
		p.pq[i].ent = p.pq[i].ent[:0]
	}
	n := 0
	for mtr, l := range p.links {
		if mtr == nil || mtr.path == "" || !l.busy {
			continue
		}
		i, ok := p.pqIdx[mtr.path]
		if !ok {
			i = n
			n++
			p.pqIdx[mtr.path] = i
			if i == len(p.pq) {
				p.pq = append(p.pq, dl259Path{})
			}
			p.pq[i].path = mtr.path
		}
		p.pq[i].ent = append(p.pq[i].ent, dl259Ent{mtr, l.q})
	}
	for i := n; i < len(p.pq); i++ {
		p.pq[i] = dl259Path{} // paths gone: drop their memory
	}
	p.pq = p.pq[:n]
	for i := range p.pq {
		slices.SortFunc(p.pq[i].ent, func(a, b dl259Ent) int { return cmp.Compare(a.q, b.q) })
	}
	return p.pq
}

// pathQIntoLocked (under p.mu): out (cleared) gets each path's median queue
// over its busy links not in own (paths with none are left out): the same
// values as pathQUncachedLocked, read from the cache.
func (p *stripeQ) pathQIntoLocked(own []*linkMeter, out map[string]time.Duration) {
	clear(out)
	if dl259Uncached.Load() {
		for k, v := range p.pathQUncachedLocked(own) {
			out[k] = v
		}
		return
	}
	for _, pp := range p.pathQCacheLocked() {
		k := 0
		if len(own) > 0 {
			for _, e := range pp.ent {
				if slices.Contains(own, e.mtr) {
					k++
				}
			}
		}
		left := len(pp.ent) - k
		if left == 0 {
			continue
		}
		want := left / 2 // ds[len(ds)/2] of the old per-call sorted slice
		for _, e := range pp.ent {
			if k > 0 && slices.Contains(own, e.mtr) {
				continue
			}
			if want == 0 {
				out[pp.path] = e.q
				break
			}
			want--
		}
	}
}

// pathQUncachedLocked (under p.mu): the pre-b264 per-path median (the
// dl259Uncached reference for tests).
func (p *stripeQ) pathQUncachedLocked(own []*linkMeter) map[string]time.Duration {
	by := map[string][]time.Duration{}
	for mtr, l := range p.links {
		if mtr == nil || mtr.path == "" || !l.busy || slices.Contains(own, mtr) {
			continue
		}
		by[mtr.path] = append(by[mtr.path], l.q)
	}
	out := make(map[string]time.Duration, len(by))
	for k, ds := range by {
		slices.Sort(ds)
		out[k] = ds[len(ds)/2]
	}
	return out
}

// pathQ: each path's median queue over its busy links not in own, as a
// fresh map (tests, diagnostics).
func (p *stripeQ) pathQ(own []*linkMeter) map[string]time.Duration {
	out := map[string]time.Duration{}
	p.mu.Lock()
	p.pathQIntoLocked(own, out)
	p.mu.Unlock()
	return out
}

// dl259GateLocked (exit, under m.mu, key on): updates the connection's cut
// paths from the per-path queue and returns the queue the width is judged
// on: the worst (base-adjusted) queue among the paths that carry a live
// stripe sub other than p and are not cut (0: none).
func (m *mConn) dl259GateLocked(pq *stripeQ, own []*linkMeter, p *msub, base, target time.Duration, now time.Time) time.Duration {
	dl259LogStart()
	dl259Stat.gates.Add(1)
	if m.dl259Q == nil {
		m.dl259Q = map[string]time.Duration{}
	}
	qs := m.dl259Q // b264 L4: reused per conn (under m.mu)
	pq.mu.Lock()
	pq.pathQIntoLocked(own, qs)
	pq.mu.Unlock()
	if m.dl259Cut == nil {
		m.dl259Cut = map[string]time.Time{}
	}
	for path, q := range qs {
		q = max(0, q-base)
		if q > target {
			if _, was := m.dl259Cut[path]; !was {
				dl259Stat.cuts.Add(1)
			}
			m.dl259Cut[path] = now.Add(stripeCutHold)
		}
	}
	for path, until := range m.dl259Cut {
		if now.Before(until) {
			continue
		}
		if q, ok := qs[path]; !ok || max(0, q-base) < target/2 {
			delete(m.dl259Cut, path)
			dl259Stat.opens.Add(1)
		}
	}
	var g time.Duration
	for _, s := range m.subs {
		if !s.stripe || s == p || s.retired || s.dead {
			continue
		}
		mt := m.dl259Meter(s)
		if mt == nil || mt.path == "" {
			continue
		}
		if _, cut := m.dl259Cut[mt.path]; cut {
			continue
		}
		g = max(g, max(0, qs[mt.path]-base))
	}
	return g
}

// dl259TestMeter (tests): the sub's meter instead of subMeterOf.
var dl259TestMeter func(*msub) *linkMeter

func (m *mConn) dl259Meter(s *msub) *linkMeter {
	if dl259TestMeter != nil {
		return dl259TestMeter(s)
	}
	return m.subMeterOf(s)
}

// dl259SubCutLocked (exit, under m.mu): s's path is cut for this connection.
func (m *mConn) dl259SubCutLocked(s *msub) bool {
	if !dl259On.Load() || m.iran || len(m.dl259Cut) == 0 {
		return false
	}
	mt := m.dl259Meter(s)
	if mt == nil || mt.path == "" {
		return false
	}
	_, cut := m.dl259Cut[mt.path]
	return cut
}
