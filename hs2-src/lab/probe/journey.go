package main

// journey: the "user journey" scenario, the acceptance test for tunnel changes.
//
// -journey N runs N virtual users at once, each like a real v2rayNG user with
// Mux off (every app action is its own TCP connection). A user starts at a
// random moment in the first -jspread seconds and then goes through these
// phases in order, over and over until the probe ends, so at any moment the
// users are spread over different phases:
//
//  1. reels:  -jreels short connections of -jreelmin..-jreelmax bytes, a pause
//     of -jreelgapmin..-jreelgapmax between them (Instagram reels/posts).
//     Metrics: time to the first -jttf bytes (TTFMB) and the whole fetch.
//  2. call:   a two-way constant stream of -jcallsize byte frames at
//     -jcallmin..-jcallmax bit/s for -jcalltmin..-jcalltmax (video call).
//     Metrics: round trip p50/p95/p99/max, share of frames above -latems.
//  3. bulk:   one connection downloading flat out for -jbulktmin..-jbulktmax
//     (a big download). Metric: Mbit/s per stream.
//  4. ping:   right after the download, -jpings times: fresh connection, a
//     1-byte request, its reply (v2rayNG "real delay"). Metric: whole time.
//  5. game:   60-200 byte frames every 20-50 ms, echoed, for -jgametmin..
//     -jgametmax. Metrics: round trip p50/p99/max, share above -latems.
//
// -jrotate (default on) starts user i at phase i%4 of its first round (ping
// always follows the download), so even a 60-90 s run samples all five phases
// and the users are spread over the phases from the start; -jrotate=false
// makes every user begin with reels.
//
// -journeyscale multiplies the sizes (reels, TTFMB threshold) and rates (call)
// so the scenario stays meaningful on a slow emulated link.
//
// The JSON object "journey" aggregates every phase over all users; a phase cut
// short by the end of the run still reports what it measured.

import (
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// dist is a percentile summary of one set of samples.
type dist struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
	Max float64 `json:"max_ms"`
}

func mkDist(v []float64) dist {
	return dist{N: len(v), P50: pct(v, .5), P95: pct(v, .95), P99: pct(v, .99), Max: pct(v, 1)}
}

type jReels struct {
	Started    int     `json:"started"`
	Done       int     `json:"done"`
	Unfinished int     `json:"unfinished"` // cut by the end of the run
	Fail       int     `json:"fail"`
	MBAvg      float64 `json:"mb_avg"` // mean size of the finished ones
	TTFMB      dist    `json:"ttfmb"`  // time to the first -jttf bytes
	Total      dist    `json:"total"`  // connect to last byte
	MbpsP50    float64 `json:"mbps_p50"`
}

type jStream struct {
	Streams    int     `json:"streams"`
	Unfinished int     `json:"unfinished"`
	Fail       int     `json:"fail"`
	Sent       int     `json:"sent"`
	Lost       int     `json:"lost"`
	LatePct    float64 `json:"late_pct"`
	RTT        dist    `json:"rtt"`
	MbpsAvg    float64 `json:"mbps_avg,omitempty"` // offered one-way rate (call)
}

type jBulk struct {
	Streams    int     `json:"streams"`
	Unfinished int     `json:"unfinished"`
	Fail       int     `json:"fail"`
	MB         float64 `json:"mb"` // all bytes, all streams
	MbpsP50    float64 `json:"mbps_p50"`
	MbpsMin    float64 `json:"mbps_min"`
	MbpsMax    float64 `json:"mbps_max"`
	SecAvg     float64 `json:"sec_avg"`
	// Short: downloads that ended (EOF or error) before their planned time
	// with the run still going; ShortSec: the planned seconds they lost.
	Short    int     `json:"short"`
	ShortSec float64 `json:"short_sec"`
	ShortErr string  `json:"short_err,omitempty"` // the first such error
}

type jPing struct {
	Fail int `json:"fail"`
	dist
}

type journeyResult struct {
	Users  int     `json:"users"`
	Scale  float64 `json:"scale"`
	Rounds int     `json:"rounds"` // whole journeys finished
	Reels  jReels  `json:"reels"`
	Call   jStream `json:"call"`
	Bulk   jBulk   `json:"bulk"`
	Ping   jPing   `json:"ping"`
	Game   jStream `json:"game"`
	// PhaseSec: seconds all users spent in each phase (reels with their
	// gaps, call, bulk with its dial, ping, game, the gap between rounds,
	// the start spread) — where the run's time went.
	PhaseSec map[string]float64 `json:"phase_sec"`
}

