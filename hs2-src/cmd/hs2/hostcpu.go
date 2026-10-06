package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// hostMeter samples the whole server between status writes. cpuMeter says how
// much of the CPU hs2 itself uses; this says whether the server has any left —
// a 2-core box shared with another tunnel can be saturated while hs2 alone
// shows 60%, and then every carrier sends late however good the path is:
//
//   - CPU busy across all cores (/proc/stat), and the softirq and steal shares
//     (packet processing in the kernel; time the hypervisor gave elsewhere);
//   - CPU pressure (/proc/pressure/cpu "some"): the share of the time some task
//     waited for a core — the queueing a busy % cannot show;
//   - IP packets the kernel discarded on output (/proc/net/snmp OutDiscards),
//     since hs2 started, for the whole server: a full interface queue, or a
//     packet a firewall refused. On an icmp listener that includes the
//     kernel's own echo reply to every tunnel packet received, which the echo
//     guard drops on purpose — so it is shown, never alarmed on. The
//     tunnel's own queue drops over icmp are counted exactly elsewhere
//     (send_refused: the send-only socket has IP_RECVERR).
//
// It logs once when the server stays saturated, and once when it is back.
type hostMeter struct {
	proc string // "/proc" (tests: a fake tree)
	logf func(string, ...any)

	last, first cpuTimes
	haveLast    bool
	hot, cool   int // samples in a row past the saturation / the clear marks
	saturated   bool

	discLast, discTotal uint64
	haveDisc            bool
}

// hostSample is one hostMeter reading. Shares are % of ALL cores (100 = every
// core busy); PSI is nil when the kernel has no pressure file.
type hostSample struct {
	BusyPct, SoftirqPct, StealPct float64
	Cores                         int
	PSI10, PSI60                  *float64
	OutDiscards                   uint64 // since the first sample
	Saturated                     bool
	ok                            bool // BusyPct etc. cover an interval
}

// hostSatBusy / hostSatPSI: the server counts as saturated when its cores are
// this busy, or tasks waited for a core this share of the last 10 s, for
// hostSatRuns samples in a row (statusInterval apart). It is clear again below
// hostClearBusy and hostClearPSI for hostClearRuns samples in a row, so a load
// around the marks does not flap the log.
const (
	hostSatBusy   = 90.0
	hostSatPSI    = 40.0
	hostSatRuns   = 3
	hostClearBusy = 75.0
	hostClearPSI  = 20.0
	hostClearRuns = 5
)

// cpuTimes is the aggregate "cpu" line of /proc/stat (USER_HZ ticks), and the
// number of per-core lines.
type cpuTimes struct {
	user, nice, sys, idle, iowait, irq, softirq, steal uint64
	cores                                              int
}

func (c cpuTimes) total() uint64 {
	return c.user + c.nice + c.sys + c.idle + c.iowait + c.irq + c.softirq + c.steal
}

func readProcStat(proc string) (cpuTimes, bool) {
	b, err := os.ReadFile(filepath.Join(proc, "stat"))
	if err != nil {
		return cpuTimes{}, false
	}
	var c cpuTimes
	ok := false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		if f[0] != "cpu" {
			c.cores++
			continue
		}
		if len(f) < 9 {
			return cpuTimes{}, false
		}
		v := make([]uint64, 8)
		for i := range v {
			x, err := strconv.ParseUint(f[i+1], 10, 64)
			if err != nil {
				return cpuTimes{}, false
			}
			v[i] = x
		}
		c.user, c.nice, c.sys, c.idle, c.iowait, c.irq, c.softirq, c.steal = v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]
		ok = true
	}
	return c, ok
}

// hostShares: busy, softirq and steal as % of all cores between a and b (ok
// false: no time passed, or the counters went back).
func hostShares(a, b cpuTimes) (busy, softirq, steal float64, ok bool) {
	if b.total() <= a.total() || b.idle+b.iowait < a.idle+a.iowait || b.softirq < a.softirq || b.steal < a.steal {
		return 0, 0, 0, false
	}
	dt := float64(b.total() - a.total())
	idle := float64((b.idle + b.iowait) - (a.idle + a.iowait))
	busy = 100 * (dt - idle) / dt
	if busy < 0 {
		busy = 0
	}
	return busy, 100 * float64(b.softirq-a.softirq) / dt, 100 * float64(b.steal-a.steal) / dt, true
}

