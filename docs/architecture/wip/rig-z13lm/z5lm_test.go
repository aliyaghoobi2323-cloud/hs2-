package engine

// Red team rig (scratch): the whole edge as RunIran builds it -- a real
// LinkManager (health loop, loss and stuck rules, autopilot, drain), its
// links dialled with NewMTCPDialer, each with its control, stats and info
// streams -- and a real exit (RunKharej) across the TUN path emulator
// (z5emu_test.go). Z5_LM_USERS users each open one connection through the
// edge's user listener and download (or, Z5_LM_UP=1, upload) as fast as
// they can for Z5_LM_DUR_S s. It reports each user's Mbit/s per second,
// when its connection ended (and why), and the edge's log lines about
// degraded, draining and closed links.
//
// z13 additions (Phase Z5 full matrix):
//   - Z5_LM_ROLE=exit | edge: the exit (RunKharej and the panel) and the
//     edge (emulator, LinkManager, users) in two processes of one network
//     namespace (rig/run2.sh), so an edge of one version can be measured
//     against an exit of another. The exit listens on 0.0.0.0:Z5_LM_PORT and
//     ends when the file Z5_LM_STOP appears, printing its kept log lines.
//   - Z5_LM_BAD_MS: the newest link, once the pool is up, gets that much more
//     round trip (half each way, after the shared bottleneck) (gap 3).
//   - Z5_LM_CUT_AT: the first user's link is black-holed (every packet both
//     ways dropped, the connection left open) that many s after the users
//     started (gap 2).
//   - Z5_LM_SHORT_MS: from Z5_LM_SHORT_FROM to Z5_LM_SHORT_TO s a short user
//     every that many ms: it sends 'I' and a 200 B request, reads the 2 KiB
//     reply (time to it, or the failure, is recorded) and closes.
//   - each user connection's link is told by its port (no longer by the
//     order of the picks), and the links' ids, edge ports and rules are
//     reported.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

type z5LMUser struct {
	Mbit1s  []float64 `json:"mbit1s"`
	EndedAt float64   `json:"endedAtS"` // -1: ran to the end
	Err     string    `json:"err,omitempty"`
	Link    int       `json:"link"` // z13: the link id it was given (-1: unknown)
}

// z12: an interactive user that arrived while the big download ran
// (Z5_LM_INTS): which link it was given, and its replies' latency.
type z5LMInt struct {
	At      float64            `json:"atS"`       // when it connected, s from tU
	SameDl  bool               `json:"sameDl"`    // on the download's link
	Links   int                `json:"links"`     // links serving as it connected
	DlPress bool               `json:"dlPressed"` // the download's link was pressed as it connected
	Lat     map[string]float64 `json:"lat"`       // ms: n, p50, p90, p99, max
	Link    int                `json:"link"`      // z13: its link id
	Bad     bool               `json:"bad"`       // z13: on the link made worse (Z5_LM_BAD_MS)
}

// z13: a short user (Z5_LM_SHORT_MS)
type z5LMShort struct {
	At   float64 `json:"atS"`
	Link int     `json:"link"`
	Cut  bool    `json:"cut"`  // it was given the black-holed link (after the cut)
	TTFB float64 `json:"ttfbMs"` // -1: failed
	Err  string  `json:"err,omitempty"`
}

// z13: a link of the pool
type z5LMLink struct {
	ID      int    `json:"id"`
	Port    int    `json:"port"`
	Bad     bool   `json:"bad,omitempty"`
	Cut     bool   `json:"cut,omitempty"`
	Serving bool   `json:"serving"`
}