type journeySpec struct {
	addr               string
	users              int
	scale              float64
	spread             time.Duration
	reels              int
	reelMin, reelMax   int
	ttf                int
	reelGapMin, gapMax time.Duration
	callMin, callMax   float64 // bit/s
	callSize           int
	callTMin, callTMax time.Duration
	bulkTMin, bulkTMax time.Duration
	pings              int
	gameTMin, gameTMax time.Duration
	lateMs             float64
	rotate             bool // user i starts at phase i%4 (see user)
	// seed != 0: each user's phase plan (start, sizes, gaps, durations, call
	// rate) comes from its own generator seeded with (seed, user id), so two
	// runs with the same seed (e.g. with and without a feature) get the same
	// workload; 0: random each run.
	seed uint64
}

func randDur(r *rand.Rand, a, b time.Duration) time.Duration {
	if b <= a {
		return a
	}
	return a + time.Duration(r.Int64N(int64(b-a)+1))
}

func randInt(r *rand.Rand, a, b int) int {
	if b <= a {
		return a
	}
	return a + r.IntN(b-a+1)
}

// journeyAgg collects the samples of all users.
type journeyAgg struct {
	mu        sync.Mutex
	res       journeyResult
	ttfmb     []float64
	reelTotal []float64
	reelMbps  []float64
	reelBytes int64
	callRTT   []float64
	callLate  int
	callMbps  float64
	bulkMbps  []float64
	bulkSec   float64
	bulkBytes int64
	ping      []float64
	gameRTT   []float64
	gameLate  int
}

// stopped tells whether the probe has ended.
func stopped(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// waitOr sleeps d unless stop closes first.
func waitOr(d time.Duration, stop <-chan struct{}) bool {
	select {
	case <-stop:
		return false
	case <-time.After(d):
		return true
	}
}

// phaseStop makes a stop channel that closes after d or when the probe stops.
func phaseStop(d time.Duration, stop <-chan struct{}) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		waitOr(d, stop)
		close(ch)
	}()
	return ch
}

// reel downloads n bytes on a fresh connection (the 'R' request).
func (a *journeyAgg) reel(sp journeySpec, n int, stop <-chan struct{}) {
	a.mu.Lock()
	a.res.Reels.Started++
	a.mu.Unlock()
	s := time.Now()
	fail := func() {
		a.mu.Lock()
		if stopped(stop) {
			a.res.Reels.Unfinished++
		} else {
			a.res.Reels.Fail++
		}
		a.mu.Unlock()
	}
	c, err := net.DialTimeout("tcp", sp.addr, 10*time.Second)
	if err != nil {
		fail()
		return
	}
	defer c.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-stop:
			c.Close()
		case <-done:
		}
	}()
	pad := 200 + rand.IntN(400)
	req := make([]byte, 7+pad)
	req[0] = 'R'
	req[1], req[2] = byte(pad>>8), byte(pad)
	req[3], req[4], req[5], req[6] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	c.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := c.Write(req); err != nil {
		fail()
		return
	}
	buf := make([]byte, 64<<10)
	got, ttf := 0, -1.0
	thr := min(sp.ttf, n)
	for got < n {
		k, err := c.Read(buf[:min(len(buf), n-got)])
		got += k
		if ttf < 0 && got >= thr {
			ttf = ms(time.Since(s))
		}
		if err != nil {
			break
		}
	}
	el := time.Since(s)
	a.mu.Lock()
	defer a.mu.Unlock()
	if ttf >= 0 {
		a.ttfmb = append(a.ttfmb, ttf)
	}
	if got < n {
		if stopped(stop) {
			a.res.Reels.Unfinished++
		} else {
			a.res.Reels.Fail++
		}
		return
	}
	a.res.Reels.Done++
	a.reelBytes += int64(n)
	a.reelTotal = append(a.reelTotal, ms(el))
	a.reelMbps = append(a.reelMbps, float64(n)*8/el.Seconds()/1e6)
}

