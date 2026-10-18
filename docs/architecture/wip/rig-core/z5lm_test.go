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
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

type z5LMUser struct {
	Mbit1s  []float64 `json:"mbit1s"`
	EndedAt float64   `json:"endedAtS"` // -1: ran to the end
	Err     string    `json:"err,omitempty"`
}

type z5LMRes struct {
	Scen  string            `json:"scen"`
	Ver   string            `json:"ver"`
	Rep   string            `json:"rep"`
	Env   map[string]string `json:"env"`
	Users []z5LMUser        `json:"users"`
	Lines []string          `json:"lines"`
	Links int               `json:"linksAtEnd"`
	T0    string            `json:"t0,omitempty"` // wall clock when the users started
}

func TestZ5LM(t *testing.T) {
	scen := os.Getenv("Z5_SCEN")
	if scen == "" {
		t.Skip("Z5_SCEN not set")
	}
	z5LoUp()
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
	if _, err := newZ5Emu(path(z5Env("Z5_UP_MBIT", 100)), path(z5Env("Z5_DN_MBIT", 100)), mtu); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// the panel: 'D' writes forever, 'U' reads forever
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
				for {
					if _, err := c.Write(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	var logMu sync.Mutex
	var lines []string
	t0 := time.Now()
	keep := func(side string) func(string, ...any) {
		return func(f string, a ...any) {
			l := fmt.Sprintf(f, a...)
			if os.Getenv("Z5_LM_LOGALL") != "" || strings.Contains(l, "degraded") || strings.Contains(l, "drain") ||
				strings.Contains(l, "closed") || strings.Contains(l, "lossy") || strings.Contains(l, "stuck") ||
				strings.Contains(l, "go to the other links") || strings.Contains(l, "on par with the other links") ||
				strings.Contains(l, "window") || strings.Contains(l, "fill the path") || strings.Contains(l, "link down") || strings.Contains(l, "link up") {
				logMu.Lock()
				lines = append(lines, fmt.Sprintf("%6.1fs %s: %s", time.Since(t0).Seconds(), side, l))
				logMu.Unlock()
			}
		}
	}

	key := bytes.Repeat([]byte{0x5a}, 32)
	srv := &tlscarrier.Server{SharedKey: key, Cert: testCert(t), BackendAddr: "127.0.0.1:1"}
	ln, err := net.Listen("tcp", net.IP(z5Local[:]).String()+":0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go RunKharej(ctx, KharejConfig{Listener: ln, Server: srv, Panel: pln.Addr().String(), Log: keep("exit")})
	d := NewMTCPDialer(net.IP(z5ExitIP[:]).String()+":"+strconv.Itoa(port), "lab.example.com", key, "")
	lm := NewLinkManager(d, int(z5Env("Z5_LM_MIN", 2)), int(z5Env("Z5_LM_MAX", 8)), 50, keep("edge"))
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
	go func() {
		for {
			c, err := userLn.Accept()
			if err != nil {
				return
			}
			go serveUserTCP(ctx, c, lm, 0)
		}
	}()
	// wait for the pool to come up
	for i := 0; i < 300 && lm.Stats().Links < 1; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(time.Duration(z5Env("Z5_LM_SETTLE_S", 5) * float64(time.Second)))

	nU := int(z5Env("Z5_LM_USERS", 1))
	dur := z5Env("Z5_LM_DUR_S", 130)
	up := z5Env("Z5_LM_UP", 0) > 0
	users := make([]z5LMUser, nU)
	bins := make([]z5Bins, nU)
	tU := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < nU; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			users[i].EndedAt = -1
			c, err := net.Dial("tcp", userLn.Addr().String())
			if err != nil {
				users[i].Err, users[i].EndedAt = err.Error(), 0
				return
			}
			defer c.Close()
			end := tU.Add(time.Duration(dur * float64(time.Second)))
			c.SetDeadline(end)
			buf := make([]byte, 32<<10)
			if up {
				c.Write([]byte{'U'})
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
			c.Write([]byte{'D'})
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
		}(i)
	}
	wg.Wait()
	for i := range users {
		users[i].Mbit1s = bins[i].secs(int(dur))
		users[i].EndedAt = math.Round(users[i].EndedAt*10) / 10
	}
	res := z5LMRes{Scen: scen, Ver: os.Getenv("Z5_VER"), Rep: os.Getenv("Z5_REP"), Users: users, Links: lm.Stats().Links, Env: map[string]string{}}
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
