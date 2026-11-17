package engine

import "time"

// HS2_LB_SILENT=1 (bh93, off by default): a SILENT path is read as bad.
//
// fc90 (results90): a blackholed path mostly gives NO reading at all — its
// links get no ACKs, so the few segments they put out (RTO backoff) never add
// up to lb2MinSegs, and auto2 never reads it bad (bL: 11.9 s or missed with
// every guard off; lbv81: lowered +13/+7 s, out of service +41/+46 s).
//
// With the key on, a path whose links had data waiting for an ACK (an RTO
// fired: backoff > 0, or segments resent) and got NOTHING acked over a whole
// reading window is silent, and its reading is silentLoss (100 %) — but only
// while some other path of the same tunnel read normally in the same tick:
//   - idle tunnel: no path reads at all -> no silent reading counts;
//   - a path with no traffic assigned: nothing waits for an ACK (no backoff,
//     nothing resent) -> not silent (the exit's smux keepalive / the edge's
//     control pings make a live idle link ack something every few seconds);
//   - lossy but alive: something is acked -> not silent, judged by its loss.
//
// A silent reading lowers the path at once (smoothed loss jumps) and cuts it
// on the next one (lb2CutRuns); the first normal reading after a silence
// restarts the smoothing from that reading (a silence is not a loss sample),
// so a path whose bytes flow again recovers by the usual hold.
//
// bh98: "flow again" needs evidence of two-way delivery. After a silent cut
// drains a blackholed path its links have nothing of their own waiting, so
// the next reading is not silent; its segments out are then this side's
// ACKs for what the other side (re)sends, never resent: raw loss 0 % with
// not one byte acked (bh95: re-admitted ~25 s into a 120 s blackhole). Once
// a path has read silent, a reading whose links got none of their bytes
// acked (noAck) is still silent (silentHold); only a reading with bytes acked
// (the probe/keepalive traffic of the path's kept links, or users) ends the
// silence — then the usual FAST_RECOVER / hold apply, so a cleared path is
// back within a reading or two. A cut path's kept links are mostly idle
// (too few segments for a reading for a minute or more): bytes acked on them
// still end the silence at once (ackOnly, silentAckOnly).
//
// The dialing side decides (lb_auto.go measure -> adapt2; in reverse l3mtcp:
// the exit); the edge mirrors its cuts (HS2_LB_EDGE_SYNC) and reads silence
// itself only for the paths its fresh report does not cover (steerTickLocked).
var lbSilent bool // loadEnvKeys: HS2_LB_SILENT

// bh100: a blackholed path comes back by its cooled-path probe, not by the
// judge. Its dials time out while it is dark, so it is cooled (resultLocked),
// adapt2 stops reading it (not eligible) and the judge forgets it; once the
// blackhole clears, the only way back is the cooled path's probe dial — one
// every lbProbeEvery (2 min; 20 s with the lab's HS2_LB_FAST), which put the
// path back 22-32 s after a 120 s blackhole cleared in the lab (bh98 saw no
// "recovering" line at all: a path that comes back from a cooldown starts
// the judge afresh at its full share, no hold). With the key on, a path
// cooled for dials that failed before TCP connected (no SYN-ACK: dark, not
// refusing at TLS) is probed every lbSilentProbe instead: one SYN per probe,
// which only a two-way path answers (asymmetric blackholes included), so it
// stays out while dark and is back within a probe of the clear.
var lbSilentProbe = 2 * time.Second

// silentBackTarget: a kept path the judge has no entry for gets target 1.
// bh100 lab (a1bIS1): the judge forgets a cooled path; once its probe link is
// up again it is eligible but has no judge entry until it gives a reading,
// and its 2 probe-floor links carry too few segments for one (a minute of
// carry and still < lb2MinSegs/4), so adapt2 kept its factor at the cut's
// 0.10 for the rest of the run (2 links of ~28 at clear+110 s). A path back
// from a cooldown proved two-way delivery: like a path with no reading yet it
// gets its configured share back step by step (lbMaxStepPct a reading), and
// the judge lowers or cuts it again on its first bad reading.
func silentBackTarget(kept bool) bool { return lbSilent && kept }

// silentProbeDueLocked: a cooled path's probe is due and not in flight. A
// probe is the next dial the pool makes (acquireLocked), and a steady pool
// makes none: bh100 b2bIS1 had both paths of a cleared Iran IP back only
// +22.8 s after the clear, at the pool's next dial. With the key on, fillMin
// asks for one spare dial then (lb2SpareMax bounds the pool's surplus; a
// failed probe's slot redials on a live path). Caller holds b.mu.
func (b *LB) silentProbeDueLocked(now time.Time) bool {
	if !lbSilent {
		return false
	}
	for j := range b.paths {
		if !b.until[j].IsZero() && !now.Before(b.until[j]) && !b.probing[j] {
			return true
		}
	}
	return false
}

// silentCooldown is the cooldown after a failed dial err (cd: the usual one).
func silentCooldown(cd time.Duration, err error) time.Duration {
	if lbSilent && cd > lbSilentProbe && dialConnectStage(err) {
		return lbSilentProbe
	}
	return cd
}

// silentLoss is a silent path's reading.
const silentLoss = 1.0

// silentAcc is one reading window's counts of a path's links.
type silentAcc struct {
	links   int    // links measured over the whole window
	acked   uint64 // bytes they got acked
	stalled bool   // one of them waited for an ACK (RTO backoff, or resent)
}

func (a *silentAcc) add(ackD, retrD uint64, backoff uint8) {
	a.links++
	a.acked += ackD
	if backoff > 0 || retrD > 0 {
		a.stalled = true
	}
}

