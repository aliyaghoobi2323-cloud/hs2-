// Package censor is the censor351 level-1 detector (step-3 F2): it tells
// censorship apart from ordinary network trouble and only LOGS what it thinks.
//
// Hard boundary (proposals/step3-architecture.md §2.2, censor351 §3.1):
//   - Detect and log only. Nothing in hs2 reads this package's state: every
//     hook (Note*) returns nothing, and the only outputs are log lines and the
//     read-only status block (StatusJSON, rendered by `hs2 status --censor`).
//     isolation_test.go enforces this over the whole module.
//   - Hooks never block (select with default; a full ring drops and counts),
//     take no lock, and allocate nothing when the key is off.
//   - A panic in the evaluator is recovered and turns the detector off.
//   - HS2_CENSOR_DETECT (alias HS2_CENSOR), default 1 on the Iran side and
//     0 on the Kharej side (rel69r-rc6, LevelFor): 0 off, 1 level 1
//     (hooks + evaluator + lines + status). Level 2 (the raw-socket RST/SYN
//     sampler, M8) is not built: it waits for owner decision D4; 2 runs as 1.
//   - HS2_CENSOR_RECORD=<file> (lab only, default unset): with the detector
//     on, the evaluator appends its event stream to <file> for offline
//     replay (record.go). It is read only when the detector is on.
package censor

import (
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Side is which end of the tunnel this process is.
type Side uint8

const (
	SideKharej Side = iota // dials (reverse mode): the remote IP is the Iran IP
	SideIran               // listens: the local IP is the Iran IP
)

// Front is the link's front.
type Front uint8

const (
	FrontUnknown Front = iota
	FrontTLS
	FrontReality
)

// DialStage is where a link dial ended (M1).
type DialStage uint8

const (
	DialOK DialStage = iota
	DialConnTimeout
	DialConnRefused
	DialConnUnreach
	DialConnOther
	DialHelloTimeout // TCP up, no ServerHello before the deadline
	DialHelloReset   // TCP up, RST during the handshake
	DialHelloEOF     // TCP up, closed during the handshake
	DialCert         // the reality target / a wrong certificate answered
	DialAuth         // handshake done, proof failed
	nDialStages
)

// AcceptOutcome is how a listener's TLS handshake ended (M2).
type AcceptOutcome uint8

const (
	AcceptOK       AcceptOutcome = iota
	AcceptNoHello                // deadline passed during the handshake
	AcceptReset                  // RST during the handshake
	AcceptEOF                    // closed during the handshake
	AcceptNotTLS                 // a probe / plain HTTP
	AcceptAuthFail               // TLS ok, not our auth record (forwarded to the backend)
	nAcceptOutcomes
)

// DeathClass is how a link ended (M3).
type DeathClass uint8

const (
	DeathEOF     DeathClass = iota // clean close by the other end (FIN / close_notify)
	DeathRST                       // reset
	DeathTimeout                   // stalled path, user timeout
	DeathLocal                     // our own close: retire, drain, spare, peerOld (net.ErrClosed)
	DeathAbort                     // ECONNABORTED: killed on this host
	DeathUnreach                   // ICMP unreachable
	DeathOther
	nDeathClasses
)

// Death is one link's death record. TCP fields are valid when HaveTCP.
type Death struct {
	Local, Remote netip.Addr
	Front         Front
	Class         DeathClass
	LifeMs        uint32
	Bytes         uint64 // acked + received
	HaveTCP       bool
	LastSentMs    uint32 // ms since our last data send, at death
	LastRecvMs    uint32 // ms since the last data received, at death
	MinRTTUs      uint32
}

// PathSample is one LB reading of one path (M7 subset), copied by the LB.
type PathSample struct {
	Local, Remote netip.Addr
	Loss          float64 // resent share 0..1
	QueueMs       float64
	Silent        bool
	PeerLoss      float64 // the other direction's loss; < 0 unknown (M7/O7)
	RateBps       float64 // acked bytes/s of the path; 0 unknown
	// M7 ceiling inputs: links that moved >= 64 KB this reading, and the
	// share of their wall time spent network-limited (kernel chrono busy
	// minus rwnd- and sndbuf-limited time); < 0 unknown (no busy sender).
	BusyLinks   uint16
	PathLimited float64
}

type evKind uint8

const (
	evDial evKind = iota + 1
	evAccept
	evDeath
	evPath
	evUsers
)

// event is the fixed-size record the hooks put in the ring.
type event struct {
	kind   evKind
	stage  uint8
	front  Front
	local  netip.Addr
	remote netip.Addr
	at     int64 // unix ns
	d      Death
	p      PathSample
	n      uint64
}

// ringCap bounds the event ring (≈ 256 KB). A full ring drops and counts.
const ringCap = 1024

var (
	active   atomic.Bool
	disabled atomic.Bool // set by a recovered evaluator panic; never cleared
	dropped  atomic.Uint64
	ring     chan event
	nowNs    = func() int64 { return time.Now().UnixNano() }
)

// Active reports whether the detector is on (HS2_CENSOR_DETECT ≥ 1 and not
// self-disabled). Callers may use it only to skip collecting a hook's
// arguments; it is configuration, not detector state.
func Active() bool { return active.Load() }

func send(e event) {
	select {
	case ring <- e:
	default:
		dropped.Add(1)
	}
}

// NoteDial records a link dial's outcome (Kharej side, M1 + M9).
func NoteDial(local, remote netip.Addr, front Front, stage DialStage) {
	if !active.Load() {
		return
	}
	send(event{kind: evDial, stage: uint8(stage), front: front, local: local.Unmap(), remote: remote.Unmap(), at: nowNs()})
}

// NoteAccept records a listener handshake outcome (Iran side, M2).
func NoteAccept(local, remote netip.Addr, out AcceptOutcome) {
	if !active.Load() {
		return
	}
	send(event{kind: evAccept, stage: uint8(out), local: local.Unmap(), remote: remote.Unmap(), at: nowNs()})
}

// NoteDeath records a link death (M3, M4, M5).
func NoteDeath(d Death) {
	if !active.Load() {
		return
	}
	d.Local, d.Remote = d.Local.Unmap(), d.Remote.Unmap()
	send(event{kind: evDeath, local: d.Local, remote: d.Remote, front: d.Front, d: d, at: nowNs()})
}

// NotePath records one path reading of the LB (M7, M11).
func NotePath(p PathSample) {
	if !active.Load() {
		return
	}
	p.Local, p.Remote = p.Local.Unmap(), p.Remote.Unmap()
	send(event{kind: evPath, local: p.Local, remote: p.Remote, p: p, at: nowNs()})
}

// NoteUsers records the cumulative count of user connections opened at this
// end (the domestic witness M6 on the Iran side).
func NoteUsers(total uint64) {
	if !active.Load() {
		return
	}
	send(event{kind: evUsers, n: total, at: nowNs()})
}

// Level reads HS2_CENSOR_DETECT (alias HS2_CENSOR) for the Kharej side's
// default (off). 2 is treated as 1 (no sampler in this build).
func Level() int { return LevelFor(SideKharej) }

// LevelFor is the level for side: an explicit HS2_CENSOR_DETECT (alias
// HS2_CENSOR) wins; unset (or empty), the side's default applies:
// rel69r-rc6 runs the Iran side on (LOGONLY data collection) and keeps the
// Kharej side off (its verdicts are not accurate enough yet). With
// HS2_DEFAULTS=legacy both sides default off. 2 is treated as 1.
func LevelFor(side Side) int {
	v := strings.TrimSpace(os.Getenv("HS2_CENSOR_DETECT"))
	if v == "" {
		v = strings.TrimSpace(os.Getenv("HS2_CENSOR"))
	}
	if v == "" {
		if side == SideIran && !legacyDefaults() {
			return 1
		}
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	if n > 1 {
		n = 1
	}
	return n
}

// legacyDefaults mirrors envdef.Legacy (HS2_DEFAULTS=legacy: production
// defaults off) without importing it.
func legacyDefaults() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HS2_DEFAULTS"))) {
	case "legacy", "0", "off", "false", "no":
		return true
	}
	return false
}

