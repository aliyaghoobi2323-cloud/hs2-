package censor

import (
	"bufio"
	"encoding/binary"
	"io"
	"net/netip"
	"os"
	"sort"
	"testing"
	"time"
)

// b414: churn seen from Kharej. The Iran end aborts 4 of its long-lived data
// links on ipA every tick; every redial succeeds. ownReports: the Iran end
// reports those aborts (a newer Iran), after the resets (the report rides
// the next control ping) or before them.
func kharejChurn(t *testing.T, report int) *sim {
	s := newSim(t, SideKharej)
	s.quiet(3)
	for i := 0; i < 24; i++ {
		s.healthy(ipA)
		s.healthy(ipB)
		if report < 0 {
			s.ev(event{kind: evPeerAbort, remote: ipA, n: 4})
		}
		for k := 0; k < 4; k++ {
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: uint32(100000 + 9000*k + 500*i), Bytes: 300 << 20,
				HaveTCP: true, LastSentMs: 3, MinRTTUs: 44000})
			s.dial(ipA, ipK1, DialOK)
		}
		if report > 0 {
			s.now = s.now.Add(2 * time.Second)
			s.ev(event{kind: evPeerAbort, remote: ipA, n: 4})
		}
		s.tick()
	}
	s.quiet(30)
	return s
}

// The resets of the Iran end's own aborts are no evidence once it reports
// them, whichever arrives first.
func TestPeerAbortChurnNotCensorship(t *testing.T) {
	for _, r := range []int{1, -1} {
		s := kharejChurn(t, r)
		s.noCensorship(ipA, confLow)
	}
}

// An older Iran never reports: the Kharej detector reads the same stream as
// before (the control: no report, the resets still count).
func TestPeerAbortOldPeerUnchanged(t *testing.T) {
	s := kharejChurn(t, 0)
	n := 0
	for _, l := range s.lines {
		if field(l, "peerab") != "" {
			t.Fatalf("peerab without a report: %s", l)
		}
		n++
	}
	if n == 0 {
		t.Fatalf("control churn logged nothing")
	}
}

// A report claims only as many resets as it names: injected resets beyond
// the Iran end's own aborts still count (rst-inject stays rst-inject).
func TestPeerAbortClaimsOnlyItsOwn(t *testing.T) {
	s := newSim(t, SideKharej)
	s.quiet(3)
	restartBurstKharejWithReport(s)
	if len(s.closes(ipA)) == 0 {
		t.Fatalf("injection with a small report logged nothing: %v", s.lines)
	}
}

func restartBurstKharejWithReport(s *sim) {
	s.ev(event{kind: evPeerAbort, remote: ipA, n: 1})
	for i := 0; i < 12; i++ {
		s.healthy(ipB)
		for k := 0; k < 8; k++ {
			s.death(ipA, ipK1, Death{Class: DeathRST, LifeMs: 300, Bytes: 600, HaveTCP: true, LastSentMs: 1, MinRTTUs: 44000})
			s.dial(ipA, ipK1, DialOK)
		}
		s.tick()
	}
	s.quiet(30)
}

// mergePeerReports (lab replay only) emulates a newer Iran on a recording
// made without the signal: every Iran-side own abort (DeathAbort) in irPath
// becomes a report to the Kharej stream khPath lag later (the next control
// ping), merged in time order. Returns the merged file.
func mergePeerReports(t *testing.T, irPath, khPath string, lag time.Duration) string {
	read := func(p string) [][recSize]byte {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		br := bufio.NewReaderSize(f, 1<<20)
		var out [][recSize]byte
		for {
			var b [recSize]byte
			if _, err := io.ReadFull(br, b[:]); err != nil {
				return out
			}
			out = append(out, b)
		}
	}
	var reps [][recSize]byte
	rep := func(ip netip.Addr, n uint64, at int64) {
		var o [recSize]byte
		o[0] = uint8(evPeerAbort)
		o[6] = putAddr(o[16:32], netip.Addr{}) | putAddr(o[32:48], ip)<<2
		binary.LittleEndian.PutUint64(o[8:], uint64(at))
		binary.LittleEndian.PutUint64(o[48:], n)
		reps = append(reps, o)
	}
	// b414 v2: a newer Iran reports from its first ping, so the Kharej
	// knows it reports right after each Kharej start (generation header).
	ips := map[netip.Addr]bool{}
	for _, b := range read(irPath) {
		if b[0] == uint8(evDeath) && DeathClass(b[3]) == DeathAbort {
			ips[decode(&b).local] = true
		}
	}
	for _, b := range read(khPath) {
		if b[0] == recHeader {
			for ip := range ips {
				rep(ip, 0, int64(binary.LittleEndian.Uint64(b[8:]))+int64(lag))
			}
		}
	}
	for _, b := range read(irPath) {
		if b[0] != uint8(evDeath) || DeathClass(b[3]) != DeathAbort {
			continue
		}
		ev := decode(&b)
		rep(ev.local, 1, ev.at+int64(lag))
	}
	sort.SliceStable(reps, func(i, j int) bool {
		return binary.LittleEndian.Uint64(reps[i][8:]) < binary.LittleEndian.Uint64(reps[j][8:])
	})
	out := khPath + ".peer"
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	j := 0
	for _, b := range read(khPath) {
		at := binary.LittleEndian.Uint64(b[8:])
		if b[0] != recHeader {
			for j < len(reps) && binary.LittleEndian.Uint64(reps[j][8:]) <= at {
				w.Write(reps[j][:])
				j++
			}
		}
		w.Write(b[:])
	}
	w.Flush()
	f.Close()
	t.Logf("%s: %d peer reports merged (lag %v)", out, j, lag)
	return out
}

var _ = netip.Addr{}