type z5LMRes struct {
	Scen   string            `json:"scen"`
	Ver    string            `json:"ver"`
	Rep    string            `json:"rep"`
	Env    map[string]string `json:"env"`
	Users  []z5LMUser        `json:"users"`
	Lines  []string          `json:"lines"`
	Links  int               `json:"linksAtEnd"`
	T0     string            `json:"t0,omitempty"` // wall clock when the users started
	Ints   []z5LMInt         `json:"ints,omitempty"`
	Shorts []z5LMShort       `json:"shorts,omitempty"`
	Pool0  []z5LMLink        `json:"pool0,omitempty"` // links as the users started
	Pool1  []z5LMLink        `json:"pool1,omitempty"` // links at the end
	BadID  int               `json:"badId"`
	CutID  int               `json:"cutId"`
	CutAt  float64           `json:"cutAtS"`
	CutDr  [2]int64          `json:"cutDrops"` // up, down
}

// idOfLocked is a link's id (-2: not in the pool); the caller holds lm.mu.
func idOfLocked(lm *LinkManager, l Link) int {
	for _, ml := range lm.links {
		if ml.link == l {
			return ml.id
		}
	}
	return -2
}

// z5LinkPort is a link's edge-side TCP port (0: unknown).
func z5LinkPort(l Link) int {
	if m, ok := l.(*mtcpLink); ok && m.tls != nil {
		if tc := m.tls.TCPConn(); tc != nil {
			if a, ok := tc.LocalAddr().(*net.TCPAddr); ok {
				return a.Port
			}
		}
	}
	return 0
}

