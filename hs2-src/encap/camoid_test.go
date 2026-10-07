package encap

import "testing"

// camoLinkID hands out unique, non-zero ids that CLUSTER like a host's own
// related ping processes (a base plus small increasing steps), not the uniform
// spread over the whole 16-bit space that N random ids to one peer would show.
func TestCamoLinkIDClusters(t *testing.T) {
	camoIDBase = 20000
	camoIDNext = 0
	inUse := map[uint16]bool{}
	var ids []uint16
	for i := 0; i < 8; i++ {
		id := camoLinkID(inUse)
		if id == 0 {
			t.Fatal("zero id")
		}
		if inUse[id] {
			t.Fatalf("duplicate id %d", id)
		}
		inUse[id] = true
		ids = append(ids, id)
	}
	// Consecutive ids differ by at most the step bound (1..40): a tight cluster.
	for i := 1; i < len(ids); i++ {
		d := int(ids[i]) - int(ids[i-1])
		if d <= 0 || d > 40 {
			t.Fatalf("ids not a climbing cluster: %v (step %d at %d)", ids, d, i)
		}
	}
	// Total spread stays far below a uniform draw's (~8k expected over 8 ids).
	if spread := int(ids[len(ids)-1]) - int(ids[0]); spread > 8*40 {
		t.Fatalf("cluster too wide: spread %d over %v", spread, ids)
	}
}

// Even packed near a boundary it stays unique and non-zero (wrap is fine).
func TestCamoLinkIDWrapsSafely(t *testing.T) {
	camoIDBase = 0xfff0
	camoIDNext = 0
	inUse := map[uint16]bool{}
	for i := 0; i < 50; i++ {
		id := camoLinkID(inUse)
		if id == 0 || inUse[id] {
			t.Fatalf("bad id %d at %d (zero or dup)", id, i)
		}
		inUse[id] = true
	}
}
