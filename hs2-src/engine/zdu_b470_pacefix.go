package engine

import (
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// b470 (rc14j, HS2_B470_PACEFIX): the paced drain end keeps its deadline at
// 24k users.
//
// rc14i lab1 (24068 user conns, ZDU exit, DRAINFAST): the drain loop asked
// ZDUDrainEndPaceLead for the lead with the exit's conn count (24068, every
// call/game sub counted) while the pace closes only the user connections
// (12034): lead 9 s + 1 s period. And the pace itself reached only
// ~1000 users/s instead of 2667: "closed 198 links (12034 users) in 927
// batches over 12.064s" — every 5 ms batch of 13 kills took ~8 ms of
// sequential close work on top of its full sleep. The old process exited
// 1.3-2.1 s past the deadline and the new one logged "drain channel ended
// (handoff: timeout)". With the lead fixed alone the exit would be ~6 s late.
//
// The lead's overcount is what kept the proven 12k runs on time (6034
// users, lead from 12068 conns: ~1420/s over 4.25 s); the 2667/s the pace
// asks for was never reached. So the lead stays as it is and the pace fits it.
//
// With the key on:
//
//   - the user pace spreads its users over the lead it started with
//     (users/lead, never above HS2_ZDU_PACE_RATE): ~1340/s at 12k and 24k,
//     the rate of the proven 12k runs;
//
//   - the pace keeps a debt: close work and oversleep come off the next
//     sleep, and while behind the next batch grows to catch up, so the
//     close ends at users/rate whatever one close costs;
//
//   - a batch of 8+ entries closes on up to b470Par goroutines (one kill is
//     up.Close + mc.Close, ~0.6 ms each at 24k on one goroutine).
//
//     HS2_B470_PACEFIX=0          rc14 behaviour (2667/s asked, open-loop pace)
//     HS2_B470_PACEFIX unset / 1  the fix
//     HS2_B470_PAR=N (1..32, 4)   close goroutines per batch
var zduPaceNow = time.Now

func b470On() bool {
	return strings.TrimSpace(os.Getenv("HS2_B470_PACEFIX")) != "0"
}

func b470Par() int { return b469EnvInt("HS2_B470_PAR", 4, 1, 32) }

// b470Lead: the drain loop's last ZDUDrainEndPaceLead (the pace starts that
// long, plus one period, before the deadline); 0: none asked.
var b470Lead atomic.Int64

// b470Rate: the user pace's rate for users user connections: spread over
// the lead the drain loop started the pace with (users/lead, never above
// zduPaceRate), so the pace ends at the lead whatever the conn count the
// lead came from; key off or no lead: zduPaceRate.
func b470Rate(users int) int {
	lead := time.Duration(b470Lead.Load())
	if !b470On() || zduPaceRate <= 0 || lead <= 0 || users <= 0 {
		return zduPaceRate
	}
	r := int(math.Ceil(float64(users) / lead.Seconds()))
	return min(max(r, 1), zduPaceRate)
}

// b470CloseAll closes every entry of batch, in parallel (fix on, 8+ entries).
func b470CloseAll(batch []zduPaceLink, fix bool) {
	w := 1
	if fix && len(batch) >= 8 {
		w = min(b470Par(), len(batch))
	}
	if w <= 1 {
		for _, l := range batch {
			l.close()
		}
		return
	}
	var wg sync.WaitGroup
	ch := make(chan func(), len(batch))
	for _, l := range batch {
		ch <- l.close
	}
	close(ch)
	for range w {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range ch {
				f()
			}
		}()
	}
	wg.Wait()
}