// Init starts the detector when the key is on. With the key off it does
// nothing: no goroutine, no ring. Safe to call once per process.
func Init(side Side, logf func(string, ...any)) {
	initOnce.Do(func() { initLocked(side, logf) })
}

var initOnce sync.Once

func initLocked(side Side, logf func(string, ...any)) {
	if LevelFor(side) == 0 || disabled.Load() || active.Load() {
		return
	}
	ring = make(chan event, ringCap)
	ev := newEvaluator(side, logf, time.Now())
	ev.ispMap = parseISPMap(os.Getenv("HS2_CENSOR_ISPMAP"))
	ev.debug = os.Getenv("HS2_CENSOR_DEBUG") == "1"
	if p := strings.TrimSpace(os.Getenv("HS2_CENSOR_RECORD")); p != "" {
		ev.rec = openRecorder(p, side, ev.debug, ev.start, logf) // lab only (record.go)
	}
	setStatus(ev)
	active.Store(true)
	go ev.run()
	logf("censor351: v1 detector on (level 1, side %s): log only, no reaction", sideName(side))
}

func sideName(s Side) string {
	if s == SideIran {
		return "iran"
	}
	return "kharej"
}

func parseISPMap(s string) map[netip.Addr]string {
	m := map[netip.Addr]string{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		if a, err := netip.ParseAddr(strings.TrimSpace(k)); err == nil {
			m[a.Unmap()] = strings.TrimSpace(v)
		}
	}
	return m
}

// AddrOf turns a net.Addr into a netip.Addr without allocating.
func AddrOf(a net.Addr) netip.Addr {
	switch t := a.(type) {
	case *net.TCPAddr:
		ip, _ := netip.AddrFromSlice(t.IP)
		return ip.Unmap()
	case *net.UDPAddr:
		ip, _ := netip.AddrFromSlice(t.IP)
		return ip.Unmap()
	}
	return netip.Addr{}
}

// ParseAddr parses an IP literal ("" or junk: the zero Addr).
func ParseAddr(s string) netip.Addr {
	a, _ := netip.ParseAddr(s)
	return a.Unmap()
}
