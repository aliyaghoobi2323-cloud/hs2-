package censor

import (
	"fmt"
	"sync/atomic"
	"time"
)

// b429: counters that show the b414 abort-report path working in
// production without a recording. Atomic adds only; they never feed a
// decision. Kharej side (the evaluator): abort reports received, the
// aborts they carried, resets credited to them, orphan events (and of them
// the ones with evidence that started the long hold), resets held for a
// report and resets released unclaimed. Iran side (the engine): own
// ECONNABORTED link deaths counted, and of them the ones that rode at
// least one control ping (reported).
var peerStat struct {
	reports, repAborts, credited     atomic.Uint64
	orphans, orphanHolds             atomic.Uint64
	held, released                   atomic.Uint64
	ownAborts, ownReported, edgeSigs atomic.Uint64
	ownRstCloses, ownStallCloses     atomic.Uint64 // b430, o432
}

// PeerStatus is the status block's `peer` part (b429).
type PeerStatus struct {
	// Kharej side
	Reports     uint64 `json:"reports"`      // abort reports received (pings whose count rose)
	RepAborts   uint64 `json:"rep_aborts"`   // aborts those reports carried
	Credited    uint64 `json:"credited"`     // resets credited to a report (not judged)
	Orphans     uint64 `json:"orphans"`      // last reporting link on an IP ended
	OrphanHolds uint64 `json:"orphan_holds"` // of them with a recent rise: long hold
	Held        uint64 `json:"held"`         // resets held for a report
	Released    uint64 `json:"released"`     // held resets no report claimed (judged)
	EdgeSigs    uint64 `json:"edge_sigs"`    // first report signals (an IP's edge reports)
	// Iran side
	OwnAborts   uint64 `json:"own_aborts"`   // own ECONNABORTED link deaths counted
	OwnReported uint64 `json:"own_reported"` // of them carried by a control ping
	// b430: of own_aborts, the links hs2 closed itself (retire, LB or
	// probation drop, watchdog, shrink, ZDU) while the kernel still held
	// unread data, so the kernel sent the Kharej end an RST, not a FIN.
	OwnRstCloses uint64 `json:"own_rst_closes"`
	// o432: of own_aborts, the links hs2 closed itself with nothing unread
	// (a FIN) while the path had stopped answering (our segments unacked
	// through 2+ retransmission timeouts): the FIN is likely lost, and the
	// Kharej end's next data meets our closed socket, which answers with an
	// RST before any FIN arrives (lab o432: stuck-link drops, Kharej held
	// and released those resets).
	OwnStallCloses uint64 `json:"own_stall_closes"`
}

// NoteOwnAbort counts one own abort on the Iran side (b429 status only).
func NoteOwnAbort() { peerStat.ownAborts.Add(1) }

// NoteOwnAbortsReported counts n own aborts that a control ping carried for
// the first time (b429 status only).
func NoteOwnAbortsReported(n uint64) { peerStat.ownReported.Add(n) }

// NoteOwnRstClose (b430) counts one own link close the kernel sent as an
// RST (unread data at close); the caller counts it as an own abort too.
func NoteOwnRstClose() { peerStat.ownRstCloses.Add(1) }

// NoteOwnStallClose (o432) counts one own FIN close on a stalled path (see
// PeerStatus.OwnStallCloses); the caller counts it as an own abort too.
func NoteOwnStallClose() { peerStat.ownStallCloses.Add(1) }

func peerStatus() *PeerStatus {
	p := &peerStat
	st := &PeerStatus{Reports: p.reports.Load(), RepAborts: p.repAborts.Load(), Credited: p.credited.Load(),
		Orphans: p.orphans.Load(), OrphanHolds: p.orphanHolds.Load(), Held: p.held.Load(), Released: p.released.Load(),
		EdgeSigs: p.edgeSigs.Load(), OwnAborts: p.ownAborts.Load(), OwnReported: p.ownReported.Load(),
		OwnRstCloses: p.ownRstCloses.Load(), OwnStallCloses: p.ownStallCloses.Load()}
	if *st == (PeerStatus{}) {
		return nil
	}
	return st
}

// PeerCounters returns the counters now (b429; zero when none moved).
func PeerCounters() PeerStatus {
	if p := peerStatus(); p != nil {
		return *p
	}
	return PeerStatus{}
}

// PeerLogEvery is how often PeerLogLine may return a line.
const PeerLogEvery = 10 * time.Minute

var peerLog struct {
	at   atomic.Int64 // unix ns of the last line
	last atomic.Pointer[PeerStatus]
}

// PeerLogLine (b429) returns the periodic censor peer-counter line, or ""
// when it is not due (PeerLogEvery since the last one) or the counters did
// not change since the last line. Called from the status writer, never by
// the evaluator, so a replay's output stays the same.
func PeerLogLine(now time.Time) string {
	if !active.Load() && !disabled.Load() {
		return ""
	}
	if at := peerLog.at.Load(); at != 0 && now.Sub(time.Unix(0, at)) < PeerLogEvery {
		return ""
	}
	st := peerStatus()
	if st == nil {
		return ""
	}
	if l := peerLog.last.Load(); l != nil && *l == *st {
		return ""
	}
	peerLog.at.Store(now.UnixNano())
	peerLog.last.Store(st)
	return fmt.Sprintf("censor351: peer kharej: reports=%d rep_aborts=%d credited=%d orphans=%d orphan_holds=%d held=%d released=%d edge_sigs=%d; iran: own_aborts=%d own_reported=%d own_rst_closes=%d own_stall_closes=%d",
		st.Reports, st.RepAborts, st.Credited, st.Orphans, st.OrphanHolds, st.Held, st.Released, st.EdgeSigs, st.OwnAborts, st.OwnReported, st.OwnRstCloses, st.OwnStallCloses)
}
