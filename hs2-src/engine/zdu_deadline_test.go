package engine

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

func zt(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestZDUDeadlineTable(t *testing.T) {
	tehran := time.FixedZone("IRST", 3*3600+1800) // +03:30: local HH:31 = UTC HH:01
	kathmandu := time.FixedZone("NPT", 5*3600+2700)
	cases := []struct {
		name     string
		now      time.Time
		override time.Duration
		want     string // "" = refused
	}{
		// refusal window HH:00:00 - HH:03:59.999 UTC
		{"HH:00:00 refused", zt("2026-11-19T10:00:00Z"), 0, ""},
		{"HH:00:30 refused", zt("2026-11-19T10:00:30Z"), 0, ""},
		{"HH:01:30 refused", zt("2026-11-19T10:01:30Z"), 0, ""},
		{"HH:03:59 refused", zt("2026-11-19T10:03:59Z"), 0, ""},
		{"HH:03:59.999 refused", zt("2026-11-19T10:03:59.999999999Z"), 0, ""},
		{"override does not lift refusal", zt("2026-11-19T10:02:00Z"), 10 * time.Minute, ""},
		// first allowed minute: next HH:03 is 59 min away
		{"HH:04:00", zt("2026-11-19T10:04:00Z"), 0, "2026-11-19T11:03:00Z"},
		{"HH:30", zt("2026-11-19T10:30:00Z"), 0, "2026-11-19T11:03:00Z"},
		// minimum distance bound: exactly 5 min away is still this hour
		{"HH:58:00 exactly 5 min", zt("2026-11-19T10:58:00Z"), 0, "2026-11-19T11:03:00Z"},
		{"HH:58:00.001 next hour", zt("2026-11-19T10:58:00.001Z"), 0, "2026-11-19T12:03:00Z"},
		{"HH:59:59 next hour", zt("2026-11-19T10:59:59Z"), 0, "2026-11-19T12:03:00Z"},
		// day, month, year and leap-day rollover
		{"23:59 day rollover", zt("2026-11-19T23:59:00Z"), 0, "2026-11-20T01:03:00Z"},
		{"23:30 to midnight+3", zt("2026-11-19T23:30:00Z"), 0, "2026-11-20T00:03:00Z"},
		{"year end", zt("2026-12-31T23:59:30Z"), 0, "2027-01-01T01:03:00Z"},
		{"leap day", zt("2028-02-28T23:10:00Z"), 0, "2028-02-29T00:03:00Z"},
		// local zones never matter: all in UTC
		{"Tehran local 14:31 = UTC 11:01 refused", time.Date(2026, 11, 19, 14, 31, 0, 0, tehran), 0, ""},
		{"Tehran local 14:00 = UTC 10:30", time.Date(2026, 11, 19, 14, 0, 0, 0, tehran), 0, "2026-11-19T11:03:00Z"},
		{"Tehran local 03:33 = UTC 00:03 refused", time.Date(2026, 11, 20, 3, 33, 0, 0, tehran), 0, ""},
		{"Kathmandu +05:45 local 16:00 = UTC 10:15", time.Date(2026, 11, 19, 16, 0, 0, 0, kathmandu), 0, "2026-11-19T11:03:00Z"},
		// key override, and the 65 min cap on it
		{"override 20m", zt("2026-11-19T10:30:00Z"), 20 * time.Minute, "2026-11-19T10:50:00Z"},
		{"override exactly cap", zt("2026-11-19T10:30:00Z"), 65 * time.Minute, "2026-11-19T11:35:00Z"},
		{"override over cap", zt("2026-11-19T10:30:00Z"), 3 * time.Hour, "2026-11-19T11:35:00Z"},
		{"override 1s", zt("2026-11-19T10:30:00Z"), time.Second, "2026-11-19T10:30:01Z"},
	}
	for _, c := range cases {
		got, err := ZDUDeadline(c.now, c.override)
		if c.want == "" {
			if !errors.Is(err, ErrZDUWindow) || !got.IsZero() {
				t.Errorf("%s: got %v, %v; want refusal", c.name, got, err)
			}
			if !ZDURefused(c.now) {
				t.Errorf("%s: ZDURefused false", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected %v", c.name, err)
			continue
		}
		if w := zt(c.want); !got.Equal(w) || got.Location() != time.UTC {
			t.Errorf("%s: got %v want %v (UTC)", c.name, got, w)
		}
	}
}

// TestZDUDeadlineDST: real DST zones (when tzdata is present) give the same
// UTC deadline on both sides of a transition and inside the repeated hour.
func TestZDUDeadlineDST(t *testing.T) {
	for _, zone := range []string{"America/New_York", "Europe/Berlin", "Australia/Lord_Howe", "Asia/Tehran"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Logf("%s: no tzdata (%v), skipped", zone, err)
			continue
		}
		// every 7 minutes over 3 days around the US/EU fall-back and spring-forward dates
		for _, startS := range []string{"2026-03-07T00:00:00Z", "2026-03-28T00:00:00Z", "2026-10-24T00:00:00Z", "2026-10-31T00:00:00Z", "2026-04-04T00:00:00Z"} {
			start := zt(startS)
			for u := start; u.Before(start.Add(72 * time.Hour)); u = u.Add(7*time.Minute + 13*time.Second) {
				a, ea := ZDUDeadline(u, 0)
				b, eb := ZDUDeadline(u.In(loc), 0)
				if !a.Equal(b) || (ea == nil) != (eb == nil) {
					t.Fatalf("%s %v: UTC %v/%v local %v/%v", zone, u, a, ea, b, eb)
				}
			}
		}
	}
}

// TestZDUDeadlineInvariants: every minute of a day, every second at the bounds.
func TestZDUDeadlineInvariants(t *testing.T) {
	start := zt("2026-11-19T00:00:00Z")
	for u := start; u.Before(start.Add(25 * time.Hour)); u = u.Add(time.Second) {
		d, err := ZDUDeadline(u, 0)
		m := u.Minute()
		if m < 4 {
			if err == nil {
				t.Fatalf("%v: not refused", u)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%v: %v", u, err)
		}
		if d.Minute() != 3 || d.Second() != 0 || d.Nanosecond() != 0 {
			t.Fatalf("%v: deadline %v not HH:03:00", u, d)
		}
		if gap := d.Sub(u); gap < ZDUDrainMin || gap > ZDUDrainCap {
			t.Fatalf("%v: gap %v outside [%v, %v]", u, gap, ZDUDrainMin, ZDUDrainCap)
		}
	}
}

func TestParseZDUDrain(t *testing.T) {
	ok := map[string]time.Duration{"": 0, "0": 0, " 20m ": 20 * time.Minute, "90s": 90 * time.Second, "1200": 20 * time.Minute, "2h": 2 * time.Hour}
	for in, want := range ok {
		if got, err := ParseZDUDrain(in); err != nil || got != want {
			t.Errorf("%q: %v %v want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"-5", "-1m", "abc", "5x"} {
		if _, err := ParseZDUDrain(in); err == nil {
			t.Errorf("%q: no error", in)
		}
	}
}

// TestZDUReadyHooks: Ready runs once the local side is up, with the peer
// down (no link ever comes up): Iran after its user port is bound, Kharej
// (direct) with its listener accepting. nil Ready changes nothing.
func TestZDUReadyHooks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := bytes.Repeat([]byte{0x5a}, 32)
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	defer dead.Close()
	port := freePort(t)
	iranUp := make(chan bool, 1)
	go RunIran(ctx, IranConfig{
		Dialer: NewMTCPDialer(dead.Addr().String(), "lab.example.com", key, ""),
		Min:    1, Max: 1, PerLink: 50, ListenIP: "127.0.0.1", Ports: []string{port},
		Ready: func() {
			c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
			if err == nil {
				c.Close()
			}
			iranUp <- err == nil
		},
	})
	select {
	case ok := <-iranUp:
		if !ok {
			t.Fatal("iran Ready before the user port was bound")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("iran Ready never ran with the peer down")
	}

	raw, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	khUp := make(chan struct{}, 1)
	go RunKharej(ctx, KharejConfig{Listener: raw, Server: srv, Panel: "127.0.0.1:1", Ready: func() { khUp <- struct{}{} }})
	select {
	case <-khUp:
	case <-time.After(10 * time.Second):
		t.Fatal("kharej Ready never ran")
	}
	c, err := net.DialTimeout("tcp", raw.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("kharej listener: %v", err)
	}
	c.Close()
}

// Review minor: the end-of-hour guard and the configurable storm minute.
func TestZDURefusedSpanAndStormMin(t *testing.T) {
	span := 80 * time.Second
	for _, c := range []struct {
		at   string
		want bool
	}{
		{"2026-11-19T10:30:00Z", false},
		{"2026-11-19T10:58:00Z", false},
		{"2026-11-19T10:58:41Z", true}, // + 80 s = 11:00:01
		{"2026-11-19T10:59:30Z", true},
		{"2026-11-19T11:02:00Z", true},
		{"2026-11-19T11:04:00Z", false},
	} {
		if got := ZDURefusedSpan(zt(c.at), span); got != c.want {
			t.Errorf("%s: refused %v, want %v", c.at, got, c.want)
		}
	}
	was := zduStormMin
	t.Cleanup(func() { zduStormMin = was })
	zduStormMin = 31 // window HH:30:00-HH:33:59, anchor HH:33
	for _, c := range []struct {
		at   string
		want bool
	}{
		{"2026-11-19T10:01:00Z", false},
		{"2026-11-19T10:29:59Z", false},
		{"2026-11-19T10:30:00Z", true},
		{"2026-11-19T10:33:59Z", true},
		{"2026-11-19T10:34:00Z", false},
	} {
		if got := ZDURefused(zt(c.at)); got != c.want {
			t.Errorf("storm 31 %s: refused %v, want %v", c.at, got, c.want)
		}
	}
	if d, err := ZDUDeadline(zt("2026-11-19T10:40:00Z"), 0); err != nil || !d.Equal(zt("2026-11-19T11:33:00Z")) {
		t.Fatalf("storm 31 deadline %v %v, want 11:33", d, err)
	}
	zduStormMin = 0 // window wraps: HH:59-HH:02
	if !ZDURefused(zt("2026-11-19T10:59:10Z")) || !ZDURefused(zt("2026-11-19T11:02:59Z")) || ZDURefused(zt("2026-11-19T11:03:00Z")) {
		t.Fatal("storm 0: wrapped window wrong")
	}
	if d, _ := ZDUDeadline(zt("2026-11-19T10:30:00Z"), 0); !d.Equal(zt("2026-11-19T11:02:00Z")) {
		t.Fatalf("storm 0 deadline %v, want 11:02", d)
	}
}