// bulkPhase downloads flat out on one connection for d.
func (a *journeyAgg) bulkPhase(sp journeySpec, d time.Duration, stop <-chan struct{}) {
	a.mu.Lock()
	a.res.Bulk.Streams++
	a.mu.Unlock()
	c, err := net.DialTimeout("tcp", sp.addr, 10*time.Second)
	if err != nil {
		a.mu.Lock()
		a.res.Bulk.Fail++
		a.mu.Unlock()
		return
	}
	defer c.Close()
	ps := phaseStop(d, stop)
	go func() { <-ps; c.Close() }()
	s := time.Now()
	c.Write([]byte{'B'})
	buf := make([]byte, 64<<10)
	var got int64
	var rerr error
	for {
		k, err := c.Read(buf)
		got += int64(k)
		if err != nil {
			rerr = err
			break
		}
	}
	el := time.Since(s)
	a.mu.Lock()
	defer a.mu.Unlock()
	if !stopped(stop) && el < d-time.Second {
		a.res.Bulk.Short++
		a.res.Bulk.ShortSec += (d - el).Seconds()
		if a.res.Bulk.ShortErr == "" {
			a.res.Bulk.ShortErr = fmt.Sprintf("%v after %.1fs of %.1fs, %d bytes", rerr, el.Seconds(), d.Seconds(), got)
		}
	}
	if stopped(stop) && el < d {
		a.res.Bulk.Unfinished++
	}
	if el.Seconds() < 0.5 && !stopped(stop) {
		a.res.Bulk.Fail++
		return
	}
	a.bulkBytes += got
	a.bulkSec += el.Seconds()
	a.bulkMbps = append(a.bulkMbps, float64(got)*8/el.Seconds()/1e6)
}

// pingPhase: fresh connection, 1-byte request, its reply.
func (a *journeyAgg) pingPhase(sp journeySpec, stop <-chan struct{}) {
	for i := 0; i < sp.pings; i++ {
		if stopped(stop) {
			return
		}
		s := time.Now()
		c, err := net.DialTimeout("tcp", sp.addr, 5*time.Second)
		if err == nil {
			c.SetDeadline(time.Now().Add(5 * time.Second))
			var b [1]byte
			if _, err = c.Write([]byte{'E'}); err == nil {
				_, err = io.ReadFull(c, b[:])
			}
			c.Close()
		}
		a.mu.Lock()
		if err != nil {
			a.res.Ping.Fail++
		} else {
			a.ping = append(a.ping, ms(time.Since(s)))
		}
		a.mu.Unlock()
		if i+1 < sp.pings && !waitOr(200*time.Millisecond, stop) {
			return
		}
	}
}

// streamPhase runs a call or game stream for d and folds it into dst.
func (a *journeyAgg) streamPhase(sp journeySpec, dst *jStream, rtts *[]float64, late *int, mbps *float64,
	gap func() time.Duration, size func() int, bps float64, d time.Duration, stop <-chan struct{}) {
	ps := phaseStop(d, stop)
	s := time.Now()
	r := runStream(streamSpec{addr: sp.addr, gap: gap, size: size, start: s, lateMs: sp.lateMs, mbps: bps / 1e6}, ps)
	el := time.Since(s)
	a.mu.Lock()
	defer a.mu.Unlock()
	dst.Streams++
	if r.Error != "" && r.N == 0 {
		dst.Fail++
		return
	}
	if stopped(stop) && el < d {
		dst.Unfinished++
	}
	dst.Sent += r.Sent
	dst.Lost += r.Lost
	*rtts = append(*rtts, r.rtts...)
	for _, v := range r.rtts {
		if v > sp.lateMs {
			*late++
		}
	}
	*late += r.Lost
	*mbps += bps / 1e6
}

// phase adds the time since *t0 to phase name and restarts *t0.
func (a *journeyAgg) phase(name string, t0 *time.Time) {
	now := time.Now()
	a.mu.Lock()
	if a.res.PhaseSec == nil {
		a.res.PhaseSec = map[string]float64{}
	}
	a.res.PhaseSec[name] += now.Sub(*t0).Seconds()
	a.mu.Unlock()
	*t0 = now
}

