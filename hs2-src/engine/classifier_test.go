package engine

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// C1's model tests (REDESIGN §3.1): the classifier on synthetic time, one
// tick each classTick, and the replay of the edge's capture.

// clsTick is one evaluation of a flow.
type clsTick struct {
	at        time.Duration
	tx, flow  Class
	confirmed bool
}

// clsRun drives one flow in 1 ms steps for dur: bytes(ms) is what it moved
// that ms each way (this end's tx and rx, in (ms-1, ms]), and the classifier
// is ticked at the end of each ms that is phase past a multiple of classTick.
// It returns the evaluations and, per ms, the tx bytes C2 would have queued
// in the latency lane (queueClass of the verdict in force).
func clsRun(dur time.Duration, phase int, bytes func(ms int) (tx, rx int)) (ticks []clsTick, light []int) {
	var st flowStats
	var c flowClassifier
	cur := ClassUnknown
	n := int(dur / time.Millisecond)
	light = make([]int, n)
	for ms := 0; ms < n; ms++ {
		tx, rx := bytes(ms)
		if tx > 0 && queueClass(cur, &st) == ClassLight {
			light[ms] = tx
		}
		st.Add(true, tx)
		st.Add(false, rx)
		if ms >= phase && (ms-phase)%int(classTick/time.Millisecond) == 0 {
			now := time.Duration(ms) * time.Millisecond
			t, f, ok := c.Tick(&st, now)
			ticks = append(ticks, clsTick{at: now, tx: t, flow: f, confirmed: ok})
			cur = t
		}
	}
	return ticks, light
}

// clsOracle is the rule in continuous time: each ms, over exact windows of
// the last 200 ms and 2 s (one direction), unknown or light → light while
// the windows are light, bulk otherwise, and bulk → light only once quiet
// (less than classQuietBytes over 2 s). verdicts[ms] is the class at the
// end of that ms.
func clsOracle(n int, x func(ms int) int) []Class {
	out := make([]Class, n)
	cls := ClassUnknown
	b200, b2s := 0, 0
	v := make([]int, n)
	for ms := 0; ms < n; ms++ {
		v[ms] = x(ms)
		b200 += v[ms]
		b2s += v[ms]
		if ms >= 200 {
			b200 -= v[ms-200]
		}
		if ms >= 2000 {
			b2s -= v[ms-2000]
		}
		switch {
		case cls == ClassBulk && b2s >= classQuietBytes:
		case b2s < classLightSpan && b200 < classLightBurst:
			cls = ClassLight
		default:
			cls = ClassBulk
		}
		out[ms] = cls
	}
	return out
}

