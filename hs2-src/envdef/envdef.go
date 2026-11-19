// Package envdef holds the default values of the HS2_ environment keys that
// production (tunnel 1, reverse l3mtcp) runs with, so a plain install with no
// Environment= lines behaves like it.
//
// The defaults are dormant until Activate: package-level reads at process
// start see only the real environment (the old behaviour), and cmd/hs2 turns
// the table on — for this server's side — only for the l3mtcp carrier in
// reverse, the layout the values were proven on. Every other carrier and
// direct mode keep the old defaults (off).
//
// Rules:
//   - a key present in the environment always wins, whatever its value —
//     HS2_X=0, HS2_X=off and an empty HS2_X= all mean what they meant before
//     (off for every switch in the table);
//   - HS2_DEFAULTS=legacy (or 0 / off) keeps the table dormant: every key is
//     back to its old default unless set explicitly.
package envdef

import (
	"os"
	"sort"
	"strings"
	"sync/atomic"
)

// Side is where a default applies.
type Side uint8

const (
	Both Side = iota // edge (iran, mode "dial") and exit (kharej, mode "listen")
	Edge             // only the edge
	Exit             // only the exit
)

func (s Side) String() string {
	switch s {
	case Edge:
		return "edge"
	case Exit:
		return "exit"
	}
	return "both"
}

// Key is one default.
type Key struct {
	Name  string
	Value string
	Side  Side
}

