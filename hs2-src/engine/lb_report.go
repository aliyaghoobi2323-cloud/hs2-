package engine

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"time"
)

// The dialing side's path report, carried after the advert answer.
//
// In reverse the exit (kharej) dials and alone knows what the balancer does —
// the mode, each path's effective share, target, quality factor and the loss
// its own TCP_INFO reads (the sender's truth for the download) — while the
// Iran edge shows the operator only what it receives. The exit therefore
// appends a report to its kindLBAdvert answer:
//
//	exit -> edge  [advert][report]
//	report: [0xA7][ver=1][flags u8][n u8] n × path [nv u8] nv × ([len u8][text])
//	path:   [iplen][local ip][iplen][remote ip][eff ‰ u16][target u16]
//	        [links u16][factor ×100 u8][loss 0.01% u16][raw 0.01% u16]
//	        [pflags u8][len u8][reason]
//	flags:  1 = mode auto, 2 = fast auto (HS2_LB_AUTO2)
//	pflags: 1 = cut; raw 0xFFFF = no reading this tick
//
// An older edge reads exactly the advert and closes the stream: the bytes
// after it are dropped unread. An older exit closes the stream right after its
// advert: the edge's read of the report ends at EOF and nothing changes. So no
// capability bit is needed. Iplen 0 is the default route ("*").

const (
	lbReportMagic = 0xA7
	lbReportVer   = 1
	lbReportMax   = 64 // paths
	lbReasonMax   = 80
)

// LBPeerPath is one of the dialing side's paths as it reported it.
type LBPeerPath struct {
	Local   string  `json:"local"`  // the dialing side's IP ("" = default route)
	Remote  string  `json:"remote"` // the listening side's IP
	EffPct  float64 `json:"eff_pct"`
	Target  int     `json:"target"`
	Links   int     `json:"links"`
	Factor  float64 `json:"factor"`
	LossPct float64 `json:"loss_pct"`          // the sender's smoothed loss, %
	RawPct  float64 `json:"raw_pct,omitempty"` // its latest reading (-1 none)
	Cut     bool    `json:"cut,omitempty"`
	Reason  string  `json:"reason,omitempty"`
	raw     bool    // RawPct is a reading
}

// LBReport is the dialing side's report.
type LBReport struct {
	Auto     bool         `json:"auto"`
	Auto2    bool         `json:"auto2"`
	Paths    []LBPeerPath `json:"paths"`
	Verdicts []string     `json:"verdicts,omitempty"`
	At       time.Time    `json:"at"`
}

// report builds the dialing side's report (the exit's kindLBAdvert answer).
func (b *LB) report() LBReport {
	st := b.Stats()
	b.mu.Lock()
	r := LBReport{Auto: b.auto, Auto2: b.auto2On()}
	for j, p := range b.paths {
		if j >= len(st) || j >= lbReportMax {
			break
		}
		s := st[j]
		pp := LBPeerPath{Local: p.Local, Remote: p.Remote, EffPct: s.EffPct, Target: s.Target, Links: s.Links,
			Factor: b.mult[j], LossPct: 100 * b.qual[j].loss, Reason: s.AutoReason}
		if !b.auto {
			pp.EffPct = s.SharePct
		}
		if b.judge != nil {
			if q := b.judge.p[p.Name()]; q != nil {
				pp.Cut = q.cut
				if q.lastRead.Equal(b.judgeAt) {
					pp.RawPct, pp.raw = 100*q.raw, true
				}
			}
		}
		r.Paths = append(r.Paths, pp)
	}
	if b.judge != nil {
		r.Verdicts = b.judge.verdictList()
	}
	b.mu.Unlock()
	return r
}