// clsLagOK says each evaluation is one the rule had within the tick before
// it: a verdict at most one tick late, never early.
func clsLagOK(t *testing.T, name string, ticks []clsTick, oracle []Class) {
	t.Helper()
	tick := int(classTick / time.Millisecond)
	for _, k := range ticks {
		ms := int(k.at / time.Millisecond)
		ok := false
		for m := max(0, ms-tick); m <= ms; m++ {
			if oracle[m] == k.tx {
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("%s: at %v the verdict is %v, the rule's over the tick before it is %v..%v", name, k.at, k.tx, oracle[max(0, ms-tick)], oracle[ms])
		}
	}
}

func sum(v []int) int {
	s := 0
	for _, x := range v {
		s += x
	}
	return s
}

// TestClassifierDownload: a flow that moves 1 MiB/s from its first byte is
// light (unknown, then possibly light at a first tick that came early) for
// at most its first 32 KiB and one tick: at every phase of the ticks, its
// first bulk verdict comes within one tick of its 32nd KiB, it stays bulk,
// and the latency lane got no more than 32 KiB and one tick of it. Both
// ends: the sender's class_tx and the receiver's class_flow.
func TestClassifierDownload(t *testing.T) {
	const rate = 1 << 20 // B/s
	per := rate / 1000
	cross := (32<<10 + per - 1) / per // ms of the 32nd KiB
	for phase := 0; phase < 100; phase += 3 {
		for _, sender := range []bool{true, false} {
			ticks, light := clsRun(3*time.Second, phase, func(ms int) (int, int) {
				if sender {
					return per, 0
				}
				return 0, per
			})
			first := time.Duration(-1)
			for _, k := range ticks {
				c := k.flow
				if sender {
					c = k.tx
				}
				if c == ClassBulk && first < 0 {
					first = k.at
				}
				if first >= 0 && c != ClassBulk {
					t.Fatalf("phase %d sender %v: back to %v at %v after bulk at %v", phase, sender, c, k.at, first)
				}
				if !sender && k.tx != ClassLight {
					t.Fatalf("phase %d: the receiver's own direction (nothing sent) is %v at %v", phase, k.tx, k.at)
				}
			}
			if first < 0 || first > time.Duration(cross)*time.Millisecond+classTick {
				t.Fatalf("phase %d sender %v: first bulk verdict at %v, the 32nd KiB at %d ms", phase, sender, first, cross)
			}
			if sender {
				if l := sum(light); l > 32<<10+rate/10 {
					t.Fatalf("phase %d: %d B in the latency lane, more than 32 KiB and a tick (%d)", phase, l, 32<<10+rate/10)
				}
			}
		}
	}
}

// TestClassifierHTTP: a 600 B request and an 8 KiB reply each second is
// light both ways at every tick, at either end, and confirmed light from
// the second tick on.
func TestClassifierHTTP(t *testing.T) {
	for _, phase := range []int{0, 37, 99} {
		for _, edge := range []bool{true, false} {
			ticks, _ := clsRun(10*time.Second, phase, func(ms int) (int, int) {
				req, rep := 0, 0
				switch r := ms % 1000; {
				case r == 5:
					req = 600
				case r >= 80 && r < 88: // the reply comes in 8 ms
					rep = 1 << 10
				}
				if edge {
					return req, rep
				}
				return rep, req
			})
			for i, k := range ticks {
				if k.tx != ClassLight || k.flow != ClassLight {
					t.Fatalf("phase %d edge %v: tick %d at %v: tx %v flow %v", phase, edge, i, k.at, k.tx, k.flow)
				}
				if k.confirmed != (i >= 1) {
					t.Fatalf("phase %d edge %v: tick %d at %v: confirmed %v", phase, edge, i, k.at, k.confirmed)
				}
			}
		}
	}
}

// TestClassifierBurst20KiB: 20 KiB within 200 ms makes a quiet flow bulk,
// and it stays bulk until it is quiet (less than 8 KiB over the last 2 s,
// at most 2 s after its last byte), then is light within a tick.
//
// The rule is judged each 100 ms over the last 200 ms, and the hot path
// counts bytes only (no clock), so a burst is seen by where the ticks fall:
// 20 KiB in chunks over 160 ms is caught at every phase; spread evenly over
// exactly 200 ms it is caught only by a tick within 160..240 ms of its start
// (80 of every 100 phases), and the evenly spread case is held to that.
func TestClassifierBurst20KiB(t *testing.T) {
	const start, total = 500, 20 << 10
	shapes := []struct {
		name  string
		bytes func(ms int) int
		last  int  // the burst's last ms
		every bool // caught at every phase
	}{
		{"5×4KiB/160ms", func(ms int) int {
			if d := ms - start; d >= 0 && d <= 160 && d%40 == 0 {
				return 4 << 10
			}
			return 0
		}, start + 160, true},
		{"even/200ms", func(ms int) int {
			if d := ms - start; d >= 0 && d < 200 {
				return total*(d+1)/200 - total*d/200
			}
			return 0
		}, start + 199, false},
	}
	for _, sh := range shapes {
		// quiet: the first ms from its last byte whose 2 s hold less than
		// 8 KiB of it (2 s of silence at the latest)
		quiet := sh.last
		for ; ; quiet++ {
			n := 0
			for m := quiet - 1999; m <= quiet; m++ {
				n += sh.bytes(m)
			}
			if n < classQuietBytes {
				break
			}
		}
		if quiet > sh.last+2000 {
			t.Fatalf("%s: quiet at %d ms, past 2 s of silence", sh.name, quiet)
		}
		q := time.Duration(quiet) * time.Millisecond
		caught := 0
		for phase := 0; phase < 100; phase++ {
			ticks, _ := clsRun(6*time.Second, phase, func(ms int) (int, int) { return sh.bytes(ms), 0 })
			bulkAt := time.Duration(-1)
			for _, k := range ticks {
				if k.tx == ClassBulk && bulkAt < 0 {
					bulkAt = k.at
				}
			}
			want := sh.every
			if !want {
				// a tick within 160..240 ms of the start (the windows that
				// hold 16 KiB of the even spread)
				for _, k := range ticks {
					if d := int(k.at/time.Millisecond) - start; d >= 159 && d <= 239 {
						want = true
					}
				}
			}
			if !want {
				if bulkAt >= 0 {
					t.Fatalf("%s phase %d: bulk at %v with no tick that holds 16 KiB", sh.name, phase, bulkAt)
				}
				continue
			}
			caught++
			if bulkAt < 0 || bulkAt > time.Duration(sh.last)*time.Millisecond+classTick {
				t.Fatalf("%s phase %d: bulk at %v, the burst ended at %d ms", sh.name, phase, bulkAt, sh.last)
			}
			for _, k := range ticks {
				switch {
				case k.at < bulkAt:
				case k.at < q && k.tx != ClassBulk:
					t.Fatalf("%s phase %d: %v at %v, before it was quiet (%v)", sh.name, phase, k.tx, k.at, q)
				case k.at >= q+classTick && k.tx != ClassLight:
					t.Fatalf("%s phase %d: still %v at %v, quiet since %v", sh.name, phase, k.tx, k.at, q)
				}
			}
		}
		t.Logf("%s: caught at %d of 100 phases", sh.name, caught)
	}
}

// TestClassifierTrickleIsQuiet: a flow that only trickles after a burst
// (an app's keepalives and acks) goes back to light once its last 2 s hold
// less than 8 KiB, never 2 s without a byte (decisions, T1); one that
// trickles past 8 KiB over 2 s stays bulk.
func TestClassifierTrickleIsQuiet(t *testing.T) {
	for _, c := range []struct {
		name  string
		every int // ms between its writes after the burst
		size  int
		light bool
	}{
		{"500 B each 200 ms (5 KB/2 s)", 200, 500, true},
		{"100 B each 20 ms (10 KB/2 s)", 20, 100, false},
		{"1 KiB each 200 ms (10 KiB/2 s)", 200, 1 << 10, false},
	} {
		for _, phase := range []int{0, 33, 77} {
			x := func(ms int) int {
				switch {
				case ms < 500:
					return 0
				case ms < 1000: // 512 KiB in 0.5 s
					return 1 << 10
				case ms%c.every == 0:
					return c.size
				}
				return 0
			}
			ticks, _ := clsRun(8*time.Second, phase, func(ms int) (int, int) { return x(ms), 0 })
			oracle := clsOracle(8000, x)
			clsLagOK(t, fmt.Sprintf("%s phase %d", c.name, phase), ticks, oracle)
			// the burst's 512 KiB leave the 2 s window at 3 s; a tick on
			got := ticks[len(ticks)-1].tx
			for _, k := range ticks {
				if k.at >= 3100*time.Millisecond && k.tx != got {
					t.Fatalf("%s phase %d: %v at %v, %v at the end", c.name, phase, k.tx, k.at, got)
				}
			}
			if (got == ClassLight) != c.light {
				t.Fatalf("%s phase %d: %v after the burst left the window", c.name, phase, got)
			}
		}
	}
}

// TestClassifierVoIPTun: 50 packets of 160 B a second each way (a call
// inside the TUN) is light at every tick, confirmed from the second.
func TestClassifierVoIPTun(t *testing.T) {
	for _, phase := range []int{0, 10, 55} {
		ticks, _ := clsRun(8*time.Second, phase, func(ms int) (int, int) {
			tx, rx := 0, 0
			if ms%20 == 3 {
				tx = 160
			}
			if ms%20 == 13 {
				rx = 160
			}
			return tx, rx
		})
		for i, k := range ticks {
			if k.tx != ClassLight || k.flow != ClassLight || k.confirmed != (i >= 1) {
				t.Fatalf("phase %d tick %d at %v: tx %v flow %v confirmed %v", phase, i, k.at, k.tx, k.flow, k.confirmed)
			}
		}
	}
}

// TestClassifierPeriodic: on for 1 s at 1 MiB/s, off for 2 s, four times:
// every evaluation is the rule's (in continuous time) within the tick
// before it, at every phase; and it is bulk through each on spell from a
// tick past its 16th KiB.
func TestClassifierPeriodic(t *testing.T) {
	const per = (1 << 20) / 1000
	x := func(ms int) int {
		if ms%3000 < 1000 {
			return per
		}
		return 0
	}
	const n = 12000
	oracle := clsOracle(n, x)
	for phase := 0; phase < 100; phase += 7 {
		ticks, _ := clsRun(n*time.Millisecond, phase, func(ms int) (int, int) { return x(ms), 0 })
		clsLagOK(t, fmt.Sprintf("phase %d", phase), ticks, oracle)
		for _, k := range ticks {
			ms := int(k.at / time.Millisecond)
			if r := ms % 3000; r >= 16+100 && r < 1000 && k.tx != ClassBulk {
				t.Fatalf("phase %d: %v at %v within an on spell", phase, k.tx, k.at)
			}
		}
	}
}

// TestClassifierUnknownFirst32K: a new flow is unknown until its first
// tick, and C2 queues the first 32 KiB it sends meanwhile as light and the
// rest as bulk; unknown is never confirmed light (no duplicate, C11).
func TestClassifierUnknownFirst32K(t *testing.T) {
	var st flowStats
	var c flowClassifier
	if q := queueClass(ClassUnknown, &st); q != ClassLight {
		t.Fatalf("a new flow is queued as %v", q)
	}
	light := 0
	for i := 0; i < 100; i++ { // 1 KiB a ms for 100 ms, before any tick
		if queueClass(ClassUnknown, &st) == ClassLight {
			light += 1 << 10
		}
		st.Add(true, 1<<10)
	}
	if light != 32<<10 {
		t.Fatalf("%d B queued as light before the first tick, want 32 KiB", light)
	}
	if q := queueClass(ClassUnknown, &st); q != ClassBulk {
		t.Fatalf("past 32 KiB an unknown flow is queued as %v", q)
	}
	tx, flow, ok := c.Tick(&st, 100*time.Millisecond)
	if tx != ClassBulk || flow != ClassBulk || ok {
		t.Fatalf("first tick: tx %v flow %v confirmed %v", tx, flow, ok)
	}
	// a quiet new flow: light at its first tick, not yet confirmed
	var st2 flowStats
	var c2 flowClassifier
	st2.Add(true, 300)
	if tx, _, ok := c2.Tick(&st2, 100*time.Millisecond); tx != ClassLight || ok {
		t.Fatalf("a quiet flow's first tick: %v confirmed %v", tx, ok)
	}
	if tx, _, ok := c2.Tick(&st2, 200*time.Millisecond); tx != ClassLight || !ok {
		t.Fatalf("a quiet flow's second tick: %v confirmed %v", tx, ok)
	}
}

// TestUserTagIPv6Slash64: a user is an IPv4 address or an IPv6 /64 (its
// privacy addresses rotate within it); the prefix can be set.
func TestUserTagIPv6Slash64(t *testing.T) {
	a := netip.MustParseAddr
	same := []string{"2001:db8:1:2::1", "2001:db8:1:2:a1b2:c3d4:e5f6:789", "2001:db8:1:2:ffff:ffff:ffff:ffff"}
	tag := UserTag(a(same[0]), 64)
	for _, s := range same[1:] {
		if UserTag(a(s), 64) != tag {
			t.Fatalf("%s and %s, one /64, two tags", same[0], s)
		}
	}
	if UserTag(a("2001:db8:1:3::1"), 64) == tag {
		t.Fatal("two /64s, one tag")
	}
	if UserTag(a(same[0]), 0) != tag {
		t.Fatal("the default prefix is not /64")
	}
	// /56: the two /64s above are one user
	if UserTag(a("2001:db8:1:3::1"), 56) != UserTag(a(same[0]), 56) {
		t.Fatal("/56: two tags within one /56")
	}
	// IPv4: each address its own user; a v4-mapped address is the v4 one
	if UserTag(a("192.0.2.1"), 64) == UserTag(a("192.0.2.2"), 64) {
		t.Fatal("two IPv4 addresses, one tag")
	}
	if UserTag(a("::ffff:192.0.2.1"), 64) != UserTag(a("192.0.2.1"), 64) {
		t.Fatal("a v4-mapped address is another user")
	}
	if UserTag(a("fe80::1%eth0"), 64) != UserTag(a("fe80::2"), 64) {
		t.Fatal("a zone makes another user")
	}
	if UserTag(netip.Addr{}, 64) != 0 || UserTag(a("192.0.2.1"), 64) == 0 {
		t.Fatal("0 is no tag: an invalid address has it, a valid one never")
	}
}

// TestClassOffAllBulk: HS2_TUNE_CLASS=0 (TuneClass 0) counts every flow as
// bulk: an HTTP-like flow, and a new one's first bytes.
func TestClassOffAllBulk(t *testing.T) {
	defer func(v int) { TuneClass = v }(TuneClass)
	TuneClass = 0
	if !ClassOff() {
		t.Fatal("TuneClass 0 and ClassOff false")
	}
	ticks, light := clsRun(3*time.Second, 0, func(ms int) (int, int) {
		if ms%1000 == 5 {
			return 600, 0
		}
		return 0, 0
	})
	for _, k := range ticks {
		if k.tx != ClassBulk || k.flow != ClassBulk || k.confirmed {
			t.Fatalf("at %v: tx %v flow %v confirmed %v", k.at, k.tx, k.flow, k.confirmed)
		}
	}
	if sum(light) != 0 {
		t.Fatalf("%d B queued as light", sum(light))
	}
}

// TestClassifierStatusLine: the status line is the document's (§3.1).
func TestClassifierStatusLine(t *testing.T) {
	got := classLine(3, 2, 1, 4, 5)
	want := "classes: light 3 / bulk 2 / unknown 1 per link; flips/min: light→bulk 4, bulk→light 5"
	if got != want {
		t.Fatalf("%q\nwant %q", got, want)
	}
	// the flips a classifier counts (class_tx)
	var st flowStats
	var c flowClassifier
	now := time.Duration(0)
	tick := func() { now += classTick; c.Tick(&st, now) }
	tick()               // light
	st.Add(true, 40<<10) // bulk
	tick()               //
	for range 21 {       // 2 s of silence
		tick()
	}
	if c.toBulk != 1 || c.toLight != 1 {
		t.Fatalf("flips light→bulk %d bulk→light %d, want 1 and 1", c.toBulk, c.toLight)
	}
}

// ---- the replay (task 3's capture of the edge) ------------------------------

// clsRec is one line of the capture: a flow's bytes up (user → abroad, the
// edge's tx) and down over the 100 ms that end at t (ms).
type clsRec struct {
	F  json.RawMessage `json:"f"`
	T  int64           `json:"t"`
	Up int64           `json:"up"`
	Dn int64           `json:"dn"`
	// Want: the labeled session's verdict for the flow's pattern ("light",
	// "bulk"; c1_labeled.jsonl.gz only)
	Want string `json:"want"`
}

// clsBin is the 100 ms tick a capture's line ends in. flowcap writes its
// lines a few ms either side of a 100 ms mark (the edge's 20 minutes: 95-99
// ms past one, the labeled session 0-4 ms past), and a line up to 5 ms past
// a mark is that mark's tick: rounded up, the session's 3301 and 3400 fell
// in one tick (884 of its lines).
func clsBin(t int64) int64 { return (t + 94) / 100 }

// clsReplayPath is the edge's capture (task 3: 20 minutes of real traffic,
// ≤ 5 MB); clsSynthPath a synthetic capture in its format (clsSynth, seed
// 1; HS2_C1_SYNTH=1 writes it anew), replayed where the real one is not in
// the tree.
const (
	clsReplayPath = "testdata/c1_replay.jsonl.gz"
	clsSynthPath  = "testdata/c1_synth.jsonl.gz"
)

func clsLoad(path string) (map[string][]clsRec, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		return nil, err
	}
	flows := map[string][]clsRec{}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r clsRec
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("%s: %q: %v", path, line, err)
		}
		if r.Up < 0 || r.Dn < 0 {
			return nil, fmt.Errorf("%s: negative bytes: %q", path, line)
		}
		k := string(r.F)
		flows[k] = append(flows[k], r)
	}
	return flows, sc.Err()
}