// readPSI returns the "some" avg10 and avg60 of /proc/pressure/cpu; ok false
// when the kernel has no PSI (older than 4.20, or booted with psi=0).
func readPSI(proc string) (avg10, avg60 float64, ok bool) {
	b, err := os.ReadFile(filepath.Join(proc, "pressure", "cpu"))
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != "some" {
			continue
		}
		n := 0
		for _, kv := range f[1:] {
			k, v, _ := strings.Cut(kv, "=")
			x, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			switch k {
			case "avg10":
				avg10, n = x, n+1
			case "avg60":
				avg60, n = x, n+1
			}
		}
		return avg10, avg60, n == 2
	}
	return 0, 0, false
}

// readOutDiscards returns Ip OutDiscards from /proc/net/snmp (the header line
// names the columns; the value line follows it).
func readOutDiscards(proc string) (uint64, bool) {
	b, err := os.ReadFile(filepath.Join(proc, "net", "snmp"))
	if err != nil {
		return 0, false
	}
	var head []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "Ip:" {
			continue
		}
		if head == nil {
			head = f
			continue
		}
		for i := 1; i < len(f) && i < len(head); i++ {
			if head[i] == "OutDiscards" {
				v, err := strconv.ParseUint(f[i], 10, 64)
				return v, err == nil
			}
		}
		return 0, false
	}
	return 0, false
}

// sample reads the server's state since the last call; hsPct is hs2's own use
// (cpuMeter, % of one core) for the saturation line.
func (m *hostMeter) sample(hsPct float64) hostSample {
	var s hostSample
	if c, ok := readProcStat(m.proc); ok {
		s.Cores = c.cores
		if m.haveLast {
			s.BusyPct, s.SoftirqPct, s.StealPct, s.ok = hostShares(m.last, c)
		}
		m.last, m.haveLast = c, true
	}
	if a10, a60, ok := readPSI(m.proc); ok {
		s.PSI10, s.PSI60 = &a10, &a60
	}
	s.BusyPct, s.SoftirqPct, s.StealPct = round1(s.BusyPct), round1(s.SoftirqPct), round1(s.StealPct)
	if d, ok := readOutDiscards(m.proc); ok {
		switch {
		case !m.haveDisc:
			m.haveDisc = true
		case d >= m.discLast:
			m.discTotal += d - m.discLast
		} // d < discLast: the counter went back (it never should); count from there
		m.discLast = d
		s.OutDiscards = m.discTotal
	}
	m.judge(&s, hsPct)
	return s
}

func (m *hostMeter) judge(s *hostSample, hsPct float64) {
	psi10 := -1.0
	if s.PSI10 != nil {
		psi10 = *s.PSI10
	}
	if !s.ok && s.PSI10 == nil {
		s.Saturated = m.saturated // nothing measured: no change either way
		return
	}
	hot := (s.ok && s.BusyPct >= hostSatBusy) || psi10 >= hostSatPSI
	cool := (!s.ok || s.BusyPct < hostClearBusy) && psi10 < hostClearPSI
	switch {
	case hot:
		m.cool = 0
		if m.hot++; m.hot >= hostSatRuns && !m.saturated {
			m.saturated = true
			m.logf("cpu: the server is saturated — %s; hs2 itself uses %.0f%% of one core. Every carrier now sends late however good the path is: other programs on this server (another tunnel?) or a bigger VPS are the fix, not the tunnel's settings", hostCPUText(*s), hsPct)
		}
	case cool:
		m.hot = 0
		if m.cool++; m.cool >= hostClearRuns && m.saturated {
			m.saturated = false
			m.logf("cpu: the server has room again — %s", hostCPUText(*s))
		}
	default:
		m.hot, m.cool = 0, 0 // between the marks: neither builds up nor clears
	}
	s.Saturated = m.saturated
}

// hostCPUText renders a host sample, e.g. "92% busy across 2 core(s)
// (softirq 11%, steal 3%), tasks waited for a core 58% of the last 10 s".
func hostCPUText(s hostSample) string {
	var b strings.Builder
	if s.ok || s.BusyPct > 0 {
		fmt.Fprintf(&b, "%.0f%% busy across %d core(s)", s.BusyPct, s.Cores)
		var extra []string
		if s.SoftirqPct >= 1 {
			extra = append(extra, fmt.Sprintf("softirq %.0f%%", s.SoftirqPct))
		}
		if s.StealPct >= 1 {
			extra = append(extra, fmt.Sprintf("steal %.0f%%", s.StealPct))
		}
		if len(extra) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(extra, ", "))
		}
	}
	if s.PSI10 != nil {
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "tasks waited for a core %.0f%% of the last 10 s", *s.PSI10)
	}
	return b.String()
}