// Table is the production set (tunnel 1 since 2026-11-10). Left out on
// purpose: HS2_MCONN_STRIPE_JUMP_MS (no proven gain).
var Table = []Key{
	{"HS2_SHAPE_FAST", "1", Both},
	{"HS2_SHAPE_SMALL", "1", Both},
	{"HS2_LINK_PROBATION", "1", Edge},
	{"HS2_PARTIAL_REFILL", "1", Edge},
	{"HS2_PICK_LANE", "on", Edge},
	{"HS2_L3_LANE", "1", Both},
	{"HS2_HEAVY_BACKLOG", "1", Both},
	{"HS2_L3_DUP", "1", Both},
	{"HS2_MCONN", "lite", Both},
	{"HS2_FAST_LINK", "1", Both},
	{"HS2_MCONN_BATCH", "1", Both},
	{"HS2_SHAPE_BULK", "1", Both},
	{"HS2_QUIET_IDLE", "1", Both},
	{"HS2_TLS_RESUME", "1", Both},
	{"HS2_AUTH_PAD", "1", Both},
	{"HS2_COVER_NGINX", "1", Both},
	{"HS2_MCONN_DEADLINE_FIX", "1", Both},
	{"HS2_MCONN_CLOSE_HS", "1", Both},
	{"HS2_LB_AUTO2", "1", Both},
	{"HS2_MCONN_CALL", "1", Both},
	{"HS2_MCONN_CALL_AVOID", "1", Both},
	{"HS2_MCONN_STRIPE", "8", Both},
	{"HS2_MCONN_STRIPE_UP", "1", Both},
	{"HS2_MCONN_STRIPE_SHARE", "1", Both},
	{"HS2_MCONN_STRIPE_REL", "1", Both},
	{"HS2_MCONN_STRIPE_RESEND", "1", Both}, // rel49: a striped connection survives its links' death (§11.9.5)
	{"HS2_LB_CUT_DRAIN", "1", Both},
	{"HS2_MCONN_STRIPE_QWIN_MS", "8000", Both},
	{"HS2_SMUX_AUTOWIN", "1", Both},
	{"HS2_SMUX_AUTOWIN_REL", "1", Both},
	{"HS2_SMUX_AUTOWIN_SHARE_MB", "32", Both},
	{"HS2_STRIPE_AW_SOLO", "1", Edge},
	{"HS2_LB_FAST_RECOVER", "1", Both},
	{"HS2_LB_START_GRACE_S", "20", Both},
	{"HS2_STRIPE_AW_HEAVY", "2", Edge},
	{"HS2_LB_FILL_PCT", "25", Exit},
	{"HS2_LB_SPARE_PER_ROUND", "3", Exit},
	{"HS2_LB_SPARE_EDGE", "1", Exit},  // sp176: spareForGap asks only for a spare the edge would serve
	{"HS2_AP_SHRINK_HYST", "1", Both}, // sp176: the pattern shrinks a link's worth under the grow line
	{"HS2_LB_REC_CONFIRM", "1", Exit},
	{"HS2_LB_FLAP_PEN", "1", Exit},
	{"HS2_LB_JUDGE_DATA", "1", Both},
	{"HS2_LB_RESCUE", "1", Edge},
	{"HS2_LB_RESCUE_CALLFREE", "1", Edge},
	{"HS2_LB_CALL_LATE", "1", Edge},
	// stuck61: light users off CUT paths (Iran). HS2_LB_RESCUE_CUT_L3 is NOT
	// a default: with both keys the whole-lab P download was -5.5% (n=8,
	// p 0.53; stuck59's slotted P -15%, p 0.016), over the 5% bar
	{"HS2_LB_RESCUE_CUT", "1", Edge},
	// judge58: the edge keeps a cut the dialing side's fresh report still holds
	{"HS2_LB_EDGE_SYNC", "1", Edge},
	{"HS2_START_DIAL", "1", Exit},
	{"HS2_REFILL_WAIT_MS", "300", Edge},
	{"HS2_REFILL_WAIT_BYTES", "1", Edge},
	// lb89: the force close of trickling streams on retiring / cut-drained
	// links skips copy subs (their rate EWMA is never updated, lb_copyslow81.go)
	{"HS2_LB_SLOW_SKIP_COPY", "1", Edge},
	// up57: a single upload's stripe widens fast and early (Iran, the
	// stripe's sender); the upload gate reads the light users' queue per path
	// rel54c: on by default by the operator's decision (whole-lab u1: upload
	// +35-60 %, light p99 crowd +3.0 ms, echo +7.7 ms during one heavy
	// upload); HS2_MCONN_STRIPE_FAST=0 on Iran turns it off
	{"HS2_MCONN_STRIPE_FAST", "1", Edge},
	{"HS2_MCONN_STRIPE_FAST_EARLY_MS", "200", Edge},
	// rel54b: the fast rule only for an upload that stripes alone (u8 crowd p99)
	{"HS2_MCONN_STRIPE_FAST_SOLO", "1", Edge},
	// dr116: a single download stripes from its first 200 ms samples (80 Mbit/s
	// twice in a row) and widens by doubling, solo (lab d1 n=12/11: 455 -> 761
	// Mbit/s mean, Welch p 0.002; d8 +18 %; light p99 not worse)
	{"HS2_MCONN_STRIPE_DL_FAST", "1", Edge},
	// cs141/rel62k3: the download fast slot is a 3 s lease taken only by a
	// download's first growth, and a fast download may take one of 4 stripe
	// slots past stripeMaxConns (lab: test download among 3-10 background
	// users 427 -> 752 Mbit/s mean, n=10 paired, 9/10; background throughput
	// and light p99 unchanged; lab/cs141, lab/df142)
	{"HS2_MCONN_STRIPE_DL_FAIR", "1", Edge},
	{"HS2_MCONN_STRIPE_UP_PATHQ", "1", Edge},
	// gate74: Iran's upload gate grows on the median over the learned base
	// (10 ms cap) and cuts on the latest sample (10 ms base, 67 % of the
	// target): one upload 368 -> 483 Mbit/s mean in the production-like lab,
	// light users' p99 within the noise (CHANGELOG gate74)
	{"HS2_MCONN_STRIPE_UP_SPLIT", "1", Edge},
	// share62: an upload's shared stripe subs may take links with lite's
	// copies (uploads only; the game's lane links last) — production's
	// single uploads ran raw on one link without it
	{"HS2_MCONN_STRIPE_SHARE_COPIES", "1", Edge},
	// qctl63: the path-queue controller caps a heavy flow's queue so light
	// users are not stuck behind it (§11.16). Only kharej's (downloads) is a
	// default: rel58v (lab1, u8 n=8) measured the edge's HS2_QUEUE_CTL_UP at
	// echo p99 +10 ms (p 0.05; without it −5 ms vs rel54c), so it stays opt-in.
	{"HS2_QUEUE_CTL", "1", Exit},
	// rel69a (hl204..hl232): the edge's own upload controller (HS2_QUEUE_CTL_UP)
	// on top, both controllers cap a big queue sooner (_FAST), keep an idle cap
	// at the path's measured rate (_KEEP: seen clamp, burst onset, growth probe,
	// Iran's upload stripe gate taking the edge cap's target), and a light
	// link's collapsed BBR estimate is re-inited (HS2_LINK_CC_REFRESH, us200 on
	// the edge, cc203 on the exit). lab1 hl232 ABAB 3+3 vs hl205: crowd p50
	// 92-98 ms in every phase (B 92-377), heavy -4.0..+10.3 % (CHANGELOG rel69a)
	{"HS2_QUEUE_CTL_UP", "1", Edge},
	{"HS2_QUEUE_CTL_FAST", "1", Both},
	{"HS2_QUEUE_CTL_KEEP", "1", Both},
	// rel69e (pk239): the queue-ctl shaper (htb+cake) on the last mq child, the
	// root mq kept, links steered there only under a cap (skbedit queue_mapping;
	// TSQ kept). lab1 ABAB 4+4 vs root mode: heavy within 2 %, crowd p99 equal or
	// lower. Falls back to root mode by itself on a 1-queue NIC (the exit).
	{"HS2_QUEUE_CTL_SHAPER", "mqchild", Both},
	{"HS2_LINK_CC_REFRESH", "1", Both},
	// dl243/dl244: the exit re-sends a young stripe sub's first hole (<= 256
	// KiB, 1 per sub, 2 per conn per 10 s) on an older sub when the young
	// sub's own TCP link has not acked anything for max(300 ms, 2 x srtt).
	// lab2 transient ABBA x3 (dl244.md): see CHANGELOG rel69f
	{"HS2_DL243_RESCUE", "1", Exit},
	// dl259: the exit's download stripe gate judges the queue per path (a path
	// whose other busy links queue over the 3 ms target is cut for that
	// download; width judged on the uncut stripe paths). lab3 dr259 4+4 light
	// + 2+2 heavy (3000 crowd, unlimited bg downloads, one 3 % path): see
	// engine/mconn_dl259.go for the numbers. OFF by default since prod A/B
	// 2026-11-17 (same hour, kharej key off/on: tundown single-stream DL
	// median ~1128 vs ~474 Mbit/s: on prod every path carries other users'
	// busy links, so the per-path gate cut nearly all paths). =1 turns it on.
	{"HS2_DL259_PATHQ", "0", Exit},
	// dl79: a download's sub windows never under 4 MiB once it proves heavy,
	// dropped while its own loop shows 2 queued probes in a row (Iran, the
	// receiver of downloads); interleaved A/B on the production-like lab:
	// see engine/smux_autowin.go stripeHeavyFloorGate for the numbers
	{"HS2_STRIPE_HEAVY_FLOOR_MB", "4", Edge},
	{"HS2_STRIPE_HEAVY_FLOOR_GATE", "2", Edge},
	// bh101: a blackholed path (nothing acked while data waits) is read as
	// 100 % loss and cut, held out with 0 re-admissions while dark, and back
	// 2-8 s after the clear (dark-cooled path probed every 2 s). The dialing
	// side (the exit in reverse) decides and probes; interleaved A/B on the
	// production-like lab: lab/bh100/results100.txt, lab/bh101/results101.txt
	{"HS2_LB_SILENT", "1", Exit},
	// fl107/fl108: a path the dialing side reports cut gets no new
	// connections (0 in the steering draw, its links sorted after every other
	// link, before pressure); flap lab: dark phases 2-5 0 vs 73-111 failed
	// crowd connections, single blackhole and quiet runs unchanged
	// (lab/fl107/results107.txt)
	{"HS2_LB_CUT_NONEW", "1", Edge},
	// b331: the exit dials a spare for a recovered PATH under its share of
	// the edge's SERVING links by >= 2 (past the slot cap; 1/path/30 s,
	// 6/min); the edge sends per-path serving counts in the advert (optional
	// block, old peers unaffected) and takes the link as serving per path.
	// On since rel69p-rc1 (lab/rb331 v4-v6, rb336, rb337: gap 3.9 -> 1.7
	// links on a recovered path, no call p99 cost after placement).
	{"HS2_B331_SPAREPATH", "1", Both},
	// cp331: log a call's sub paths/links on each placement change (Iran,
	// lab/rb331 v6; log only).
	{"HS2_CALLPATH_LOG", "0", Edge},
	// b336: move a call copy sitting on a clearly lossier path (smoothed loss
	// >= 0.15 % worse for 10 s) onto the best eligible path: fresh sub first,
	// old one given up through the close handshake once the fresh one read
	// (Iran; 1/call/30 s, 20/min; lab/rb336). On since rel69p-rc3 (rb338:
	// abandon + relative damper + 30..300 s per-call backoff, no churn, with
	// the rescue-cut per-connection backoff HS2_B338_RC_GAP0_MS on).
	{"HS2_B336_CALLMOVE", "1", Edge},
	// id125/rk127: a pool shrink keeps live idle user connections on retiring
	// links (reclaimed only after 60 m idle; the old 5 m rule still applies to
	// the surplus retiring links above max_links and to cut-path drains);
	// production edge closed ~8.7k idle user conns a day; lab ec125 0 vs 34
	// EOF on 900 idle conns, rk127 interleaved 65 min pairs (lab/rk127)
	{"HS2_RETIRE_KEEP_IDLE", "1", Edge},
	// er91..er97/rel61: a raw (and, via HS2_MCONN_RESCUE_REC, records-mode)
	// light connection whose path goes silent is resent on a good path from a
	// bounded per-connection ring; er97 blackhole lab: failed connections -74 %,
	// soak99 6.7 h memory flat, 0 failed rescues; costs: kharej +~80 MB in its
	// bounded pools, crowd p99 +25 ms only inside blackhole windows
	// (lab/er97, lab/soak99)
	{"HS2_MCONN_RESCUE", "1", Both},
	// rs145/rs146: the rescue join goes only to a link with fresh progress,
	// on another Iran IP than the paths it leaves (a whole-IP outage darkens
	// all of that IP's paths); the records/raw ring budgets grow with the
	// connections keeping a ring, capped at HS2_MCONN_RESCUE_RAMPCT (4) % of
	// MemTotal. Production-topology lab (6 Iran IPs x 2 kharej IPv4 + IPv6,
	// whole-IP outages, interleaved per-key arms): JOINPICK cuts records
	// lostjoin, SCALE takes exit NAKs to ~0 for ~+100-270 MB on the exit
	// (lab/rs145, lab/rs146). JOINPICK acts only on the edge (no wire
	// change); SCALE is local memory on each side.
	{"HS2_MCONN_RESCUE_JOINPICK", "1", Edge},
	// rel64: a rescue join lost before its answer (closed, or its link in the
	// rescue watch's silent set) is replaced once on another path; with
	// JOINPICK records lostjoin ~106 -> ~19 a run, acked 0.966 -> ~0.978
	// (lab/rs146 Y vs B). The joins start on the edge only.
	{"HS2_MCONN_RESCUE_RETRY", "1", Edge},
	{"HS2_MCONN_RESCUE_SCALE", "1", Both},
	// hs112..hs132/rel63: a striped heavy stream keeps each sub's chunks until
	// the peer's smux consumed count passes them, closes a sub whose link went
	// dark (ack stall, checked against its TCP state) and resends on a live
	// sub; the receiver gives an old hole reorder room only after 1.5 s without
	// progress (hs131). The receiver opens its hole rooms only toward a peer
	// that announces KEEPSTALL (capability ext bit10, hs133), so both sides
	// need it for the dark-path protection; a mixed pair interoperates and is
	// memory-safe. Dark-path download/upload survive (lab hs126..hs132); quiet
	// light p99 / throughput not worse than off (lab/hs131, lab/hs132).
	// rel67b: HS2_MCONN_STRIPE_KEEPSTALL_IDLE (in-code default on) frees what
	// quiet connections keep (mb174).
	{"HS2_MCONN_STRIPE_KEEPSTALL", "1", Both},
	// lk162/lk165/fp185: new user connections and mconn subs are placed under
	// the edge pool's read lock (CAS-checked placement, lock-free stripeOn,
	// steer draw cached per tick); in the sc161 connection storm 98.6 % of
	// the edge's mutex wait was that write lock (engine/lm_fastpick.go)
	{"HS2_LM_FASTPICK", "1", Edge},
	// rel68b: HS2_MCONN_DARKCALL (cs193/cs194): a timer finds a call
	// connection's dark sub (no ack progress + repeated RTO + nothing received
	// for HS2_MCONN_DARKCALL_MS, in-code 1000) and moves the call's primary
	// to a live sub on another socket and path with a ring resend (Iran: a
	// records rescue when there is none). Each side acts on its own sending
	// direction; no wire change. HS2_MCONN_DARKCALL_TRACE stays off.
	{"HS2_MCONN_DARKCALL", "1", Both},
	// rel68b: HS2_MCONN_STRIPE_UP_RAW_COPIES (ul197): an upload's raw upgrade
	// takes the share-copies hint (its first stripe sub may share a link with
	// lite's copies). HS2_MCONN_STRIPE_UPGATE_HYST stays off.
	{"HS2_MCONN_STRIPE_UP_RAW_COPIES", "1", Edge},
	// rel68b: HS2_MCONN_STRIPE_DL_COPIES (dl186/dl188): downloads take the
	// SHARE_COPIES rule too, bounded by HS2_MCONN_STRIPE_DL_COPIES_MAX
	// (in-code 4).
	{"HS2_MCONN_STRIPE_DL_COPIES", "1", Edge},
	// rel68b: HS2_LB_STRIPE_STEER (st149/st150): stripe picks and senders keep
	// off paths the steering reports cut (edge: uploads; exit: the judge
	// steers downloads). In production via an env drop-in since 2026-11-14.
	{"HS2_LB_STRIPE_STEER", "1", Both},
	// rel68c: HS2_MCONN_SUB_RESEND (r68fix2..r68fix4): a small light
	// connection (at most HS2_MCONN_SUB_RESEND_SCOPE_KB, in-code 256, sent)
	// keeps its subs' unacked chunks and resends them on a fresh live sub
	// when one dies, instead of a lost-copy reset; keep age max(300 ms,
	// 3 x srtt), per-sub healing gaps, 32 MiB global cap. Each side alone.
	{"HS2_MCONN_SUB_RESEND", "1", Both},
	// rel68d: the fx212 kill switches (an210 + audit211 M2). Read in code with
	// an in-code default of on (envBoolDefault, package init), so the rows
	// document the default; "0" gives back the rel68c behaviour.
	// HS2_FX212_DARKCALL: the DARKCALL in-flight cap needs a stalled link, a
	// call with 2+ live subs never has all capped, cap fits the resend ring.
	{"HS2_FX212_DARKCALL", "1", Both},
	// HS2_FX212_DL_PRIM: a download takes a link carrying lite copy-only subs
	// only when every open user stream on it is a copy (light p99). "2" (dlp217): grade the
	// links instead (no primary, idle primaries, then at most
	// HS2_FX212_DL_PRIM_HOTK=1 sub on a link with active ones).
	// rel68e: "2" (dlp217 rank mode) is the edge default; this row is read
	// through the table (engine loadEnvKeys), so the exit and other carriers
	// keep mode 1. HS2_FX212_DL_PRIM=1 gives back rel68d, =0 turns it off.
	{"HS2_FX212_DL_PRIM", "2", Edge},
	// HS2_FX212_SUB_RESEND: SUB/CALL_RESEND light-path cost — resend only with
	// undelivered bytes, shared 1 s keep sweeper, keptMax per tick, skip
	// chunks the target already holds.
	{"HS2_FX212_SUB_RESEND", "1", Both},
	// HS2_FX212_AGE_GAP: a sub-resend age trim without TCP proof opens the
	// sub's gap (no 30 s hole stall on a dark-path death).
	{"HS2_FX212_AGE_GAP", "1", Both},
	// rel68e: the cs221..cs229 call/dark-path keys. Read in code with an
	// in-code default (envBoolDefault, package init, every carrier), so the
	// rows document the default; "0" (LITEPLACE: "1") gives back the old rule.
	// HS2_CS221_DARKCALL: a darkcall switch resends only what the target
	// lacks; a call pair never puts both copies of a chunk on one path while
	// another path has room; the rx rescue waits while a live sub reads.
	{"HS2_CS221_DARKCALL", "1", Both},
	// HS2_CS223_CALLRESEND: a given-up call sub's chunks are resent at
	// give-up on a live sub (not at the sub's death), and resends skip what
	// the target delivered; HS2_CS223_DARKRESEND: a call sub that goes dark
	// off the primary resends at once the chunks the others lack (needs
	// CALLRESEND).
	{"HS2_CS223_CALLRESEND", "1", Both},
	{"HS2_CS223_DARKRESEND", "1", Both},
	// HS2_CS224_COPYIDLE: a moving copy-only sub is not idle for the drain
	// reclaim; HS2_CS224_STALESWITCH: no ring resend off a sub an earlier
	// switch already left.
	{"HS2_CS224_COPYIDLE", "1", Both},
	{"HS2_CS224_STALESWITCH", "1", Both},
	// HS2_DC254: darkcall catches a silent call whose links look alive
	// (txDark switch, silent-rx/tx-stall counters, call copy hold).
	{"HS2_DC254", "1", Both},
	// HS2_XS261: the cross-side switch (a side that moves a call off a
	// stalled link, or whose call receive is silent, asks the peer to move
	// its primary too; sent only to a peer announcing it) + b258's cs224
	// re-chosen-sub fix.
	{"HS2_XS261", "1", Both},
	// HS2_B262_COVER: a darkcall switch trusts cs221's "the target holds
	// it" only for bytes the target provably wrote or still queues, and a
	// ring that does not reach the needed range is short, not covered.
	{"HS2_B262_COVER", "1", Both},
	// HS2_B262_REPLACE: Iran closes a call sub a darkcall switch left that
	// stays dark >= 2 s with >= 16 KB in its socket, and the call opens a
	// fresh second sub on another link (rate-limited).
	{"HS2_B262_REPLACE", "0", Edge},
	// HS2_B270_TXRES: a 2-sub call's copies stranded on a half-dead
	// non-primary sub (txDark, or its link receives nothing while the conn's
	// receive advances) are resent on the live sub (lab3 ab270 data-dark).
	{"HS2_B270_TXRES", "1", Both},
	// HS2_B278_L3STALL: a TUN interactive flow (ping, small packets) leaves
	// a link whose socket has data in flight with no ack progress (RTO
	// backoff or bytes_acked still for max(400 ms, 3 srtt)), and the dup's
	// second copy skips such a link (lab3 data-dark 5-9 s ping gaps).
	{"HS2_B278_L3STALL", "1", Both},
	// HS2_B274_REPAIR: a call conn left with one live sub re-opens its
	// second at the next manager tick when call-like within 5 s.
	{"HS2_B274_REPAIR", "1", Edge},
	// HS2_JM271_RST: the exit answers a missed plain join with an RST record
	// (rate-limited) so Iran ends the dead conn instead of re-joining it forever.
	{"HS2_JM271_RST", "1", Exit},
	// HS2_UL268_RESCUE: Iran rescues raw conns off a data-dark link (TCP_INFO:
	// data frozen while another path still acks); calls stay with darkcall.
	{"HS2_UL268_RESCUE", "1", Edge},
	// HS2_UL281_SLOT: a new fast upload (>= 80 Mbit/s, raw conn < 20 s) may
	// take one of the bounded dlfair extra stripe slots when all 8 are held.
	{"HS2_UL281_SLOT", "1", Edge},
	// HS2_CS225_CALLPATH: a call's second sub on another path (replaced by
	// HS2_CS229_MINPLACE while that is on).
	{"HS2_CS225_CALLPATH", "1", Both},
	// HS2_CS226_LITEPATH: master of the lite dark-path rules (=0: cs225);
	// HS2_CS228_LITEVISIT: darkcall visits a lite conn only with unconsumed
	// bytes on a silent or lowered link; HS2_CS228_LITEPLACE (off): the
	// RTT/loss-gated lite placement, replaced by HS2_CS229_MINPLACE: keep the
	// original pick, re-pick on another healthy path only when it shares a
	// path with the conn's live subs.
	{"HS2_CS226_LITEPATH", "1", Both},
	{"HS2_CS228_LITEVISIT", "1", Both},
	{"HS2_CS228_LITEPLACE", "0", Both},
	{"HS2_CS229_MINPLACE", "1", Both},
	// HS2_CS286_CALLAPART: a call's copy sub never shares the primary's path
	// while another path exists (a lowered/cooled one beats co-location).
	{"HS2_CS286_CALLAPART", "1", Both},
	// HS2_B296_DCSKIP: the darkcall timer skips a non-call conn proven idle
	// (subs all consumed, no write since); every 25th tick visits all conns.
	{"HS2_B296_DCSKIP", "1", Both},
	// HS2_GC296_RINGPOOL: rescue ring buffers come from a per-size-class
	// sync.Pool (4 KB-8 MB) and are returned when the ring is dropped.
	{"HS2_GC296_RINGPOOL", "1", Both},
	// HS2_B302_COPYCNT: linkCopies reads a per-link copy-sub counter instead
	// of walking every stream on the link under LinkManager.mu.
	{"HS2_B302_COPYCNT", "1", Both},
	// HS2_B307_ASYNCCLOSE: msub.close runs the smux Stream.Close (FIN behind
	// queued link writes) in a goroutine and waits at most 1 ms, so other
	// closers no longer queue in its Once (sc307 3x: Iran Once 116 s -> 1.6 s).
	// b315: ON by default (p313 fair A/B: neutral on heavy, ICMP/call p99
	// equal or better; b310's -20 % was the lab H1 softirq bias); =0 off.
	{"HS2_B307_ASYNCCLOSE", "1", Both},
	// HS2_B311_HEAVYSYNC: with the async close on, a heavy (ever-striped or
	// bulk) connection keeps the synchronous sub close (rel69m order).
	{"HS2_B311_HEAVYSYNC", "1", Both},
	// HS2_B303_DUPSTICK: an interactive flow keeps the dup's second link
	// (dupSecond + b278 pick) while it stays alive, light, not stuck, off cut
	// and off the first's path, re-picking at most every second.
	{"HS2_B303_DUPSTICK", "1", Both},
	// HS2_B308_DUPAVOID: after a lane flow's first copy leaves a stuck or
	// cut link, its second copy avoids that link's path (and the new
	// first's path) for 3 s when a third path has a usable link.
	{"HS2_B308_DUPAVOID", "1", Both},
	// HS2_B309_DUPSCAN: a second copy left on the first's path, a stuck link
	// or a cut path walks every other-path link in rank order (light, not
	// stuck, off cut, off avoided paths); it also avoids the path of a kept
	// second link it left stuck or cut for 3 s. Nothing valid: as before.
	{"HS2_B309_DUPSCAN", "1", Both},
	// HS2_B312_OPENNOLOCK: an edge's join/stripe open picks, opens and
	// writes its header without the per-connection lock; only the install
	// takes it (and refuses after end, releasing the pick).
	{"HS2_B312_OPENNOLOCK", "1", Edge},
	// HS2_B313_RJMEMO: a rescue join's placement evaluates its per-link
	// filter once per pick instead of in every scan, and skips scans no link
	// can pass (same choice, less time under LinkManager.mu).
	{"HS2_B313_RJMEMO", "1", Edge},
	// HS2_B313_RJFAST: with HS2_LM_FASTPICK, a rescue join is placed under
	// LinkManager.mu's read lock (fpPicks CAS, as lk162), falling back to the
	// write-locked path after a few tries (lab1 A/B: LM mutex -42 %/-18 %).
	{"HS2_B313_RJFAST", "1", Edge},
	// HS2_B322_EXITNOWATCH: an exit sub gets no watcher goroutine; its
	// bookkeeping runs once from the sub's close or the connection's end
	// (b332: on; st328 lab3: Kharej storm run-queue peak -16 %, sched p99
	// -16 ms, -4.6 % goroutines created; quiet call equal to the noise).
	{"HS2_B322_EXITNOWATCH", "1", Exit},
	// HS2_B327_PICKLIST: a user connection's fast pick scans a per-path
	// candidate list (best 16 links of each steering group, slack 0) instead
	// of every link; same choice class and lightest load as the full scan
	// (b334: on; st327 lab2 5 rounds: storm pick CPU -65..-75 %, open path
	// -32..-45 %, latency equal within noise). Both sides, as A/B'd.
	{"HS2_B327_PICKLIST", "1", Both},
	// HS2_B337_PATHSTALL: a waiting link whose whole path stalls (2+ of its
	// links wait, outnumbering its prompt ones: a cut path coming back) is
	// not judged stuck before stuckPathWait (20 s) (ab337: one 5-6 s down-cut
	// of 1 of 4 paths drained 17-38 links and closed their idle users at 4.5x).
	{"HS2_B337_PATHSTALL", "1", Edge},
	// HS2_B339_PATHHYST: a path found stalled whole keeps its links held for
	// stuckPathWait from that first sample even as part of it answers again
	// (ab337 X5: 23 backoff stragglers judged at 8 s once some links resumed).
	{"HS2_B339_PATHHYST", "1", Edge},
	// HS2_B339_CUTGRACE: drainTick does not reclaim idle / evac / trickling
	// conns on a cut-drained link until its path's cut has lasted 30 s (no
	// grace for a path cut again within 120 s). rel69p-rc2 ships it dark
	// (ab339c: no user-visible gain on lossy cuts with HS2_LB_EVAC off).
	{"HS2_B339_CUTGRACE", "0", Edge},
	// HS2_B338_RC_GAP0_MS: rescue-cut per-connection backoff, first drop
	// immediate, each further drop within 30 s waits GAP0 << n (max 16 s);
	// 0 = off (rb339: bounds the 148-subs-in-11-s churn loop).
	{"HS2_B338_RC_GAP0_MS", "500", Both},
	// HS2_B340_SKIPCUT: light / call / rescue placement skips rescue-cut
	// marked links while an unmarked one exists (rb340: 0 copies on marked
	// links vs 6-102 per arm; backoff holds 0).
	{"HS2_B340_SKIPCUT", "1", Both},
	// HS2_B340_CUTCLEAR: a cut path that reads clean 3 times in a row (report
	// OK bit) is lowered, not cut-marked, for the rest of its recovery hold.
	{"HS2_B340_CUTCLEAR", "1", Both},
	// HS2_B342_DCFAST: darkcall switches a call's primary that has bytes in
	// flight and no ack progress for max(3 x srtt, 300 ms) to a live copy on
	// another path with recent ack progress, without waiting for RTO
	// evidence (rb342: switch ~0.4 s instead of 1.2-2 s, no flapping, light
	// users equal or better). Each side its own sending direction; on since
	// rel69p-rc3.
	{"HS2_B342_DCFAST", "1", Both},
	// HS2_B388_PEEREXEMPT (sec353 1a): the reality front's pre-hello cap
	// (64 per client /24) exempts peer IPs (the other side's lb IPs, re-read
	// on reload, plus IPs that authenticated a link in 24 h); each such IP has
	// its own bound of 2*max_links+8 pre-hello conns (rq387: a same-/24
	// neighbour with 64 idle conns pushed our refill off reality). Listening
	// side only; no wire change.
	{"HS2_B388_PEEREXEMPT", "1", Both},
	// HS2_B375_RRCOVER: an er97 records rescue covers the exit's missing
	// range [Rk, Bi) from the retired subs' queued, in-write and kept chunks
	// below the records ring (rb372 B2 k7: a heavy call ended nocover with
	// 62.5 KB still queued/kept on Iran). Hold-not-copy since 000e2ccf; on
	// in rel69q-rc1, off again since rel69q-rc2: sk393 drop-storm opens
	// p50/p99 +30-40 % with it on (key off = rel69p-rc3), and it never
	// extended a snapshot in any lab run (ext 0). "1" enables it.
	{"HS2_B375_RRCOVER", "0", Both},
	// HS2_B375_RINGFLOOR: a records ring's cap is at least the conn's
	// in-flight bytes (largest sub queue + written-unacked) and the young
	// window's rate; 1 = calls and conns with a full lite queue
	// (HS2_B375_RINGFLOOR_QKB, 32), 2 = every records conn. Dark: rb375 saw
	// +150-380 MB RSS and ring grow/shrink churn with it on everywhere.
	{"HS2_B375_RINGFLOOR", "0", Both},
	// rel69h (st244/st245/st246): the edge paces lite raw->dual upgrades
	// through one token bucket, "auto" = max(500, 100 x GOMAXPROCS) a second
	// (burst rate/10); call upgrades and interactive flows
	// (HS2_ST246_INTERACTIVE_BPS, in-code 1000 B/s) skip it. lab3 isolated
	// 6-core storm harness, UPG only n=4 vs rel69e n=3: crowd p99 m1/m2
	// 313/317 vs 451/447 ms, storm open p99 ~980 vs 1937/1503 ms, stall
	// seconds 3.0 vs 6.3, edge RSS equal. A number sets the rate, 0 = off.
	{"HS2_ST244_UPG_RATE", "auto", Edge},
}