// clsSynth writes a synthetic capture (seeded) in the format of task 3:
// request/response, downloads and uploads, a call in the TUN, video in
// chunks, bursts and typing.
func clsSynth(path string, seed uint64) error {
	rnd := rand.New(rand.NewPCG(seed, seed^0x51c1))
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(fh)
	w := bufio.NewWriter(zw)
	id := 0
	emit := func(gen func(bin int) (up, dn int), from, bins int) {
		id++
		for b := 0; b < bins; b++ {
			up, dn := gen(b)
			if up > 0 || dn > 0 {
				fmt.Fprintf(w, "{\"f\":%d,\"t\":%d,\"up\":%d,\"dn\":%d}\n", id, int64(from+b)*100, up, dn)
			}
		}
	}
	for range 400 {
		from := rnd.IntN(3000) // bins (5 min)
		switch rnd.IntN(7) {
		case 0: // request/response
			period := 5 + rnd.IntN(45)
			req, rep := 300+rnd.IntN(1200), 2<<10+rnd.IntN(58<<10)
			emit(func(b int) (int, int) {
				switch b % period {
				case 0:
					return req, 0
				case 1:
					return 0, rep
				}
				return 0, 0
			}, from, 100+rnd.IntN(500))
		case 1, 2: // download or upload
			r := (100 << 10) + rnd.IntN(10<<20) // B/s
			n := 10 + rnd.IntN(300)
			up := rnd.IntN(2) == 0
			emit(func(b int) (int, int) {
				v := r/10 + rnd.IntN(r/20+1) - r/40
				if b == 0 {
					return 400, 0
				}
				if up {
					return v, 64
				}
				return 64, v
			}, from, n)
		case 3: // a call: 50 × 160 B a second each way
			emit(func(int) (int, int) { return 800, 800 }, from, 300+rnd.IntN(600))
		case 4: // video: a chunk every 2-4 s
			chunk, every := (500<<10)+rnd.IntN(1500<<10), 20+rnd.IntN(20)
			emit(func(b int) (int, int) {
				if r := b % every; r < 4 {
					return 0, chunk / 4
				}
				return 0, 0
			}, from, 300+rnd.IntN(600))
		case 5: // a burst, then quiet
			size := (10 << 10) + rnd.IntN(90<<10)
			emit(func(b int) (int, int) {
				switch b {
				case 0:
					return 500, 0
				case 1, 2:
					return 0, size / 2
				}
				if b%30 == 0 {
					return 100, 100
				}
				return 0, 0
			}, from, 200+rnd.IntN(200))
		case 6: // typing
			emit(func(int) (int, int) {
				if rnd.IntN(3) == 0 {
					return 40 + rnd.IntN(200), 40 + rnd.IntN(400)
				}
				return 0, 0
			}, from, 200+rnd.IntN(800))
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return fh.Close()
}

// clsRef is the rule on one direction's 100 ms bins, the plain way (full
// history): the class at each bin and whether its windows alone were light.
func clsRef(x []int64) (cls []Class, lightNow []bool) {
	cls = make([]Class, len(x))
	lightNow = make([]bool, len(x))
	c := ClassUnknown
	for b := range x {
		var b200, b2s int64
		for k := max(0, b-1); k <= b; k++ {
			b200 += x[k]
		}
		for k := max(0, b-19); k <= b; k++ {
			b2s += x[k]
		}
		lightNow[b] = b2s < classLightSpan && b200 < classLightBurst
		switch {
		case c == ClassBulk && b2s >= classQuietBytes:
		case lightNow[b]:
			c = ClassLight
		default:
			c = ClassBulk
		}
		cls[b] = c
	}
	return cls, lightNow
}

// TestClassifierReplay replays the edge's capture (task 3), or the
// synthetic one where that is not in the tree, each flow from either end
// (the edge sends up, the exit down), ticked on the capture's 100 ms bins:
//
//   - each verdict (class_tx, class_flow, confirmed) is the rule's on the
//     bins, computed the plain way;
//   - no bulk episode of a flow puts more than 32 KiB and one tick of its
//     bytes in the latency lane;
//   - it reports how much bulk traffic went in the latency lane, and how
//     often the rule's hold (bulk until quiet: under 8 KiB over 2 s) kept light traffic
//     in the bulk lane.
func TestClassifierReplay(t *testing.T) {
	if os.Getenv("HS2_C1_SYNTH") == "1" {
		if err := clsSynth(clsSynthPath, 1); err != nil {
			t.Fatal(err)
		}
	}
	path := clsReplayPath
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		path = clsSynthPath
		t.Logf("%s is not in the tree: replaying the synthetic %s", clsReplayPath, clsSynthPath)
	}
	flows, err := clsLoad(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) == 0 {
		t.Fatalf("%s: no flows", path)
	}
	ids := make([]string, 0, len(flows))
	for k := range flows {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	var (
		nFlows, ticks                   int
		bulkBytes, bulkLight, laneBytes int64
		bulkTicks, heldTicks            int
		worst                           int64
	)
	for _, id := range ids {
		recs := flows[id]
		sort.SliceStable(recs, func(i, j int) bool { return recs[i].T < recs[j].T })
		bin := clsBin
		b0 := bin(recs[0].T)
		n := int(bin(recs[len(recs)-1].T)-b0) + 21 // its last byte, and 2 s of silence and a tick
		up, dn := make([]int64, n), make([]int64, n)
		for _, r := range recs {
			b := bin(r.T) - b0
			up[b] += r.Up
			dn[b] += r.Dn
		}
		nFlows++
		for _, edge := range []bool{true, false} {
			tx, rx := up, dn
			if !edge {
				tx, rx = dn, up
			}
			refTx, lightNow := clsRef(tx)
			refRx, _ := clsRef(rx)
			var st flowStats
			var c flowClassifier
			cur := ClassUnknown
			run := 0
			epLight, epFirst := int64(0), int64(0)
			for b := 0; b < n; b++ {
				// the bin's bytes go out under the verdict in force (the
				// previous tick's; unknown before the first)
				inLight := tx[b] > 0 && queueClass(cur, &st) == ClassLight
				if inLight {
					laneBytes += tx[b]
				}
				st.Add(true, int(tx[b]))
				st.Add(false, int(rx[b]))
				ct, cf, ok := c.Tick(&st, time.Duration(b0+int64(b))*classTick)
				ticks++
				wantF := refTx[b]
				if refRx[b] == ClassBulk {
					wantF = ClassBulk
				}
				if refTx[b] == ClassLight {
					run++
				} else {
					run = 0
				}
				if ct != refTx[b] || cf != wantF || ok != (run >= 2) {
					t.Fatalf("flow %s (edge %v) bin %d: got tx %v flow %v confirmed %v, the rule's %v %v %v",
						id, edge, b, ct, cf, ok, refTx[b], wantF, run >= 2)
				}
				// the rule's bulk episodes: the latency lane's share of them
				if refTx[b] == ClassBulk {
					if b == 0 || refTx[b-1] != ClassBulk {
						epLight, epFirst = 0, tx[b]
					}
					bulkBytes += tx[b]
					bulkTicks++
					if lightNow[b] {
						heldTicks++
					}
					if inLight {
						bulkLight += tx[b]
						epLight += tx[b]
						worst = max(worst, epLight)
						if epLight > 32<<10+epFirst {
							t.Fatalf("flow %s (edge %v) bin %d: %d B of a bulk episode in the latency lane, more than 32 KiB and its first tick (%d B)",
								id, edge, b, epLight, epFirst)
						}
					}
				}
				cur = ct
			}
		}
	}
	share := func(a, b int64) float64 {
		if b == 0 {
			return 0
		}
		return 100 * float64(a) / float64(b)
	}
	t.Logf("%s: %d flows, %d ticks (both ends)", filepath.Base(path), nFlows, ticks)
	t.Logf("bulk traffic in the latency lane: %.2f%% of bulk bytes (%d of %d B), %.2f%% of the lane's bytes; most of one episode %d B",
		share(bulkLight, bulkBytes), bulkLight, bulkBytes, share(bulkLight, laneBytes), worst)
	t.Logf("held bulk (light windows, bulk until under 8 KiB over 2 s): %.2f%% of bulk ticks (%d of %d)",
		share(int64(heldTicks), int64(bulkTicks)), heldTicks, bulkTicks)
}

// clsLabeledPath is the labeled session (task 3, step 3b;
// docs/architecture/wip/r1/labeled): known patterns played one after
// another through the live tunnel, captured by flowcap on the edge, each
// line with the verdict its pattern should get ("want", one for the whole
// flow).
const clsLabeledPath = "testdata/c1_labeled.jsonl.gz"

// clsSession is the session's patterns in the order session.sh plays them,
// each with its label and where its data goes (up, dn; both: a light
// pattern's two directions).
var clsSession = []struct {
	name, want string
	dir        string
}{
	{"bulkdn", "bulk", "dn"}, {"bulkup", "bulk", "up"}, {"http", "light", "both"},
	{"burst", "bulk", "dn"}, {"onoff", "bulk", "dn"}, {"voip", "light", "both"},
}

// TestClassifierLabeled replays the labeled session through the classifier
// (each flow from both ends, on the capture's 100 ms ticks) and reports,
// per pattern, how often the verdict differs from the label, and which part
// of the rule (§3.1, decision T1) makes it differ:
//
//   - quiet: a bulk flow's data moved less than 8 KiB over 2 s (between the
//     bursts of "burst"): the rule says light (T1), the label bulk;
//   - below: a bulk flow under the light thresholds (a transfer's first
//     ticks, before 16 KiB in 200 ms or 64 KiB in 2 s): the rule says light;
//   - held: a light flow kept bulk until it is quiet;
//   - over: a light flow past the thresholds;
//   - other: none of these (none may be).
//
// It asserts the rule's bounds: each verdict is the rule's (computed the
// plain way); no bulk episode of a flow puts more than 32 KiB and one tick
// of its bytes in the latency lane; and no flow is bulk more than one tick
// into a quiet spell (T1).
func TestClassifierLabeled(t *testing.T) {
	flows, err := clsLoad(clsLabeledPath)
	if err != nil {
		t.Fatal(err)
	}
	type lflow struct {
		id    string
		recs  []clsRec
		start int64
	}
	var all []lflow
	for id, recs := range flows {
		sort.SliceStable(recs, func(i, j int) bool { return recs[i].T < recs[j].T })
		all = append(all, lflow{id, recs, recs[0].T})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].start < all[j].start })
	// a pattern's connections start together, 5 s or more after the
	// pattern before's last
	var groups [][]lflow
	for i, f := range all {
		if i == 0 || f.start-all[i-1].start > 3000 {
			groups = append(groups, nil)
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], f)
	}
	if len(groups) != len(clsSession) {
		t.Fatalf("%d groups of flows, the session plays %d patterns", len(groups), len(clsSession))
	}
	type tally struct {
		flows, ticks                         int
		flowErr, txErr, txTicks              int
		quiet, below, held, over, other      int
		laneBulk, bulkBytes, worst, worstLim int64
	}
	for gi, g := range groups {
		p := clsSession[gi]
		var tl tally
		for _, f := range g {
			var up, dn int64
			for _, r := range f.recs {
				if r.Want != p.want {
					t.Fatalf("pattern %d (%s): flow %s labeled %q", gi, p.name, f.id, r.Want)
				}
				up += r.Up
				dn += r.Dn
			}
			if p.dir == "dn" && dn <= up || p.dir == "up" && up <= dn {
				t.Fatalf("pattern %d (%s): flow %s moved %d B up, %d B down", gi, p.name, f.id, up, dn)
			}
			b0 := clsBin(f.recs[0].T)
			span := int(clsBin(f.recs[len(f.recs)-1].T)-b0) + 1 // its lines
			n := span + 21                                     // and 2 s of silence and a tick
			x := map[string][]int64{"up": make([]int64, n), "dn": make([]int64, n)}
			for _, r := range f.recs {
				b := clsBin(r.T) - b0
				x["up"][b] += r.Up
				x["dn"][b] += r.Dn
			}
			tl.flows++
			// the rule on each direction, the plain way
			ref, light := map[string][]Class{}, map[string][]bool{}
			quiet := map[string][]bool{}
			for _, d := range []string{"up", "dn"} {
				ref[d], light[d] = clsRef(x[d])
				quiet[d] = make([]bool, n)
				var w int64
				for b := range n {
					w += x[d][b]
					if b >= 20 {
						w -= x[d][b-20]
					}
					quiet[d][b] = w < classQuietBytes
				}
			}
			for _, tx := range []string{"up", "dn"} {
				rx := map[string]string{"up": "dn", "dn": "up"}[tx]
				var st flowStats
				var c flowClassifier
				cur := ClassUnknown
				run := 0
				epLight, epFirst := int64(0), int64(0)
				for b := range n {
					inLight := x[tx][b] > 0 && queueClass(cur, &st) == ClassLight
					st.Add(true, int(x[tx][b]))
					st.Add(false, int(x[rx][b]))
					ct, cf, ok := c.Tick(&st, time.Duration(b0+int64(b))*classTick)
					wantF := ref[tx][b]
					if ref[rx][b] == ClassBulk {
						wantF = ClassBulk
					}
					if ref[tx][b] == ClassLight {
						run++
					} else {
						run = 0
					}
					if ct != ref[tx][b] || cf != wantF || ok != (run >= 2) {
						t.Fatalf("%s flow %s (tx %s) tick %d: got tx %v flow %v confirmed %v, the rule's %v %v %v",
							p.name, f.id, tx, b, ct, cf, ok, ref[tx][b], wantF, run >= 2)
					}
					// no flow bulk more than a tick into a quiet spell
					if ct == ClassBulk && b > 0 && quiet[tx][b] && quiet[tx][b-1] {
						t.Fatalf("%s flow %s (tx %s) tick %d: bulk, quiet since the tick before", p.name, f.id, tx, b)
					}
					// the latency lane's share of the rule's bulk episodes
					if ref[tx][b] == ClassBulk {
						if b == 0 || ref[tx][b-1] != ClassBulk {
							epLight, epFirst = 0, x[tx][b]
						}
						tl.bulkBytes += x[tx][b]
						if inLight {
							tl.laneBulk += x[tx][b]
							epLight += x[tx][b]
							if epLight > tl.worst {
								tl.worst, tl.worstLim = epLight, 32<<10+epFirst
							}
							if epLight > 32<<10+epFirst {
								t.Fatalf("%s flow %s (tx %s) tick %d: %d B of a bulk episode in the latency lane, past 32 KiB and its first tick (%d B)",
									p.name, f.id, tx, b, epLight, epFirst)
							}
						}
					}
					cur = ct
					if b >= span {
						continue // (after its last line: the bounds only)
					}
					want := ClassLight
					if p.want == "bulk" {
						want = ClassBulk
					}
					// class_tx of the data's direction (both, for a light
					// pattern) against the label
					if p.dir == tx || p.dir == "both" {
						tl.txTicks++
						if ct != want {
							tl.txErr++
						}
					}
					if tx != "up" {
						continue // (class_flow once a flow)
					}
					tl.ticks++
					if cf == want {
						continue
					}
					tl.flowErr++
					data := p.dir
					switch {
					case want == ClassBulk && data != "both" && quiet[data][b]:
						tl.quiet++
					case want == ClassBulk && data != "both" && light[data][b]:
						tl.below++
					case want == ClassLight && light["up"][b] && light["dn"][b]:
						tl.held++
					case want == ClassLight:
						tl.over++
					default:
						tl.other++
					}
				}
			}
		}
		pct := func(a, b int) float64 {
			if b == 0 {
				return 0
			}
			return 100 * float64(a) / float64(b)
		}
		t.Logf("%-6s want %-5s flows %d ticks %4d | class_flow off %5.2f%% (quiet %d, below %d, held %d, over %d, other %d) | class_tx off %5.2f%% | latency lane %d of %d bulk B, worst episode %d (bound %d)",
			p.name, p.want, tl.flows, tl.ticks, pct(tl.flowErr, tl.ticks), tl.quiet, tl.below, tl.held, tl.over, tl.other,
			pct(tl.txErr, tl.txTicks), tl.laneBulk, tl.bulkBytes, tl.worst, tl.worstLim)
		if tl.other != 0 {
			t.Fatalf("%s: %d verdicts off the label that the rule does not explain", p.name, tl.other)
		}
	}
}

// BenchmarkClassifierTick: one flow's tick (one per op), at 10 and 10000
// flows that each moved some bytes since their last; the cost per flow
// must not grow with the flows.
func BenchmarkClassifierTick(b *testing.B) {
	for _, n := range []int{10, 10000} {
		b.Run(fmt.Sprintf("flows=%d", n), func(b *testing.B) {
			st := make([]flowStats, n)
			cs := make([]flowClassifier, n)
			now := time.Duration(0)
			i := 0
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if i == n {
					i = 0
					now += classTick
				}
				st[i].Add(i&1 == 0, 1000+i&0xff)
				cs[i].Tick(&st[i], now)
				i++
			}
		})
	}
}