func (a *silentAcc) silent() bool { return a != nil && a.links > 0 && a.acked == 0 && a.stalled }

// noAck: links were measured over the window and none of their bytes was
// acked (no evidence of two-way delivery, stalled or not).
func (a *silentAcc) noAck() bool { return a != nil && a.links > 0 && a.acked == 0 }

// silentAckOnly keeps an ackOnly reading (bytes acked on a path whose links
// put out too few segments for a reading: a cut path's idle kept links once
// the blackhole clears) only for a path that has read silent: it ends that
// silence at once. For any other path it is dropped (no reading, as before).
func (j *q2Judge) silentAckOnly(rs map[string]q2Reading) map[string]q2Reading {
	var drop []string
	for k, r := range rs {
		if !r.ackOnly {
			continue
		}
		if q := j.p[k]; q == nil || q.silentRuns == 0 {
			drop = append(drop, k)
		}
	}
	if len(drop) == 0 {
		return rs
	}
	out := make(map[string]q2Reading, len(rs))
	for k, r := range rs {
		out[k] = r
	}
	for _, k := range drop {
		delete(out, k)
	}
	return out
}

// silentHold turns a noAck reading of a path that has read silent (and not
// acked since) into a silent one: such a path recovers only on bytes acked.
// rs is returned as is when nothing changes.
func (j *q2Judge) silentHold(rs map[string]q2Reading) map[string]q2Reading {
	var out map[string]q2Reading
	for k, r := range rs {
		q := j.p[k]
		if q == nil || q.silentRuns == 0 || r.silent || !r.noAck {
			continue
		}
		if out == nil {
			out = make(map[string]q2Reading, len(rs))
			for k2, r2 := range rs {
				out[k2] = r2
			}
		}
		out[k] = q2Reading{loss: silentLoss, segs: r.segs, silent: true}
		if !q.silentHeld {
			q.silentHeld = true
			j.log("lb auto2: path %s still silent: raw loss %.1f%% but not one byte acked — kept silent until bytes are acked (HS2_LB_SILENT)", k, 100*r.loss)
		}
	}
	if out == nil {
		return rs
	}
	return out
}

// silentFilter drops the silent readings of a tick in which no path read
// normally (an idle tunnel, or every path stalled at once: not one path's
// fault). rs is returned as is when nothing is dropped.
func silentFilter(rs map[string]q2Reading) map[string]q2Reading {
	normal, sil := false, false
	for _, r := range rs {
		if r.silent {
			sil = true
		} else {
			normal = true
		}
	}
	if !sil || normal {
		return rs
	}
	out := make(map[string]q2Reading, len(rs))
	for k, r := range rs {
		if !r.silent {
			out[k] = r
		}
	}
	return out
}

// silentFold notes a path's reading for the silence bookkeeping: it returns
// true when the smoothing should restart from this reading (the first normal
// reading after a silence).
func (j *q2Judge) silentFold(k string, q *q2Path, r q2Reading, others int) bool {
	if r.silent {
		q.silentRuns++
		if q.silentRuns == 1 {
			j.log("lb auto2: path %s silent: nothing acked for a reading while %d other paths read — read as 100%% loss (HS2_LB_SILENT)", k, others)
		}
		return false
	}
	if q.silentRuns > 0 {
		j.log("lb auto2: path %s acks again after %d silent readings (raw loss %.1f%%)", k, q.silentRuns, 100*r.loss)
		q.silentRuns, q.silentHeld = 0, false
		return true
	}
	return false
}

// HS2_LB_SILENT_REGROW=1 (bh101, lab key, off; acts only with HS2_LB_SILENT):
// a path back from a blackhole regrows its links faster.
//
// bh100/bh101 lab: a cleared path is back (probe link up, share 1.00) within
// 2-8 s, but its LINKS come back slowly: 2 -> 4..15 of ~18-20 at clear+110 s,
// S0 no faster. rebalance2 moves links only by retiring idle ones on the
// paths over their share — one per path a round (every lb2RebalEvery, 10 s),
// lb2DrainBatch (2) a round, lb2DrainPerMin (12) a minute, and only while the
// pool is whole (c1bIS1: 2 retires every 10 s from the clear on, a 30 s gap
// at the per-minute cap) — so ~16 missing links take 80 s and more.
// With this key, while an eligible path is lb2RegrowGap links or more under
// its share, a round may retire up to lb2RegrowBatch links (still one per
// donor path, still only idle ones, still only while the pool is whole) and
// lb2RegrowPerMin a minute: at most 0.4 replacement dials a second.
var lbSilentRegrow bool // loadEnvKeys: HS2_LB_SILENT_REGROW

const (
	lb2RegrowGap    = 4.0
	lb2RegrowBatch  = 4
	lb2RegrowPerMin = 24
)

// drainLimitsLocked is rebalance2's batch and per-minute cap. Caller holds b.mu.
func (b *LB) drainLimitsLocked(now time.Time, total int) (batch, perMin int) {
	if !lbSilent || !lbSilentRegrow || total < 2 {
		return lb2DrainBatch, lb2DrainPerMin
	}
	eff := b.effWeightsLocked()
	w := 0
	for j := range b.paths {
		if b.eligibleLocked(j, now) {
			w += eff[j]
		}
	}
	if w == 0 {
		return lb2DrainBatch, lb2DrainPerMin
	}
	for j := range b.paths {
		if !b.eligibleLocked(j, now) || eff[j] == 0 {
			continue
		}
		if float64(total)*float64(eff[j])/float64(w)-float64(b.links[j]) >= lb2RegrowGap {
			return lb2RegrowBatch, lb2RegrowPerMin
		}
	}
	return lb2DrainBatch, lb2DrainPerMin
}
