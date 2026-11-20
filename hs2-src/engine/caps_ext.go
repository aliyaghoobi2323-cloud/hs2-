package engine

// The kindInfo extended-caps owner table (peerinfo.go: ext after the ports,
// byte i bit j = ext bit 8i+j, infoExtMax bytes). One place for every branch,
// so two features written on separate branches cannot take the same bit (step3
// risk R12). TestCapsExtOwners checks every capExt* constant of this tree
// against it and that no two owners share a bit. A branch that takes a bit
// adds its row here first (also when the code lives on another branch).
//
// Checked 2026-11-19 against every local refs/heads (b331/b352/b356/b366/b367
// included) and proposals/*.md.
//
//	bit  0  capExtMConnStripe     peerinfo.go
//	bit  1  capExtUDPFast         another tree (not on a local branch): kept free
//	bit  2  capExtMConnStripeUp   mconn_stripe_up.go
//	bit  3  capExtMConnResend     mconn_stripe_evac.go
//	bit  4  capExtMConnOverlap    mconn_stripe_overlap.go
//	bit 5-7 free, but other trees skip them (rr156 note): do not use
//	bit  8  capExtMConnRescue     mconn_rescue.go
//	bit  9  capExtMConnRescueRec  mconn_rescue_rec.go
//	bit 10  capExtMConnKeepStall  mconn_stripe_keepstall.go
//	bit 11  capExtMConnRescueRst  mconn_rescue_rst.go
//	bit 12  capExtMConnRstEnd     mconn_rescue_rst.go
//	bit 13  capExtHalfClose       halfclose.go
//	bit 14  capExtMConnXS         mconn_xs261.go
//	bit 15  capExtZDU             zdu.go (b372, proposals/zdu350.md)
//	bit 16  capExtMConnHSRescue   b367 (mconn_b367_hsrescue.go, proposals/hsr367.md)
//	bit 17+ free
type capExtOwner struct {
	Bit   uint
	Name  string
	Owner string // file, or the branch that holds it when it is not in this tree
	Here  bool   // the constant is in this tree (TestCapsExtOwners checks its value)
}

var capExtOwners = []capExtOwner{
	{0, "capExtMConnStripe", "peerinfo.go", true},
	{1, "capExtUDPFast", "another tree", false},
	{2, "capExtMConnStripeUp", "mconn_stripe_up.go", true},
	{3, "capExtMConnResend", "mconn_stripe_evac.go", true},
	{4, "capExtMConnOverlap", "mconn_stripe_overlap.go", true},
	{5, "(skip)", "rr156: kept free", false},
	{6, "(skip)", "rr156: kept free", false},
	{7, "(skip)", "rr156: kept free", false},
	{8, "capExtMConnRescue", "mconn_rescue.go", true},
	{9, "capExtMConnRescueRec", "mconn_rescue_rec.go", true},
	{10, "capExtMConnKeepStall", "mconn_stripe_keepstall.go", true},
	{11, "capExtMConnRescueRst", "mconn_rescue_rst.go", true},
	{12, "capExtMConnRstEnd", "mconn_rescue_rst.go", true},
	{13, "capExtHalfClose", "halfclose.go", true},
	{14, "capExtMConnXS", "mconn_xs261.go", true},
	{15, "capExtZDU", "zdu.go", true},
	{16, "capExtMConnHSRescue", "b367 mconn_b367_hsrescue.go", false},
}
