package tlscarrier

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowConn delays every read by half the target RTT (a far target).
type slowConn struct {
	net.Conn
	d time.Duration
}

func (c *slowConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		time.Sleep(c.d)
	}
	return n, err
}

// burstLab: a reality listener over a real TLS target reached with rtt.
func burstLab(t *testing.T, rtt time.Duration, down bool) *realityLab {
	tgt := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "site") }))
	t.Cleanup(tgt.Close)
	addr := tgt.Listener.Addr().String()
	if down {
		tgt.Close()
	}
	l := newRealityLab(t, addr, nil)
	l.setup.Lock()
	l.srv.DialTarget = func(ctx context.Context, a string) (net.Conn, error) {
		time.Sleep(rtt / 2) // SYN / SYN-ACK
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", a)
		if err != nil {
			return nil, err
		}
		time.Sleep(rtt / 2)
		return &slowConn{Conn: c, d: rtt}, nil
	}
	l.setup.Unlock()
	return l
}

type burstRes struct {
	ok      int
	errs    map[string]int
	p50, mx time.Duration
}

// burst dials n reality links at once and counts the outcomes.
func burst(t *testing.T, l *realityLab, n int) burstRes {
	var mu sync.Mutex
	res := burstRes{errs: map[string]int{}}
	var lat []time.Duration
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			t0 := time.Now()
			car, err := l.dial(l.dprof)
			d := time.Since(t0)
			mu.Lock()
			defer mu.Unlock()
			lat = append(lat, d)
			if err != nil {
				msg := err.Error()
				if i := strings.LastIndex(msg, ": "); i > 0 && strings.Contains(msg, "127.0.0.1") {
					msg = msg[i+2:]
				}
				res.errs[msg]++
				return
			}
			res.ok++
			car.Close()
		}()
	}
	close(start)
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	res.p50, res.mx = lat[len(lat)/2], lat[len(lat)-1]
	return res
}

// 150 links dialing at once (a tunnel start) must all get in, none waiting
// for the auth timeout, with a near (1 ms) and a far (70 ms) target. Short
// form by default (-race friendly); HS2_BURST=N for the full lab count.
func TestRealityBurstDial(t *testing.T) {
	n := 40
	if s := os.Getenv("HS2_BURST"); s != "" {
		n, _ = strconv.Atoi(s)
	}
	for _, rtt := range []time.Duration{time.Millisecond, 70 * time.Millisecond} {
		l := burstLab(t, rtt, false)
		r0 := RealityStatsNow().Rejected
		t0 := time.Now()
		r := burst(t, l, n)
		rj := RealityStatsNow().Rejected
		var rs []string
		for k, v := range rj {
			if d := v - r0[k]; d > 0 {
				rs = append(rs, fmt.Sprintf("%s=%d", k, d))
			}
		}
		t.Logf("rtt %v n %d: ok %d errs %v rejects %v p50 %v max %v wall %v", rtt, n, r.ok, r.errs, rs, r.p50, r.mx, time.Since(t0))
		if r.ok != n {
			t.Errorf("rtt %v: %d/%d links failed: %v %v", rtt, n-r.ok, n, r.errs, rs)
		}
		if r.mx > 5*time.Second {
			t.Errorf("rtt %v: a link took %v (auth-timeout class hang)", rtt, r.mx)
		}
	}
}

// A target that refuses: an authenticated reality link fails at once
// (target_down; a "both" dialer then uses tls), no 10 s hang.
func TestRealityTargetDownFast(t *testing.T) {
	l := burstLab(t, time.Millisecond, true)
	t0 := time.Now()
	_, err := l.dial(l.dprof)
	if err == nil {
		t.Fatal("reality link with the target down")
	}
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("target down took %v: %v", d, err)
	}
}

// A target that connects but never answers its ServerHello: the link is
// served with the defaults once authTargetBudget is spent (not at the
// dialer's 10 s auth deadline); a target whose connect hangs fails the link
// at the budget (target_slow), so a "both" dialer falls back in time.
func TestRealitySlowTargetBounded(t *testing.T) {
	l := newRealityLab(t, "127.0.0.1:9", nil)
	l.setup.Lock()
	l.srv.DialTarget = func(ctx context.Context, a string) (net.Conn, error) {
		c1, c2 := net.Pipe()
		go io.Copy(io.Discard, c2) // reads the hello, never answers
		t.Cleanup(func() { c2.Close() })
		return c1, nil
	}
	l.setup.Unlock()
	late := RealityStatsNow().HelloLate
	t0 := time.Now()
	car, err := l.dial(l.dprof)
	if err != nil {
		t.Fatalf("silent target: %v after %v", err, time.Since(t0))
	}
	car.Close()
	if d := time.Since(t0); d < authTargetBudget-500*time.Millisecond || d > authTargetBudget+2*time.Second {
		t.Fatalf("silent target: link took %v", d)
	}
	if RealityStatsNow().HelloLate != late+1 {
		t.Fatal("hello_late not counted")
	}

	l2 := newRealityLab(t, "127.0.0.1:9", nil)
	l2.setup.Lock()
	l2.srv.DialTarget = func(ctx context.Context, a string) (net.Conn, error) {
		<-ctx.Done() // a blackholed SYN
		return nil, ctx.Err()
	}
	l2.setup.Unlock()
	s0 := rejected("target_slow")
	t0 = time.Now()
	if _, err := l2.dial(l2.dprof); err == nil {
		t.Fatal("blackholed target: link up")
	}
	if d := time.Since(t0); d > authTargetBudget+2*time.Second {
		t.Fatalf("blackholed target: failure took %v", d)
	}
	if rejected("target_slow") != s0+1 {
		t.Fatal("target_slow not counted")
	}
}
