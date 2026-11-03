package engine

import (
	"testing"
	"time"
)

// With HS2_PICK_CLASS a new connection goes to an unpressed link without a
// download before one with a download, even a busier one; pressure still
// ranks first. Without it the order is the one of before (least flowing).
func TestPickAvoidsHeavyLinks(t *testing.T) {
	setup := func() (*LinkManager, []*managedLink) {
		return pickPool(
			func(ml *managedLink) { ml.flowing, ml.heavy = 1, 1 },                    // a download, nothing else
			func(ml *managedLink) { ml.flowing = 4 },                                 // four interactive flows
			func(ml *managedLink) { ml.pressed, ml.flowing = true, 0 },               // pressed, empty
			func(ml *managedLink) { ml.flowing, ml.heavy = 2, 2; ml.users.Store(9) }, // two downloads
		)
	}
	pick := func(m *LinkManager) Link {
		l, rel, ok := m.Pick()
		if !ok {
			t.Fatal("no link")
		}
		rel()
		return l
	}
	old := pickClass
	defer func() { pickClass = old }()

	pickClass = false
	m, ls := setup()
	if l := pick(m); l != ls[0].link {
		t.Fatalf("without the key: want the least-flowing link (the download's), got another")
	}
	pickClass = true
	m, ls = setup()
	if l := pick(m); l != ls[1].link {
		t.Fatalf("with the key: want the busier link without a download")
	}
	// Every unpressed link carries a download: back to least flowing among them.
	m, ls = setup()
	ls[1].heavy = 1
	if l := pick(m); l != ls[0].link {
		t.Fatalf("all heavy: want the least flowing")
	}
}

// A stream at a download's rate counts as heavy; an interactive one (a game's
// few KB/s) is flowing but not heavy; a download that stopped stops counting
// within flowRecent.
func TestFlowStatsHeavy(t *testing.T) {
	l := &mtcpLink{flows: map[*countedStream]struct{}{}}
	dl, game := &countedStream{}, &countedStream{}
	l.flows[dl], l.flows[game] = struct{}{}, struct{}{}
	now := time.Now()
	var fs flowSnap
	for i := 0; i < 5; i++ { // 10 s: a 5 Mbit/s download beside a 40 kbit/s game
		now = now.Add(healthTick)
		dl.bytes.Add(625 << 10 * 2)
		game.bytes.Add(5 << 10 * 2)
		fs = l.flowStats(now, healthTick, time.Minute)
	}
	if fs.flowing != 2 || fs.heavy != 1 {
		t.Fatalf("flowing %d heavy %d, want 2 and 1", fs.flowing, fs.heavy)
	}
	for i := 0; i < 4; i++ { // the download ends; the game goes on
		now = now.Add(healthTick)
		game.bytes.Add(5 << 10 * 2)
		fs = l.flowStats(now, healthTick, time.Minute)
	}
	if fs.heavy != 0 {
		t.Fatalf("a finished download still counts as heavy %v later", 4*healthTick)
	}
}
