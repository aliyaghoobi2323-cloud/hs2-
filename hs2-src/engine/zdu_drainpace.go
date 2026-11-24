package engine

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// b447: a paced drain end on the old exit (Kharej) process.
//
// When the old Kharej reaches its drain deadline with users still on its
// links (HH:03 in production), it used to exit at once: every link and every
// user connection riding them closed in the same instant. Iran then tears
// down all those user connections at once and their clients reconnect
// through the new process together. Lab l447 (prod-t1 pair, exact t1
// drop-ins, 20 pps ICMP through the TUN, tcpdump on both TUNs) measured:
//   - 184 links / 168 users closed at once: no extra ICMP delay;
//   - 192 links / 6068 users closed at once: Iran's process stalls both TUN
//     directions for ~0.4 s (request one-way 37 -> 380 ms, reply 37 -> 242 ms,
//     0 lost), Iran CPU 2.5 -> 4.5 cores; the new Kharej stays idle until
//     the reconnects arrive (+3 s).
//
// So the burst size in user connections, not the link count, is the cost.
//
// b454 (scale): the b447 shape (400 users per 150 ms, a hard 4 s budget)
// held at 6k user connections (63-73 ms TUN max on Iran) but not at 12k
// (r453: 30 batches over 4.44 s, the last ~3 past the budget closed unpaced
// together, then the exit: 356-1629 ms TUN delay at +4..8 s, 12-58 ICMP
// lost). At 4x users the old cap only made the batches bigger. Now the pace
// is a rate, not a batch count: users close at zduPaceRate a second (2667 =
// the proven 400 per 150 ms), one small batch at a time (at least
// rate x zduPaceMinStep users, or one whole link), and every batch, the
// last included, waits its share of the rate: a batch of n users is
// followed by n/rate. The whole close takes users/rate; only past
// zduPaceMax (30 s, ~80k users) does the rate rise to fit it. The drain
// loop starts the pace that long (ZDUDrainEndPaceLead) before the deadline,
// so the exit still lands at the deadline (HH:03) at any user count.
// Links never redial here: the draining exit pool retires every slot whose
// link ends (exit_pool.go). The process exits right after, as before.
//
//	HS2_ZDU_PACE_RATE unset or bad  2667 user connections a second
//	HS2_ZDU_PACE_RATE=N             N a second (0: the old all-at-once close)
//
// HS2_ZDU off, not draining, or not an exit: returns at once.
var (
	zduPaceRate    = zduPaceRateEnv(os.Getenv("HS2_ZDU_PACE_RATE"))
	zduPaceMinStep = 25 * time.Millisecond // smallest step between batches
	zduPaceMax     = 30 * time.Second      // whole paced close at most
)

// zduPaceRateEnv: the users-a-second rate HS2_ZDU_PACE_RATE asks for.
func zduPaceRateEnv(env string) int {
	const def = 2667
	env = strings.TrimSpace(env)
	if env == "" {
		return def
	}
	n, err := strconv.Atoi(env)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// ZDUDrainEndPaceLead: how long the paced close of users user connections
// takes (the drain loop starts it that long before its deadline); 0: off.
func ZDUDrainEndPaceLead(users int) time.Duration {
	return zduPaceDur(users, zduPaceRate, zduPaceMax)
}

func zduPaceDur(users, rate int, budget time.Duration) time.Duration {
	if users <= 0 || rate <= 0 {
		return 0
	}
	return min(time.Duration(users)*time.Second/time.Duration(rate), budget)
}

// zduPaceLink is one exit link to close: its user count and its close.
type zduPaceLink struct {
	users int
	close func()
}

// ZDUDrainEndPace closes this draining exit's links at the paced rate (see
// above) and returns the links closed, the users on them and the batches
// used. sleep is time.Sleep outside tests.
func ZDUDrainEndPace(sleep func(time.Duration)) (links, users, batches int) {
	if !zduOn.Load() || !zduDraining.Load() {
		return 0, 0, 0
	}
	p := zduExitPeers.Load()
	if p == nil {
		return 0, 0, 0
	}
	if t := zduExitTraffic.Load(); t != nil && b468DrainFastOn() {
		return b468PaceUsers(t, p, sleep)
	}
	p.mu.Lock()
	ls := make([]zduPaceLink, 0, len(p.m))
	for m := range p.m {
		if c := m.tcp; c != nil {
			ls = append(ls, zduPaceLink{users: int(m.users.Load()), close: func() { c.Close() }})
		}
	}
	p.mu.Unlock()
	return zduPaceClose(ls, zduPaceRate, zduPaceMinStep, zduPaceMax, sleep)
}

// zduPaceClose closes ls (idle links first, then by users ascending) in
// batches of at least rate x minStep users (or one link), each batch of n
// users followed by n/rate before the next one, so users close at rate a
// second and the whole close never takes longer than budget (the rate rises
// to fit). rate <= 0: everything at once.
func zduPaceClose(ls []zduPaceLink, rate int, minStep, budget time.Duration, sleep func(time.Duration)) (links, users, batches int) {
	if len(ls) == 0 {
		return 0, 0, 0
	}
	sort.SliceStable(ls, func(i, j int) bool { return ls[i].users < ls[j].users })
	total := 0
	for _, l := range ls {
		total += l.users
	}
	r := float64(rate)
	if rate > 0 && budget > 0 && float64(total)/r > budget.Seconds() {
		r = float64(total) / budget.Seconds() // over the budget: the rate rises, every batch still paced
	}
	minUsers := 1
	if rate > 0 {
		minUsers = max(int(r*minStep.Seconds()), 1)
	}
	var batch []zduPaceLink
	inBatch, owe := 0, 0 // owe: the previous batch's users, not yet waited for
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if owe > 0 && rate > 0 {
			sleep(time.Duration(float64(owe) / r * float64(time.Second)))
		}
		for _, l := range batch {
			l.close()
			links++
			users += l.users
		}
		batches++
		owe = inBatch
		batch, inBatch = batch[:0], 0
	}
	for _, l := range ls {
		batch = append(batch, l)
		inBatch += l.users
		if rate > 0 && inBatch >= minUsers {
			flush()
		}
	}
	flush()
	return links, users, batches
}