func encodeReport(r LBReport) []byte {
	fl := byte(0)
	if r.Auto {
		fl |= 1
	}
	if r.Auto2 {
		fl |= 2
	}
	n := min(len(r.Paths), lbReportMax)
	out := []byte{lbReportMagic, lbReportVer, fl, byte(n)}
	ipb := func(s string) []byte {
		ip := net.ParseIP(s)
		if ip == nil {
			return []byte{0}
		}
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		return append([]byte{byte(len(ip))}, ip...)
	}
	u16 := func(f float64) []byte {
		v := uint16(math.Max(0, math.Min(65534, math.Round(f))))
		return []byte{byte(v >> 8), byte(v)}
	}
	str := func(s string) []byte {
		if len(s) > lbReasonMax {
			s = s[:lbReasonMax]
		}
		return append([]byte{byte(len(s))}, s...)
	}
	for _, p := range r.Paths[:n] {
		out = append(out, ipb(p.Local)...)
		out = append(out, ipb(p.Remote)...)
		out = append(out, u16(p.EffPct*10)...)
		out = append(out, u16(float64(p.Target))...)
		out = append(out, u16(float64(p.Links))...)
		out = append(out, byte(math.Max(0, math.Min(255, math.Round(p.Factor*100)))))
		out = append(out, u16(p.LossPct*100)...)
		if p.raw {
			out = append(out, u16(p.RawPct*100)...)
		} else {
			out = append(out, 0xFF, 0xFF)
		}
		pf := byte(0)
		if p.Cut {
			pf |= 1
		}
		out = append(out, pf)
		out = append(out, str(p.Reason)...)
	}
	nv := min(len(r.Verdicts), 16)
	out = append(out, byte(nv))
	for _, v := range r.Verdicts[:nv] {
		out = append(out, str(v)...)
	}
	return out
}

// readReport reads a report; ok false on EOF (an older exit) or garbage.
func readReport(r io.Reader) (LBReport, bool) {
	br := bufio.NewReader(r)
	var rep LBReport
	b := func() (byte, bool) {
		c, err := br.ReadByte()
		return c, err == nil
	}
	h := make([]byte, 4)
	if _, err := io.ReadFull(br, h); err != nil || h[0] != lbReportMagic || h[1] < 1 || int(h[3]) > lbReportMax {
		return rep, false
	}
	rep.Auto, rep.Auto2 = h[2]&1 != 0, h[2]&2 != 0
	ip := func() (string, bool) {
		n, ok := b()
		if !ok || n != 0 && n != 4 && n != 16 {
			return "", false
		}
		if n == 0 {
			return "", true
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			return "", false
		}
		return net.IP(buf).String(), true
	}
	u16 := func() (int, bool) {
		var x [2]byte
		if _, err := io.ReadFull(br, x[:]); err != nil {
			return 0, false
		}
		return int(x[0])<<8 | int(x[1]), true
	}
	str := func() (string, bool) {
		n, ok := b()
		if !ok || int(n) > lbReasonMax {
			return "", false
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			return "", false
		}
		return string(buf), true
	}
	for i := 0; i < int(h[3]); i++ {
		var p LBPeerPath
		var ok bool
		var v int
		if p.Local, ok = ip(); !ok {
			return rep, false
		}
		if p.Remote, ok = ip(); !ok || p.Remote == "" {
			return rep, false
		}
		if v, ok = u16(); !ok {
			return rep, false
		}
		p.EffPct = float64(v) / 10
		if p.Target, ok = u16(); !ok {
			return rep, false
		}
		if p.Links, ok = u16(); !ok {
			return rep, false
		}
		f, ok := b()
		if !ok {
			return rep, false
		}
		p.Factor = float64(f) / 100
		if v, ok = u16(); !ok {
			return rep, false
		}
		p.LossPct = float64(v) / 100
		if v, ok = u16(); !ok {
			return rep, false
		}
		p.RawPct = -1
		if v != 0xFFFF {
			p.RawPct, p.raw = float64(v)/100, true
		}
		pf, ok := b()
		if !ok {
			return rep, false
		}
		p.Cut = pf&1 != 0
		if p.Reason, ok = str(); !ok {
			return rep, false
		}
		rep.Paths = append(rep.Paths, p)
	}
	nv, ok := b()
	if !ok {
		return rep, true // a report without verdicts is still a report
	}
	for i := 0; i < int(nv) && i < 16; i++ {
		s, ok := str()
		if !ok {
			break
		}
		rep.Verdicts = append(rep.Verdicts, s)
	}
	return rep, true
}

// SetReport makes the exit append the dialing side's report to its advert
// answers (reverse, lb on).
func (a *LBAdvert) SetReport(f func() []byte) {
	a.mu.Lock()
	a.report = f
	a.mu.Unlock()
}

