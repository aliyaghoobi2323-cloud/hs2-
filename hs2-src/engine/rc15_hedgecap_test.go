package engine

import (
	"context"
	"os"
	"testing"
)

// rc15: Iran (K4M2 default) toward an rc14/rc12 exit (no ext bit 18) sends
// no role 7: a hedge open is a plain join, the first-sub gate keeps raw, and
// hedgeStartLocked's key check refuses. Toward an rc15 exit: role 7.
func TestRC15HedgeCapMixed(t *testing.T) {
	setLite(t, true)
	setHedge3(t, true)
	for _, newExit := range []bool{false, true} {
		l := newMCLink(capPortTags | capMConn)
		if newExit {
			l.mtr.peerInfo.Store(&peerInfo{V2: true, Caps: capPortTags | capMConn, Ext: capExtLiteHedge})
		}
		if hedgePeerOK(l) != newExit || hedgeLinkOK(l) != newExit || hedgeKeyOK(l) != newExit {
			t.Fatalf("newExit=%v: peerOK %v linkOK %v keyOK %v", newExit, hedgePeerOK(l), hedgeLinkOK(l), hedgeKeyOK(l))
		}
		m := mcManager(l)
		e := &mconnEdge{ctx: context.Background(), lm: m, port: 443, id: 77, first: true}
		if _, _, err := e.open(planeLight, []any{hedgeMark{}}); err != nil {
			t.Fatal(err)
		}
		h := l.firstWrite(t)
		want := mconnRoleJoin
		if newExit {
			want = mconnRoleHedge
		}
		if len(h) < 12 || h[11] != want { // mconnHeader: role last
			t.Fatalf("newExit=%v: role %v want %d", newExit, h, want)
		}
	}
	// a nil peerInfo (kindInfo not answered yet) is an old exit
	l := newMCLink(capPortTags | capMConn)
	l.mtr.peerInfo.Store(nil)
	if hedgePeerOK(l) || hedgeKeyOK(l) {
		t.Fatal("no peerInfo counted as a hedge exit")
	}
	// keys that are not links (fake openers) are not gated
	if !hedgeKeyOK(1) {
		t.Fatal("non-link key gated")
	}
}

// HEDGE3 off (HS2_LITE_HEDGE8=0): no gate, no bit announced.
func TestRC15HedgeCapOff(t *testing.T) {
	setLite(t, true)
	setHedge3(t, false)
	l := newMCLink(capPortTags | capMConn)
	if hedgePeerOK(l) || !hedgeLinkOK(l) || !hedgeKeyOK(l) {
		t.Fatal("gate with HEDGE3 off")
	}
	if infoExt()&capExtLiteHedge != 0 {
		t.Fatal("bit 18 announced with HEDGE3 off")
	}
	setHedge3(t, true)
	if mconnOn.Load() && infoExt()&capExtLiteHedge == 0 {
		t.Fatal("bit 18 not announced with HEDGE3 on")
	}
}

// the rc15 defaults are K4M2
func TestRC15HedgeDefaults(t *testing.T) {
	if osLookupHedge8() {
		t.Skip("HS2_LITE_HEDGE8 set in the env")
	}
	if hedge3Bytes != 4096 || hedge3Dur.Milliseconds() != 200 {
		t.Skipf("window env-tuned: %d %v", hedge3Bytes, hedge3Dur)
	}
	if i := rc15Init; !i.key || !i.h8 || !i.h7 || !i.h4 || !i.h3 || !i.h {
		t.Fatal("K4M2 not on by default")
	}
	if liteHedge10 || liteH6 || noRaw463 || b462Dbg {
		t.Fatal("an experiment key is on by default")
	}
}

func osLookupHedge8() bool {
	_, ok := os.LookupEnv("HS2_LITE_HEDGE8")
	return ok
}