var byName = func() map[string]Key {
	m := make(map[string]Key, len(Table))
	for _, k := range Table {
		m[k.Name] = k
	}
	return m
}()

// active: 0 dormant, 1 edge, 2 exit.
var active atomic.Int32

// Legacy reports HS2_DEFAULTS=legacy (or 0 / off): the table stays dormant.
func Legacy() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HS2_DEFAULTS"))) {
	case "legacy", "0", "off", "false", "no":
		return true
	}
	return false
}

// Activate turns the table on for this side (edge: the iran server). It
// returns false (and changes nothing) under HS2_DEFAULTS=legacy.
func Activate(edge bool) bool {
	if Legacy() {
		active.Store(0)
		return false
	}
	if edge {
		active.Store(1)
	} else {
		active.Store(2)
	}
	return true
}

// Deactivate makes the table dormant again (tests).
func Deactivate() { active.Store(0) }

// Default is k's default for the active side ("" and false: none).
func Default(k string) (string, bool) {
	a := active.Load()
	if a == 0 {
		return "", false
	}
	d, ok := byName[k]
	if !ok || d.Side == Edge && a != 1 || d.Side == Exit && a != 2 {
		return "", false
	}
	return d.Value, true
}

// Get is os.Getenv with the table: an explicitly set key wins (even when
// empty), an unset one returns its default (or "").
func Get(k string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	d, _ := Default(k)
	return d
}

// Applied lists "KEY=value" for the defaults in force (keys left unset),
// sorted, for the start-up log.
func Applied() []string {
	var out []string
	for _, k := range Table {
		if _, set := os.LookupEnv(k.Name); set {
			continue
		}
		if v, ok := Default(k.Name); ok {
			out = append(out, k.Name+"="+v)
		}
	}
	sort.Strings(out)
	return out
}
