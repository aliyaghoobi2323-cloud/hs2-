// patgen: known traffic patterns for the labeled C1 session (R1 task 3, step 3b).
//
//	patgen server -listen HOST:PORT[,HOST:PORT]
//	patgen client -addr HOST:PORT -pat NAME -dur 90s [-n K]
//
// The client plays the user and the server plays the edge's far side, so
// "dn" (server -> client) is what flowcap counts as dn on an accepted socket.
// Every connection starts with a 4-byte header {'P', pattern, secs u16 BE}.
// Patterns (one connection each, -n of them in parallel):
//
//	bulkdn  the server writes 64 KiB buffers for dur
//	bulkup  the client writes 64 KiB buffers for dur
//	http    every 1 s: a 600 B request, an 8 KiB reply (one keep-alive connection)
//	burst   the server sends 20 KiB every 3 s
//	onoff   the server sends 1 MiB/s for 1 s, then nothing for 2 s
//	voip    160 B each way every 20 ms
//
// The client prints one JSON line per connection: pattern, start and end in
// Unix milliseconds. Standard library only; no addresses are printed.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

var pats = map[string]byte{"bulkdn": 1, "bulkup": 2, "http": 3, "burst": 4, "onoff": 5, "voip": 6}

const (
	reqSize   = 600
	replySize = 8 << 10
	burstSize = 20 << 10
	voipSize  = 160
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: patgen server -listen A[,B] | patgen client -addr A -pat NAME -dur D [-n K]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "server":
		fs := flag.NewFlagSet("server", flag.ExitOnError)
		listen := fs.String("listen", "", "HOST:PORT[,HOST:PORT]")
		fs.Parse(os.Args[2:])
		server(*listen)
	case "client":
		fs := flag.NewFlagSet("client", flag.ExitOnError)
		addr := fs.String("addr", "", "HOST:PORT")
		pat := fs.String("pat", "", "bulkdn|bulkup|http|burst|onoff|voip")
		dur := fs.Duration("dur", 90*time.Second, "pattern length")
		n := fs.Int("n", 1, "parallel connections")
		fs.Parse(os.Args[2:])
		client(*addr, *pat, *dur, *n)
	default:
		os.Exit(2)
	}
}

func server(listen string) {
	var wg sync.WaitGroup
	for _, a := range strings.Split(listen, ",") {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			log.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				c, err := ln.Accept()
				if err != nil {
					log.Print(err)
					return
				}
				go serve(c)
			}
		}()
	}
	wg.Wait()
}

func serve(c net.Conn) {
	defer c.Close()
	var h [4]byte
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(c, h[:]); err != nil || h[0] != 'P' {
		return
	}
	c.SetReadDeadline(time.Time{})
	dur := time.Duration(binary.BigEndian.Uint16(h[2:])) * time.Second
	start := time.Now()
	end := start.Add(dur)
	switch h[1] {
	case 1: // bulkdn
		buf := make([]byte, 64<<10)
		for time.Now().Before(end) {
			if _, err := c.Write(buf); err != nil {
				return
			}
		}
	case 2: // bulkup
		io.Copy(io.Discard, c)
	case 3: // http
		req := make([]byte, reqSize)
		reply := make([]byte, replySize)
		for {
			if _, err := io.ReadFull(c, req); err != nil {
				return
			}
			if _, err := c.Write(reply); err != nil {
				return
			}
		}
	case 4: // burst
		buf := make([]byte, burstSize)
		for k := 0; ; k++ {
			t := start.Add(time.Duration(k) * 3 * time.Second)
			if !t.Before(end) {
				break
			}
			time.Sleep(time.Until(t))
			if _, err := c.Write(buf); err != nil {
				return
			}
		}
	case 5: // onoff: 32 KiB every 31.25 ms for 1 s (1 MiB/s), then 2 s off
		buf := make([]byte, 32<<10)
		for k := 0; ; k++ {
			t := start.Add(time.Duration(k) * 3 * time.Second)
			if !t.Before(end) {
				break
			}
			for i := 0; i < 32; i++ {
				time.Sleep(time.Until(t.Add(time.Duration(i) * 31250 * time.Microsecond)))
				if _, err := c.Write(buf); err != nil {
					return
				}
			}
		}
	case 6: // voip
		go io.Copy(io.Discard, c)
		buf := make([]byte, voipSize)
		for k := 0; ; k++ {
			t := start.Add(time.Duration(k) * 20 * time.Millisecond)
			if !t.Before(end) {
				break
			}
			time.Sleep(time.Until(t))
			if _, err := c.Write(buf); err != nil {
				return
			}
		}
	}
	// let the far side read what is in flight before the close
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.Copy(io.Discard, c)
}

