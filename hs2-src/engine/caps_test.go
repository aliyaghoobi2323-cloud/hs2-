package engine

import "testing"

// TestCapsDistinct: every kindInfo capability bit is its own bit of the one
// caps byte. Two features written on separate branches once both took 1<<4,
// so a peer advertising one was read as supporting the other.
func TestCapsDistinct(t *testing.T) {
	caps := map[string]int{
		"capPortTags":  capPortTags,
		"capL3Quiet":   capL3Quiet,
		"capL3Dup":     capL3Dup,
		"capMConn":     capMConn,
		"capMConnRaw":  capMConnRaw,
		"capLBAdvert":  capLBAdvert,
		"capLB":        capLB,
		"capQuietIdle": capQuietIdle,
	}
	seen := map[int]string{}
	for name, v := range caps {
		if v <= 0 || v > 0xff || v&(v-1) != 0 {
			t.Fatalf("%s = %#x: not a single bit of the caps byte", name, v)
		}
		if other, ok := seen[v]; ok {
			t.Fatalf("%s and %s share bit %#x", name, other, v)
		}
		seen[v] = name
	}
}

// TestCapsExtDistinct: the same for the extended caps (kindInfo's ext after
// the ports, peerinfo.go), within what this version sends (infoExtMax bytes).
func TestCapsExtDistinct(t *testing.T) {
	caps := map[string]uint32{
		"capExtMConnStripe":                         capExtMConnStripe,
		"capExtMConnStripeUp":                       capExtMConnStripeUp,
		"capExtMConnResend":                         capExtMConnResend,
		"capExtMConnOverlap":                        capExtMConnOverlap,
		"capExtMConnRescue":                         capExtMConnRescue,
		"capExtMConnRescueRec":                      capExtMConnRescueRec,
		"capExtMConnKeepStall":                      capExtMConnKeepStall,
		"capExtMConnRescueRst":                      capExtMConnRescueRst,
		"capExtMConnRstEnd":                         capExtMConnRstEnd,
		"capExtHalfClose":                           capExtHalfClose,
		"capExtMConnXS":                             capExtMConnXS,
		"capExtZDU":                                 capExtZDU,
		"capExtMConnHSRescue (b367, reserved here)": 1 << 16,
	}
	seen := map[uint32]string{}
	for name, v := range caps {
		if v == 0 || v&(v-1) != 0 || uint64(v) >= 1<<(8*infoExtMax) {
			t.Fatalf("%s = %#x: not a single bit of the ext", name, v)
		}
		if other, ok := seen[v]; ok {
			t.Fatalf("%s and %s share bit %#x", name, other, v)
		}
		seen[v] = name
	}
}

// TestCapsExtOwners: the owner table (caps_ext.go) has one row per bit, every
// ext constant of this tree sits at its row's bit, ZDU keeps bit 15 and b367
// bit 16, and with every key on infoExt announces only owned bits.
func TestCapsExtOwners(t *testing.T) {
	here := map[string]uint32{
		"capExtMConnStripe":    capExtMConnStripe,
		"capExtMConnStripeUp":  capExtMConnStripeUp,
		"capExtMConnResend":    capExtMConnResend,
		"capExtMConnOverlap":   capExtMConnOverlap,
		"capExtMConnRescue":    capExtMConnRescue,
		"capExtMConnRescueRec": capExtMConnRescueRec,
		"capExtMConnKeepStall": capExtMConnKeepStall,
		"capExtMConnRescueRst": capExtMConnRescueRst,
		"capExtMConnRstEnd":    capExtMConnRstEnd,
		"capExtHalfClose":      capExtHalfClose,
		"capExtMConnXS":        capExtMConnXS,
		"capExtZDU":            capExtZDU,
	}
	bits := map[uint]string{}
	var owned uint32
	for _, o := range capExtOwners {
		if o.Bit >= 8*infoExtMax {
			t.Fatalf("%s: bit %d past infoExtMax", o.Name, o.Bit)
		}
		if prev, ok := bits[o.Bit]; ok {
			t.Fatalf("bit %d owned by %s and %s", o.Bit, prev, o.Name)
		}
		bits[o.Bit] = o.Name
		owned |= 1 << o.Bit
		if o.Here {
			v, ok := here[o.Name]
			if !ok {
				t.Fatalf("%s marked in this tree but not checked here", o.Name)
			}
			if v != 1<<o.Bit {
				t.Fatalf("%s = %#x, table says bit %d", o.Name, v, o.Bit)
			}
			delete(here, o.Name)
		}
	}
	if len(here) != 0 {
		t.Fatalf("constants without a table row: %v", here)
	}
	if bits[15] != "capExtZDU" || bits[16] != "capExtMConnHSRescue" {
		t.Fatalf("bit 15 %q, bit 16 %q", bits[15], bits[16])
	}
	// every key on: no announced bit outside the table
	oldOn, oldLite, oldRes, oldRec, oldRst := mconnOn.Load(), mconnLite.Load(), rescueOn.Load(), rescueRecOn.Load(), rescueRstOn.Load()
	oldK, oldUp, oldResend, oldKS, oldXS, oldHC, oldZ := stripeK.Load(), stripeUp.Load(), stripeResend.Load(), keepStallOn.Load(), xs261On.Load(), halfCloseOn.Load(), zduOn.Load()
	t.Cleanup(func() {
		mconnOn.Store(oldOn)
		mconnLite.Store(oldLite)
		rescueOn.Store(oldRes)
		rescueRecOn.Store(oldRec)
		rescueRstOn.Store(oldRst)
		stripeK.Store(oldK)
		stripeUp.Store(oldUp)
		stripeResend.Store(oldResend)
		keepStallOn.Store(oldKS)
		xs261On.Store(oldXS)
		halfCloseOn.Store(oldHC)
		zduOn.Store(oldZ)
	})
	mconnOn.Store(true)
	mconnLite.Store(true)
	rescueOn.Store(true)
	rescueRecOn.Store(true)
	rescueRstOn.Store(true)
	stripeK.Store(2)
	stripeUp.Store(true)
	stripeResend.Store(true)
	keepStallOn.Store(true)
	xs261On.Store(true)
	halfCloseOn.Store(true)
	zduOn.Store(true)
	if e := infoExt(); e&^owned != 0 || e&capExtZDU == 0 {
		t.Fatalf("infoExt %#x announces unowned bits %#x", e, e&^owned)
	}
}