// PeerReport is the dialing side's latest report (edge; ok false: none).
func (a *LBAdvert) PeerReport() (LBReport, bool) {
	if a == nil {
		return LBReport{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rep == nil {
		return LBReport{}, false
	}
	return *a.rep, true
}

// gotReport stores a report the edge read.
func (a *LBAdvert) gotReport(r LBReport) {
	r.At = time.Now()
	a.mu.Lock()
	a.rep = &r
	f := a.onReport
	a.mu.Unlock()
	if f != nil {
		f(r)
	}
}

// OnReport sets a func called with every report the edge reads.
func (a *LBAdvert) OnReport(f func(LBReport)) {
	a.mu.Lock()
	a.onReport = f
	a.mu.Unlock()
}

// reasonRank orders reasons for a row that merges several paths: the worst
// one is shown.
func reasonRank(s string) int {
	switch {
	case strings.HasPrefix(s, "cut"):
		return 4
	case strings.HasPrefix(s, "lower"):
		return 3
	case strings.HasPrefix(s, "recovering"):
		return 2
	case strings.HasPrefix(s, "all paths"):
		return 1
	}
	return 0
}

type rowAgg struct {
	eff          float64
	tgt          int
	fsum         float64
	n            int
	loss, lw     float64
	lossN        int
	reason       string
	bad, badOf   int
	haveLoss     bool
	haveSteerRow bool
}

func (a *rowAgg) addReason(r string) {
	a.badOf++
	if reasonRank(r) >= 3 {
		a.bad++
	}
	if a.reason == "" || reasonRank(r) > reasonRank(a.reason) {
		a.reason = r
	}
}

func (a *rowAgg) reasonText() string {
	if a.bad > 0 && a.badOf > 1 && a.bad < a.badOf {
		return fmt.Sprintf("%s (%d of %d paths)", a.reason, a.bad, a.badOf)
	}
	return a.reason
}

// applyReport fills rows (listening side) from the dialing side's report:
// effective share, targets, factor, reason and the sender's loss. keyOf maps a
// reported path to its row key ("" skips it).
func applyReport(rows []LBPathView, rep LBReport, keyOf func(LBPeerPath) string) {
	ag := map[string]*rowAgg{}
	for _, p := range rep.Paths {
		k := keyOf(p)
		if k == "" {
			continue
		}
		a := ag[k]
		if a == nil {
			a = &rowAgg{}
			ag[k] = a
		}
		a.eff += p.EffPct
		a.tgt += p.Target
		a.fsum += p.Factor
		a.n++
		w := float64(max(p.Links, 1))
		l := p.LossPct // smoothed; the latest reading when there is one (what the dialing side shows)
		if p.raw {
			l = p.RawPct
		}
		a.loss += w * l
		a.lw += w
		if rep.Auto2 {
			a.addReason(p.Reason)
		}
	}
	for i := range rows {
		a := ag[rows[i].key]
		if a == nil {
			continue
		}
		rows[i].EffPct = round1pct(a.eff)
		rows[i].Target = a.tgt
		if rep.Auto && a.n > 0 {
			rows[i].AutoFactor = round2(a.fsum / float64(a.n))
		}
		rows[i].AutoReason = a.reasonText()
		if rep.Auto && a.lw > 0 {
			rx := rows[i].LossDown
			l := round2(a.loss / a.lw)
			rows[i].LossDown, rows[i].LossDownRx, rows[i].LossSrc = &l, rx, "sender"
		}
	}
}

// applySteer fills rows from the edge's own steering (no report from the
// dialing side): weight and reason per full path; keyOf maps a full path to
// its row key.
func applySteer(rows []LBPathView, f map[string]float64, r map[string]string, keyOf func(string) string) {
	ag := map[string]*rowAgg{}
	for p, x := range f {
		k := keyOf(p)
		a := ag[k]
		if a == nil {
			a = &rowAgg{}
			ag[k] = a
		}
		a.fsum += x
		a.n++
		a.addReason(r[p])
	}
	for i := range rows {
		if a := ag[rows[i].key]; a != nil && a.n > 0 {
			rows[i].AutoFactor = round2(a.fsum / float64(a.n))
			rows[i].AutoReason = a.reasonText()
		}
	}
}

// markLossSrc labels the rows whose download loss is this side's own
// receiver estimate.
func markLossSrc(rows []LBPathView, exit bool) {
	for i := range rows {
		if rows[i].LossSrc == "" && rows[i].LossDown != nil {
			if exit {
				rows[i].LossSrc = "sender"
			} else {
				rows[i].LossSrc = "receiver"
			}
		}
	}
}

// lbReportFresh: a report older than this is stale — not shown, and the edge
// stops steering by it (the usual spread: the configured shares).
const lbReportFresh = 15 * time.Second