type rec struct {
	Pat   string `json:"pat"`
	Conn  int    `json:"conn"`
	Start int64  `json:"start_ms"`
	End   int64  `json:"end_ms"`
	Bytes int64  `json:"bytes_rx"`
	Err   string `json:"err,omitempty"`
}

func client(addr, pat string, dur time.Duration, n int) {
	p, ok := pats[pat]
	if !ok {
		log.Fatalf("pattern %q", pat)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	enc := json.NewEncoder(os.Stdout)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := rec{Pat: pat, Conn: i, Start: time.Now().UnixMilli()}
			got, err := play(addr, p, dur)
			r.End, r.Bytes = time.Now().UnixMilli(), got
			if err != nil {
				r.Err = err.Error()
			}
			mu.Lock()
			enc.Encode(r)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
}

type counter struct{ n int64 }

func (c *counter) Write(b []byte) (int, error) { c.n += int64(len(b)); return len(b), nil }

func play(addr string, p byte, dur time.Duration) (int64, error) {
	c, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	var h [4]byte
	h[0], h[1] = 'P', p
	binary.BigEndian.PutUint16(h[2:], uint16(dur/time.Second))
	if _, err := c.Write(h[:]); err != nil {
		return 0, err
	}
	start := time.Now()
	end := start.Add(dur)
	cnt := &counter{}
	switch p {
	case 1, 4, 5: // the server talks; read to EOF
		c.SetReadDeadline(end.Add(20 * time.Second))
		_, err = io.Copy(cnt, c)
	case 2: // bulkup
		buf := make([]byte, 64<<10)
		for time.Now().Before(end) {
			if _, err = c.Write(buf); err != nil {
				return 0, err
			}
		}
		c.(*net.TCPConn).CloseWrite()
		c.SetReadDeadline(time.Now().Add(20 * time.Second))
		_, err = io.Copy(cnt, c)
	case 3: // http
		req := make([]byte, reqSize)
		reply := make([]byte, replySize)
		for k := 0; ; k++ {
			t := start.Add(time.Duration(k) * time.Second)
			if !t.Before(end) {
				break
			}
			time.Sleep(time.Until(t))
			if _, err = c.Write(req); err != nil {
				return cnt.n, err
			}
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			var m int
			m, err = io.ReadFull(c, reply)
			cnt.n += int64(m)
			if err != nil {
				return cnt.n, err
			}
		}
		c.(*net.TCPConn).CloseWrite()
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, err = io.Copy(cnt, c)
	case 6: // voip
		done := make(chan error, 1)
		go func() {
			c.SetReadDeadline(end.Add(20 * time.Second))
			_, e := io.Copy(cnt, c)
			done <- e
		}()
		buf := make([]byte, voipSize)
		for k := 0; ; k++ {
			t := start.Add(time.Duration(k) * 20 * time.Millisecond)
			if !t.Before(end) {
				break
			}
			time.Sleep(time.Until(t))
			if _, err = c.Write(buf); err != nil {
				return cnt.n, err
			}
		}
		c.(*net.TCPConn).CloseWrite()
		err = <-done
	}
	return cnt.n, err
}
