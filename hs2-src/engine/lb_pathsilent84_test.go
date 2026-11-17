package engine

import "testing"

func TestPathSilentPaths(t *testing.T) {
	mk := func(path string, n, silent int) []psLink {
		var out []psLink
		for i := 0; i < n; i++ {
			out = append(out, psLink{path: path, silent: i < silent})
		}
		return out
	}
	cat := func(ls ...[]psLink) []psLink {
		var out []psLink
		for _, l := range ls {
			out = append(out, l...)
		}
		return out
	}
	// blackholed path e: all silent, others hear
	if got := pathSilentPaths(cat(mk("a", 20, 1), mk("e", 20, 20), mk("h", 20, 2))); len(got) != 1 || !got["e"] {
		t.Fatalf("blackhole: got %v", got)
	}
	// 80 % edge: 16 of 20 silent counts, 15 does not
	if got := pathSilentPaths(cat(mk("a", 20, 0), mk("e", 20, 16))); !got["e"] {
		t.Fatalf("16/20: got %v", got)
	}
	if got := pathSilentPaths(cat(mk("a", 20, 0), mk("e", 20, 15))); got["e"] {
		t.Fatalf("15/20: got %v", got)
	}
	// lossy path where a few links are momentarily silent: not marked
	if got := pathSilentPaths(cat(mk("a", 20, 0), mk("h", 20, 5))); len(got) != 0 {
		t.Fatalf("lossy: got %v", got)
	}
	// everything silent (our side's network, CPU stall): nothing marked
	if got := pathSilentPaths(cat(mk("a", 20, 20), mk("e", 20, 20))); len(got) != 0 {
		t.Fatalf("all silent: got %v", got)
	}
	// too few links on the path
	if got := pathSilentPaths(cat(mk("a", 20, 0), mk("e", 2, 2))); len(got) != 0 {
		t.Fatalf("2 links: got %v", got)
	}
	// no other hearing path with enough links
	if got := pathSilentPaths(cat(mk("a", 2, 0), mk("e", 20, 20))); len(got) != 0 {
		t.Fatalf("no witness: got %v", got)
	}
}
