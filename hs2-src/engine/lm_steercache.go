package engine

import (
	"slices"
	"sort"
	"sync"
)

// lk165 (HS2_LM_FASTPICK): steerDrawLocked without its per-pick maps.
//
// steerDrawLocked ran for every new connection and sub: a map of the paths
// with a candidate link (cand on every link), the keys sorted, then the
// weights — 16% of the edge's CPU in the sc161 storm. The weights are a
// function of the steer state, which only steerTickLocked changes (under the
// write lock: steerGen moves), and of the set of paths that have a candidate.
// So, per steer tick and pool (the links in order, compared pointer by pointer
// on every draw: the slice is also filtered in place, so neither its address
// nor its length alone says it is the same pool): each link's path index (-1:
// no path or turned off; a link's path never changes, its steer.off only in
// the tick), and per (light, set of paths) the weights, built the first
// time that set is seen. A pick then only finds which paths have a candidate
// (cand called once per link until every path has one: usually a handful of
// calls) and draws with one rand.Float64, exactly as steerDrawLocked does:
// the same distribution from the same state.

// steerCacheMaxPaths: more distinct paths than this, the uncached draw (a
// path set is a bit mask; the top bit is light).
const steerCacheMaxPaths = 63

type steerCache struct {
	gen   uint64
	links []*managedLink // a copy of m.links at build
	paths []string       // sorted distinct paths of the links not turned off
	pidx  []int8         // per m.links index: its index in paths, -1 none
	w     sync.Map       // light<<63 | path mask -> *steerW
}

// steerW: the draw for one set of paths (tot <= 0: "", no steering).
type steerW struct {
	keys []string
	ws   []float64
	tot  float64
}

// steerDrawCached is steerDrawLocked (steering active) from the cache; ok
// false: not cacheable (too many paths), the caller draws uncached. Caller
// holds m.mu (read or write).
func (m *LinkManager) steerDrawCached(light bool, cand func(*managedLink) bool) (string, bool) {
	c := m.steerC.Load()
	gen := m.steerGen.Load()
	if c == nil || c.gen != gen || !slices.Equal(c.links, m.links) {
		if c = m.steerCacheBuildLocked(gen); c == nil {
			return "", false
		}
		m.steerC.Store(c)
	}
	var mask uint64
	found := 0
	for i, ml := range m.links {
		k := c.pidx[i]
		if k < 0 || mask&(1<<uint(k)) != 0 {
			continue
		}
		if cand(ml) {
			mask |= 1 << uint(k)
			if found++; found == len(c.paths) {
				break
			}
		}
	}
	if found < 2 {
		return "", true
	}
	key := mask
	if light {
		key |= 1 << 63
	}
	var w *steerW
	if v, ok := c.w.Load(key); ok {
		w = v.(*steerW)
	} else {
		keys := make([]string, 0, found)
		for k, p := range c.paths {
			if mask&(1<<uint(k)) != 0 {
				keys = append(keys, p)
			}
		}
		ws, tot := m.steerWeightsLocked(light, keys)
		w = &steerW{keys: keys, ws: ws, tot: tot}
		c.w.Store(key, w) // a concurrent builder stores the same weights
	}
	if w.tot <= 0 {
		return "", true
	}
	return steerPick(w.keys, w.ws, w.tot), true
}

// steerCacheBuildLocked indexes the pool's paths (nil: too many).
func (m *LinkManager) steerCacheBuildLocked(gen uint64) *steerCache {
	c := &steerCache{gen: gen, links: slices.Clone(m.links), pidx: make([]int8, len(m.links))}
	for i, ml := range m.links {
		c.pidx[i] = -1
		p := mlPath(ml)
		if p == "" || ml.steer.off {
			continue
		}
		known := false
		for _, q := range c.paths {
			if q == p {
				known = true
				break
			}
		}
		if !known {
			if len(c.paths) == steerCacheMaxPaths {
				return nil
			}
			c.paths = append(c.paths, p)
		}
	}
	sort.Strings(c.paths)
	for i, ml := range m.links {
		p := mlPath(ml)
		if p == "" || ml.steer.off {
			continue
		}
		for k, q := range c.paths {
			if q == p {
				c.pidx[i] = int8(k)
				break
			}
		}
	}
	return c
}
