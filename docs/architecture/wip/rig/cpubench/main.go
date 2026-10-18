// cpubench: CPU per GiB of the smux receive path, base (smuxb, c9825f2's smux)
// against the need prototype (smuxn), over TCP loopback: one process, one
// session, -streams streams each moving -mib MiB, the receiver reading as fast
// as it can. Variants: B (adaptive 2 MiB / 8 MiB), N (need: 16 MiB ceiling,
// 2 MiB need, 128 MiB pool, RoundTrip 100 ms: every window past 2 MiB goes
// through fitNeed and the pool), N0 (the same with RoundTrip 0: the hooks
// only, no window past 2 MiB). One JSON line per run: process CPU (user+sys)
// over the run, per GiB moved.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	smuxb "example.com/smuxb"
	smuxn "example.com/smuxn"
	smuxp "example.com/smuxp"
)

type sess interface {
	open() (io.ReadWriteCloser, error)
	accept() (io.ReadWriteCloser, error)
	close()
}

type bS struct{ s *smuxb.Session }

func (x bS) open() (io.ReadWriteCloser, error)   { return x.s.OpenStream() }
func (x bS) accept() (io.ReadWriteCloser, error) { return x.s.AcceptStream() }
func (x bS) close()                              { x.s.Close() }

type pS struct{ s *smuxp.Session }

func (x pS) open() (io.ReadWriteCloser, error)   { return x.s.OpenStream() }
func (x pS) accept() (io.ReadWriteCloser, error) { return x.s.AcceptStream() }
func (x pS) close()                              { x.s.Close() }

type nS struct{ s *smuxn.Session }

func (x nS) open() (io.ReadWriteCloser, error)   { return x.s.OpenStream() }
func (x nS) accept() (io.ReadWriteCloser, error) { return x.s.AcceptStream() }
func (x nS) close()                              { x.s.Close() }

func mk(v string, c net.Conn, client bool, pool *smuxn.WindowPool) sess {
	if v == "B" {
		cfg := smuxb.DefaultConfig()
		cfg.Version = 2
		cfg.MaxFrameSize = 16 << 10
		cfg.MaxReceiveBuffer = 8 << 20
		cfg.MaxStreamBuffer = 2 << 20
		cfg.MinStreamBuffer = 64 << 10
		cfg.StreamLagTarget = 128 << 10
		var s *smuxb.Session
		var err error
		if client {
			s, err = smuxb.Client(c, cfg)
		} else {
			s, err = smuxb.Server(c, cfg)
		}
		if err != nil {
			panic(err)
		}
		return bS{s}
	}
	if v == "P" {
		cfg := smuxp.DefaultConfig()
		cfg.Version = 2
		cfg.MaxFrameSize = 16 << 10
		cfg.MaxReceiveBuffer = 8 << 20
		cfg.MaxStreamBuffer = 16 << 20
		cfg.MinStreamBuffer = 64 << 10
		cfg.StreamLagTarget = 128 << 10
		cfg.NeedStreamBuffer = 2 << 20
		cfg.WindowPool = smuxp.NewWindowPool(128 << 20)
		cfg.GrowByTrial = true
		cfg.RoundTrip = func() time.Duration { return 100 * time.Millisecond }
		var s *smuxp.Session
		var err error
		if client {
			s, err = smuxp.Client(c, cfg)
		} else {
			s, err = smuxp.Server(c, cfg)
		}
		if err != nil {
			panic(err)
		}
		return pS{s}
	}
	cfg := smuxn.DefaultConfig()
	cfg.Version = 2
	cfg.MaxFrameSize = 16 << 10
	cfg.MaxReceiveBuffer = 8 << 20
	cfg.MaxStreamBuffer = 16 << 20
	cfg.MinStreamBuffer = 64 << 10
	cfg.StreamLagTarget = 128 << 10
	cfg.NeedStreamBuffer = 2 << 20
	cfg.WindowPool = pool
	rtt := 100 * time.Millisecond
	if v == "N0" {
		rtt = 0
	}
	cfg.RoundTrip = func() time.Duration { return rtt }
	var s *smuxn.Session
	var err error
	if client {
		s, err = smuxn.Client(c, cfg)
	} else {
		s, err = smuxn.Server(c, cfg)
	}
	if err != nil {
		panic(err)
	}
	return nS{s}
}

func cpu() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func main() {
	v := flag.String("v", "B", "B | N | N0 | P")
	streams := flag.Int("streams", 1, "streams")
	mib := flag.Int("mib", 1024, "MiB per stream")
	label := flag.String("label", "", "label")
	flag.Parse()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	ch := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); ch <- c }()
	cc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		panic(err)
	}
	sc := <-ch
	pool := smuxn.NewWindowPool(128 << 20)
	cli := mk(*v, cc, true, pool)
	srv := mk(*v, sc, false, smuxn.NewWindowPool(128<<20))
	total := int64(*streams) * int64(*mib) << 20
	go func() { // the sender: each accepted stream gets mib MiB after its 1-byte request
		for {
			st, err := srv.accept()
			if err != nil {
				return
			}
			go func(st io.ReadWriteCloser) {
				b := make([]byte, 1)
				if _, err := io.ReadFull(st, b); err != nil {
					return
				}
				buf := make([]byte, 32<<10)
				for n := int64(0); n < int64(*mib)<<20; n += int64(len(buf)) {
					if _, err := st.Write(buf); err != nil {
						return
					}
				}
			}(st)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	var extMax int
	stop := make(chan struct{})
	if x, ok := cli.(nS); ok {
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(time.Millisecond):
				}
				if e := x.s.Extra(); e > extMax {
					extMax = e
				}
			}
		}()
	}
	c0, t0 := cpu(), time.Now()
	var wg sync.WaitGroup
	for i := 0; i < *streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := cli.open()
			if err != nil {
				panic(err)
			}
			st.Write([]byte{1})
			buf := make([]byte, 32<<10)
			want := int64(*mib) << 20
			for got := int64(0); got < want; {
				n, err := st.Read(buf)
				got += int64(n)
				if err != nil {
					panic(err)
				}
			}
			st.Close()
		}()
	}
	wg.Wait()
	c1, t1 := cpu(), time.Now()
	close(stop)
	gib := float64(total) / (1 << 30)
	out, _ := json.Marshal(map[string]any{"label": *label, "v": *v, "streams": *streams, "gib": gib,
		"cpu_s_per_gib": (c1 - c0).Seconds() / gib, "wall_s": t1.Sub(t0).Seconds(), "gbit": gib * 8 * 1.073741824 / t1.Sub(t0).Seconds(),
		"pool_left_mib": pool.Free() >> 20, "ext_max_mib": extMax >> 20})
	fmt.Println(string(out))
	cli.close()
	srv.close()
	os.Exit(0)
}