func TestZ5LM(t *testing.T) {
	scen := os.Getenv("Z5_SCEN")
	if scen == "" {
		t.Skip("Z5_SCEN not set")
	}
	z5LoUp()
	role := os.Getenv("Z5_LM_ROLE")
	mtu := int(z5Env("Z5_MTU", 1500))
	rtt := time.Duration(z5Env("Z5_RTT_MS", 100) * float64(time.Millisecond))
	qms := z5Env("Z5_QMS", 50)
	loss := z5Env("Z5_LOSS", 0)
	path := func(mbit float64) z5Path {
		p := z5Path{Rate: mbit * 1e6 / 8, Delay: rtt / 2, Loss: loss}
		p.Queue = int(p.Rate * qms / 1000)
		if p.Queue < 3*mtu {
			p.Queue = 3 * mtu
		}
		return p
	}
	var emu *z5Emu
	if role != "exit" {
		var err error
		if emu, err = newZ5Emu(path(z5Env("Z5_UP_MBIT", 100)), path(z5Env("Z5_DN_MBIT", 100)), mtu); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var logMu sync.Mutex
	var lines []string
	t0 := time.Now()
	keep := func(side string) func(string, ...any) {
		return func(f string, a ...any) {
			l := fmt.Sprintf(f, a...)
			if os.Getenv("Z5_LM_LOGALL") != "" || strings.Contains(l, "degraded") || strings.Contains(l, "drain") ||
				strings.Contains(l, "closed") || strings.Contains(l, "lossy") || strings.Contains(l, "stuck") ||
				strings.Contains(l, "go to the other links") || strings.Contains(l, "on par with the other links") ||
				strings.Contains(l, "window") || strings.Contains(l, "fill the path") || strings.Contains(l, "link down") || strings.Contains(l, "link up") ||
				strings.Contains(l, "nothing received") || strings.Contains(l, "rescue") || strings.Contains(l, "moved") {
				logMu.Lock()
				lines = append(lines, fmt.Sprintf("%6.1fs %s: %s", time.Since(t0).Seconds(), side, l))
				logMu.Unlock()
			}
		}
	}
	key := bytes.Repeat([]byte{0x5a}, 32)

	exitPort := 0
	if role != "edge" {
		// the panel: 'D' writes forever, 'U' reads forever, 'I' answers 200 B with 2 KiB
		pln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer pln.Close()
		go func() {
			for {
				c, err := pln.Accept()
				if err != nil {
					return
				}
				go func(c net.Conn) {
					defer c.Close()
					var hd [1]byte
					if _, err := io.ReadFull(c, hd[:]); err != nil {
						return
					}
					buf := make([]byte, 32<<10)
					if hd[0] == 'U' {
						io.CopyBuffer(io.Discard, c, buf)
						return
					}
					if hd[0] == 22 { // z13: a short user's TLS-hello-shaped request: one record, 2 KiB reply, close
						var rh [4]byte
						if _, err := io.ReadFull(c, rh[:]); err != nil {
							return
						}
						body := make([]byte, int(rh[2])<<8|int(rh[3]))
						if _, err := io.ReadFull(c, body); err != nil {
							return
						}
						c.Write(make([]byte, 2048))
						return
					}
					if hd[0] == 'I' { // z12: 200 B request, 2 KiB reply
						req, rep := make([]byte, 200), make([]byte, 2048)
						for {
							if _, err := io.ReadFull(c, req); err != nil {
								return
							}
							if _, err := c.Write(rep); err != nil {
								return
							}
						}
					}
					for {
						if _, err := c.Write(buf); err != nil {
							return
						}
					}
				}(c)
			}
		}()
		srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
		addr := net.IP(z5Local[:]).String() + ":0"
		if role == "exit" {
			addr = "0.0.0.0:" + strconv.Itoa(int(z5Env("Z5_LM_PORT", 47001)))
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		exitPort = ln.Addr().(*net.TCPAddr).Port
		go RunKharej(ctx, KharejConfig{Listener: ln, Server: srv, Panel: pln.Addr().String(), Log: keep("exit")})
		if role == "exit" {
			// until the edge is done (Z5_LM_STOP appears), or a safe while
			stop := os.Getenv("Z5_LM_STOP")
			end := time.Now().Add(time.Duration(z5Env("Z5_LM_DUR_S", 130)+z5Env("Z5_LM_SETTLE_S", 5)+180) * time.Second)
			for time.Now().Before(end) {
				if stop != "" {
					if _, err := os.Stat(stop); err == nil {
						break
					}
				}
				time.Sleep(200 * time.Millisecond)
			}
			logMu.Lock()
			j, _ := json.Marshal(map[string]any{"exitLines": lines, "ver": os.Getenv("Z5_VER")})
			logMu.Unlock()
			fmt.Println("Z5EXIT " + string(j))
			return
		}
	} else {
		exitPort = int(z5Env("Z5_LM_PORT", 47001))
	}

	d := NewMTCPDialer(net.IP(z5ExitIP[:]).String()+":"+strconv.Itoa(exitPort), "lab.example.com", key, "")
	lmMin := int(z5Env("Z5_LM_MIN", 2))
	lm := NewLinkManager(d, lmMin, int(z5Env("Z5_LM_MAX", 8)), 50, keep("edge"))
	lm.gateInfo = true
	mine := peerInfo{MaxLinks: lm.max, Caps: capPortTags}
	lm.OnLink = func(l Link) {
		go openControl(ctx, l, keep("edge"))
		go openStats(ctx, l, keep("edge"))
		go openInfo(ctx, l, mine)
	}
	go lm.Run(ctx)
	userLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer userLn.Close()
	// the link each user connection was given, by the user's port
	var pickMu sync.Mutex
	pickBy := map[int]Link{}
	pickCond := sync.NewCond(&pickMu)
	go func() {
		for {
			c, err := userLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				up := c.RemoteAddr().(*net.TCPAddr).Port
				st, link, release, ok := openStreamOn(ctx, lm, false, 0)
				pickMu.Lock()
				pickBy[up] = link // nil: none
				pickCond.Broadcast()
				pickMu.Unlock()
				if !ok {
					c.Close()
					return
				}
				relayUser(ctx, c, lm, 0, st, link, release)
			}(c)
		}
	}()
	// linkOf waits (up to 10 s) for the link given to the user connection c
	linkOf := func(c net.Conn) Link {
		p := c.LocalAddr().(*net.TCPAddr).Port
		end := time.Now().Add(10 * time.Second)
		pickMu.Lock()
		defer pickMu.Unlock()
		for {
			if l, ok := pickBy[p]; ok {
				return l
			}
			if time.Now().After(end) {
				return nil
			}
			go func() { time.Sleep(100 * time.Millisecond); pickMu.Lock(); pickCond.Broadcast(); pickMu.Unlock() }()
			pickCond.Wait()
		}
	}
	serving := func() int {
		lm.mu.RLock()
		defer lm.mu.RUnlock()
		n := 0
		for _, ml := range lm.links {
			if ml.serving() {
				n++
			}
		}
		return n
	}
	pressedOf := func(l Link) bool {
		lm.mu.RLock()
		defer lm.mu.RUnlock()
		for _, ml := range lm.links {
			if ml.link == l {
				return ml.pressed
			}
		}
		return false
	}
	idOf := func(l Link) int {
		if l == nil {
			return -1
		}
		lm.mu.RLock()
		defer lm.mu.RUnlock()
		for _, ml := range lm.links {
			if ml.link == l {
				return ml.id
			}
		}
		return -2 // gone from the pool
	}
	var badID, cutID = -1, -1
	var badPort int
	var cutPortA atomic.Int64
	pool := func() []z5LMLink {
		lm.mu.RLock()
		defer lm.mu.RUnlock()
		var out []z5LMLink
		for _, ml := range lm.links {
			p := z5LinkPort(ml.link)
			out = append(out, z5LMLink{ID: ml.id, Port: p, Bad: badPort != 0 && p == badPort, Cut: cutPortA.Load() != 0 && int64(p) == cutPortA.Load(), Serving: ml.serving()})
		}
		return out
	}
	// wait for the pool to come up (z13: to Z5_LM_MIN links when a link is to be made worse)
	want := 1
	if z5Env("Z5_LM_BAD_MS", 0) > 0 {
		want = lmMin
	}
	for i := 0; i < 300 && lm.Stats().Links < want; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if bm := z5Env("Z5_LM_BAD_MS", 0); bm > 0 {
		lm.mu.RLock()
		var newest *managedLink
		for _, ml := range lm.links {
			if newest == nil || ml.id > newest.id {
				newest = ml
			}
		}
		lm.mu.RUnlock()
		if newest != nil {
			badID, badPort = newest.id, z5LinkPort(newest.link)
			if badPort != 0 {
				z5SetFlow(badPort, z5Flow{Extra: time.Duration(bm / 2 * float64(time.Millisecond))})
			}
		}
	}
	// z13: Z5_LM_PRE_MS: a short user (as below, not recorded) every that many ms through
	// the settle, so that the links carry some traffic to be judged on (Z5_LM_BAD_MS)
	settle := time.Now().Add(time.Duration(z5Env("Z5_LM_SETTLE_S", 5) * float64(time.Second)))
	if pm := z5Env("Z5_LM_PRE_MS", 0); pm > 0 {
		for time.Now().Before(settle) {
			go func() {
				c, err := net.Dial("tcp", userLn.Addr().String())
				if err != nil {
					return
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				req, rep := make([]byte, 205), make([]byte, 2048)
				copy(req, []byte{22, 3, 1, 0, 200, 1, 0, 0, 196, 3, 3})
				if _, err := c.Write(req); err == nil {
					io.ReadFull(c, rep)
				}
			}()
			time.Sleep(time.Duration(pm * float64(time.Millisecond)))
		}
	}
	time.Sleep(time.Until(settle))

	nU := int(z5Env("Z5_LM_USERS", 1))
	dur := z5Env("Z5_LM_DUR_S", 130)
	up := z5Env("Z5_LM_UP", 0) > 0
	users := make([]z5LMUser, nU)
	bins := make([]z5Bins, nU)
	userLinks := make([]Link, nU)
	pool0 := pool()
	tU := time.Now()
	end := tU.Add(time.Duration(dur * float64(time.Second)))
	var wg sync.WaitGroup
	for i := 0; i < nU; i++ {
		users[i].EndedAt, users[i].Link = -1, -1
		c, err := net.Dial("tcp", userLn.Addr().String())
		if err != nil {
			users[i].Err, users[i].EndedAt = err.Error(), 0
			continue
		}
		if up {
			c.Write([]byte{'U'})
		} else {
			c.Write([]byte{'D'})
		}
		userLinks[i] = linkOf(c)
		users[i].Link = idOf(userLinks[i])
		wg.Add(1)
		go func(i int, c net.Conn) {
			defer wg.Done()
			defer c.Close()
			c.SetDeadline(end)
			buf := make([]byte, 32<<10)
			if up {
				for time.Now().Before(end) {
					n, err := c.Write(buf)
					bins[i].add(tU, n)
					if err != nil {
						if time.Now().Before(end.Add(-time.Second)) {
							users[i].EndedAt, users[i].Err = time.Since(tU).Seconds(), err.Error()
						}
						return
					}
				}
				return
			}
			for {
				n, err := c.Read(buf)
				bins[i].add(tU, n)
				if err != nil {
					if time.Now().Before(end.Add(-time.Second)) {
						users[i].EndedAt, users[i].Err = time.Since(tU).Seconds(), err.Error()
					}
					return
				}
			}
		}(i, c)
	}
	var dlLink Link
	if nU > 0 {
		dlLink = userLinks[0]
	}
	// z13: gap 2, the first user's link black-holed at Z5_LM_CUT_AT
	var cutAt float64
	// (Z5_LM_CUT_IDLE=1: instead the serving link of the lowest id that carries none of the
	// big users)
	if ca := z5Env("Z5_LM_CUT_AT", 0); ca > 0 && dlLink != nil {
		go func() {
			time.Sleep(time.Until(tU.Add(time.Duration(ca * float64(time.Second)))))
			victim := dlLink
			if z5Env("Z5_LM_CUT_IDLE", 0) > 0 {
				victim = nil
				lm.mu.RLock()
				for _, ml := range lm.links {
					busy := false
					for _, ul := range userLinks {
						if ul == ml.link {
							busy = true
						}
					}
					if !busy && ml.serving() && (victim == nil || ml.id < idOfLocked(lm, victim)) {
						victim = ml.link
					}
				}
				lm.mu.RUnlock()
				if victim == nil {
					return
				}
			}
			p := z5LinkPort(victim)
			if p == 0 {
				return
			}
			cutID = idOf(victim)
			cutAt = math.Round(time.Since(tU).Seconds()*10) / 10
			cutPortA.Store(int64(p))
			z5SetFlow(p, z5Flow{Cut: true})
			keep("rig")("z13: link %d (port %d) black-holed both ways", cutID, p)
		}()
	}
	// z13: short users
	var shortMu sync.Mutex
	var shorts []z5LMShort
	var swg sync.WaitGroup
	if ms := z5Env("Z5_LM_SHORT_MS", 0); ms > 0 {
		from, to := z5Env("Z5_LM_SHORT_FROM", 0), z5Env("Z5_LM_SHORT_TO", dur)
		swg.Add(1)
		go func() {
			defer swg.Done()
			for k := 0; ; k++ {
				at := tU.Add(time.Duration((from + float64(k)*ms/1000) * float64(time.Second)))
				if at.After(tU.Add(time.Duration(to * float64(time.Second)))) {
					return
				}
				time.Sleep(time.Until(at))
				swg.Add(1)
				go func() {
					defer swg.Done()
					s := time.Now()
					r := z5LMShort{At: math.Round(time.Since(tU).Seconds()*100) / 100, Link: -1, TTFB: -1}
					c, err := net.Dial("tcp", userLn.Addr().String())
					if err != nil {
						r.Err = err.Error()
					} else {
						c.SetDeadline(s.Add(30 * time.Second))
						// one TLS record carrying a client_hello-shaped message (what
						// rescue.go may send again on another link), 205 B in all
						req, rep := make([]byte, 205), make([]byte, 2048)
						copy(req, []byte{22, 3, 1, 0, 200, 1, 0, 0, 196, 3, 3})
						if _, err := c.Write(req); err != nil {
							r.Err = err.Error()
						} else if _, err := io.ReadFull(c, rep); err != nil {
							r.Err = err.Error()
						} else {
							r.TTFB = math.Round(float64(time.Since(s))/1e5) / 10
						}
						l := linkOf(c)
						r.Link = idOf(l)
						r.Cut = cutPortA.Load() != 0 && l != nil && int64(z5LinkPort(l)) == cutPortA.Load()
						c.Close()
					}
					shortMu.Lock()
					shorts = append(shorts, r)
					shortMu.Unlock()
				}()
			}
		}()
	}
	// z12: Z5_LM_INTS interactive users, the first Z5_LM_INT_AT s after the
	// download started, then one each Z5_LM_INT_EVERY s, to the end
	nI := int(z5Env("Z5_LM_INTS", 0))
	ints := make([]z5LMInt, nI)
	var iwg sync.WaitGroup
	for k := 0; k < nI; k++ {
		at := tU.Add(time.Duration((z5Env("Z5_LM_INT_AT", 15) + float64(k)*z5Env("Z5_LM_INT_EVERY", 15)) * float64(time.Second)))
		if !at.Before(end) {
			ints = ints[:k]
			break
		}
		time.Sleep(time.Until(at))
		ints[k].At = math.Round(time.Since(tU).Seconds()*10) / 10
		ints[k].Links = serving()
		ints[k].DlPress = dlLink != nil && pressedOf(dlLink)
		ints[k].Link = -1
		c, err := net.Dial("tcp", userLn.Addr().String())
		if err != nil {
			continue
		}
		c.Write([]byte{'I'})
		l := linkOf(c)
		ints[k].SameDl = l != nil && l == dlLink
		ints[k].Link = idOf(l)
		ints[k].Bad = badPort != 0 && l != nil && z5LinkPort(l) == badPort
		iwg.Add(1)
		go func(k int, c net.Conn) {
			defer iwg.Done()
			defer c.Close()
			c.SetDeadline(end)
			var lat z5Lat
			req, rep := make([]byte, 200), make([]byte, 2048)
			tk := time.NewTicker(100 * time.Millisecond)
			defer tk.Stop()
			for time.Now().Before(end.Add(-time.Second)) {
				<-tk.C
				s := time.Now()
				if _, err := c.Write(req); err != nil {
					break
				}
				if _, err := io.ReadFull(c, rep); err != nil {
					break
				}
				lat.add(time.Since(s))
			}
			ints[k].Lat = z5Pct(lat.v)
		}(k, c)
	}
	iwg.Wait()
	wg.Wait()
	swg.Wait()
	for i := range users {
		users[i].Mbit1s = bins[i].secs(int(dur))
		users[i].EndedAt = math.Round(users[i].EndedAt*10) / 10
	}
	res := z5LMRes{Scen: scen, Ver: os.Getenv("Z5_VER"), Rep: os.Getenv("Z5_REP"), Users: users, Links: lm.Stats().Links, Env: map[string]string{}, Ints: ints,
		Shorts: shorts, Pool0: pool0, Pool1: pool(), BadID: badID, CutID: cutID, CutAt: cutAt}
	if emu != nil {
		res.CutDr = [2]int64{emu.up.cutDrops.Load(), emu.dn.cutDrops.Load()}
	}
	res.T0 = tU.UTC().Format(time.RFC3339Nano)
	logMu.Lock()
	res.Lines = append(res.Lines, lines...)
	logMu.Unlock()
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "Z5_") {
			p := strings.SplitN(kv, "=", 2)
			res.Env[p[0]] = p[1]
		}
	}
	j, _ := json.Marshal(res)
	t.Log(string(j))
}
