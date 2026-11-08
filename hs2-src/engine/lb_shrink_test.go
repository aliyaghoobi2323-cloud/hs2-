package engine

import (
	"context"
	"testing"
)

// shrinkPool is a pool of links on paths ("local>remote" keys); rate[i] sets
// the retire order (lowest first).
func shrinkPool(paths []string, rate []float64) (*LinkManager, []*mcLink) {
	m, ls := pathLinks(paths...)
	for i, ml := range m.links {
		ml.rate10 = rate[i]
	}
	return m, ls
}

func servingPerKey(m *LinkManager) map[string]int {
	got := map[string]int{}
	for _, ml := range m.links {
		if !ml.retiring {
			got[mlPath(ml)]++
		}
	}
	return got
}

// A shrink retires from the path most over its share, not just the links the
// usual order prefers: 6/4 on a 50/50 pair shrunk to 8 leaves 4/4, although
// the usual order alone (the 4 links of b are the idlest) would leave 6/2.
func TestLBShrinkKeepsShares(t *testing.T) {
	a, b := "10.1.0.1>10.0.0.9", "10.1.0.2>10.0.0.9"
	paths := []string{a, a, a, a, a, a, b, b, b, b}
	rate := []float64{100, 101, 102, 103, 104, 105, 1, 2, 3, 4}
	m, _ := shrinkPool(paths, rate)
	mine := []LBAdvertIP{{IP: "10.1.0.1", Weight: 50}, {IP: "10.1.0.2", Weight: 50}}
	m.shrinkShares = func(ms []*managedLink) ([]string, map[string]int) { return revShrinkKeys(ms, mine) }
	m.reconcile(context.Background(), 8)
	if got := servingPerKey(m); got[a] != 4 || got[b] != 4 {
		t.Fatalf("after shrink to 8: %d/%d serving, want 4/4", got[a], got[b])
	}
	// Within a path the usual order still decides: a's two idlest went.
	for i, ml := range m.links {
		if want := i < 2; ml.retiring != want {
			t.Fatalf("link %d retiring=%v, want %v", i, ml.retiring, want)
		}
	}
	// Further down, still by share.
	m.reconcile(context.Background(), 2)
	if got := servingPerKey(m); got[a] != 1 || got[b] != 1 {
		t.Fatalf("after shrink to 2: %d/%d serving, want 1/1", got[a], got[b])
	}
}

// One path (or no weights): exactly the usual order, as without lb.
func TestLBShrinkOnePathUnchanged(t *testing.T) {
	a := "10.1.0.1>10.0.0.9"
	rate := []float64{5, 1, 4, 2, 3, 0}
	for _, mine := range [][]LBAdvertIP{{{IP: "10.1.0.1", Weight: 100}}, nil} {
		m, _ := shrinkPool([]string{a, a, a, a, a, a}, rate)
		m.shrinkShares = func(ms []*managedLink) ([]string, map[string]int) { return revShrinkKeys(ms, mine) }
		ref, _ := shrinkPool([]string{a, a, a, a, a, a}, rate) // lb off
		m.reconcile(context.Background(), 3)
		ref.reconcile(context.Background(), 3)
		for i := range m.links {
			if m.links[i].retiring != ref.links[i].retiring {
				t.Fatalf("mine %v: link %d retiring=%v, without lb %v", mine, i, m.links[i].retiring, ref.links[i].retiring)
			}
		}
	}
}

// shareShrinkPick over three weighted paths at their shares plus a path with
// none (cooled): the weightless path empties first, then after every pick the
// links left are within one of their share.
func TestLBShareShrinkPick(t *testing.T) {
	var keys []string
	for _, c := range []struct {
		key string
		n   int
	}{{"a", 20}, {"b", 12}, {"c", 8}, {"d", 4}} {
		for i := 0; i < c.n; i++ {
			keys = append(keys, c.key)
		}
	}
	// interleaved, so the order alone would not keep the shares
	for i := range keys {
		j := (i * 7) % len(keys)
		keys[i], keys[j] = keys[j], keys[i]
	}
	w := map[string]int{"a": 50, "b": 30, "c": 20} // d: no share
	for k := 0; k <= len(keys); k++ {
		out := shareShrinkPick(keys, w, k)
		left := map[string]int{}
		n := 0
		for i, gone := range out {
			if !gone {
				left[keys[i]]++
				n++
			}
		}
		if n != len(keys)-k {
			t.Fatalf("k=%d: %d left", k, n)
		}
		if k < 4 {
			if left["d"] != 4-k {
				t.Fatalf("k=%d: %d left on the weightless path, want %d", k, left["d"], 4-k)
			}
			continue
		}
		if left["d"] != 0 {
			t.Fatalf("k=%d: %d links left on the weightless path", k, left["d"])
		}
		for key, pw := range w {
			share := float64(pw*n) / 100
			if d := float64(left[key]) - share; d > 1 || d < -1 {
				t.Fatalf("k=%d path %s: %d left, share %.1f", k, key, left[key], share)
			}
		}
	}
}

// The direct edge keys links by the balancer's paths: a cooled path has no
// share, a link the balancer does not hold has no path.
func TestLBShrinkKeysDirect(t *testing.T) {
	lb, _ := NewLB(twoPaths(50, 50), nil)
	var held []Link
	for i := 0; i < 4; i++ {
		v, err := lb.dialVia(func(p LBPath) (any, error) { return Link(newMCLink(0)), nil })
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, v.(Link))
	}
	keys, w := lb.shrinkKeys(append(held, Link(newMCLink(0))))
	na, nb := lb.paths[0].Name(), lb.paths[1].Name()
	cnt := map[string]int{}
	for _, k := range keys {
		cnt[k]++
	}
	if cnt[na] != 2 || cnt[nb] != 2 || cnt[""] != 1 || w[na] != w[nb] || w[na] == 0 {
		t.Fatalf("keys %v weights %v", keys, w)
	}
	lb.mu.Lock()
	lb.until[1] = lb.now().Add(lbCooldown)
	lb.mu.Unlock()
	if _, w = lb.shrinkKeys(held); w[nb] != 0 || w[na] == 0 {
		t.Fatalf("cooled path keeps a share: %v", w)
	}
}