// user runs one virtual user's journey until stop.
func (a *journeyAgg) user(sp journeySpec, id int, stop <-chan struct{}) {
	seed := sp.seed
	if seed == 0 {
		seed = rand.Uint64()
	}
	rng := rand.New(rand.NewPCG(seed, uint64(id)))
	t0 := time.Now()
	ok := waitOr(time.Duration(rng.Int64N(int64(sp.spread)+1)), stop)
	a.phase("spread", &t0)
	if !ok {
		return
	}
	sc := func(n int) int { return max(int(float64(n)*sp.scale), 1) }
	// With rotate, user id starts its first round at phase id%4 of reels, call,
	// bulk(+ping), game, so a short run still samples every phase and the users
	// are spread over the phases from the start. The order is kept.
	first := 0
	if sp.rotate {
		first = id % 4
	}
	for !stopped(stop) {
		// 1. reels
		for i := 0; first == 0 && i < sp.reels && !stopped(stop); i++ {
			a.reel(sp, sc(randInt(rng, sp.reelMin, sp.reelMax)), stop)
			if i+1 < sp.reels && !waitOr(randDur(rng, sp.reelGapMin, sp.gapMax), stop) {
				a.phase("reels", &t0)
				return
			}
		}
		a.phase("reels", &t0)
		if stopped(stop) {
			return
		}
		// 2. call
		bps := (sp.callMin + rng.Float64()*(sp.callMax-sp.callMin)) * sp.scale
		gap, size := callSpec(bps, sp.callSize)
		if first <= 1 {
			a.streamPhase(sp, &a.res.Call, &a.callRTT, &a.callLate, &a.callMbps, gap, size, bps,
				randDur(rng, sp.callTMin, sp.callTMax), stop)
		}
		a.phase("call", &t0)
		if stopped(stop) {
			return
		}
		// 3. bulk, 4. ping right after it
		if first <= 2 {
			a.bulkPhase(sp, randDur(rng, sp.bulkTMin, sp.bulkTMax), stop)
			a.phase("bulk", &t0)
			if stopped(stop) {
				return
			}
			a.pingPhase(sp, stop)
			a.phase("ping", &t0)
		}
		if stopped(stop) {
			return
		}
		// 5. game
		gap, size = gameSpec()
		var none float64
		a.streamPhase(sp, &a.res.Game, &a.gameRTT, &a.gameLate, &none, gap, size, 0,
			randDur(rng, sp.gameTMin, sp.gameTMax), stop)
		a.phase("game", &t0)
		if stopped(stop) {
			return
		}
		if first == 0 {
			a.mu.Lock()
			a.res.Rounds++
			a.mu.Unlock()
		}
		first = 0
		ok := waitOr(randDur(rng, sp.reelGapMin, sp.gapMax), stop)
		a.phase("roundgap", &t0)
		if !ok {
			return
		}
	}
}

// runJourney runs sp.users users until stop and returns the aggregate.
func runJourney(sp journeySpec, stop <-chan struct{}) *journeyResult {
	a := &journeyAgg{}
	a.res.Users, a.res.Scale = sp.users, sp.scale
	var wg sync.WaitGroup
	for i := 0; i < sp.users; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.user(sp, i, stop)
		}()
	}
	wg.Wait()
	a.mu.Lock()
	defer a.mu.Unlock()
	r := &a.res
	r.Reels.TTFMB, r.Reels.Total = mkDist(a.ttfmb), mkDist(a.reelTotal)
	r.Reels.MbpsP50 = pct(a.reelMbps, .5)
	if r.Reels.Done > 0 {
		r.Reels.MBAvg = float64(a.reelBytes) / float64(r.Reels.Done) / 1e6
	}
	fill := func(d *jStream, rtts []float64, late int, mbps float64) {
		d.RTT = mkDist(rtts)
		if d.Sent > 0 {
			d.LatePct = 100 * float64(late) / float64(d.Sent)
		}
		if n := d.Streams - d.Fail; n > 0 && mbps > 0 {
			d.MbpsAvg = mbps / float64(n)
		}
	}
	fill(&r.Call, a.callRTT, a.callLate, a.callMbps)
	fill(&r.Game, a.gameRTT, a.gameLate, 0)
	r.Bulk.MB = float64(a.bulkBytes) / 1e6
	r.Bulk.MbpsP50, r.Bulk.MbpsMax = pct(a.bulkMbps, .5), pct(a.bulkMbps, 1)
	r.Bulk.MbpsMin = pct(a.bulkMbps, 0)
	if n := len(a.bulkMbps); n > 0 {
		r.Bulk.SecAvg = a.bulkSec / float64(n)
	}
	r.Ping.dist = mkDist(a.ping)
	return r
}
