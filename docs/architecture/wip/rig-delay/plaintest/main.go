// plaintest: a 96 MiB download over a plain smux pair on loopback TCP, many times;
// upstream v1.5.24 against hs2 c9825f2's smux: how often it stalls or ends short.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	oldsmux "example.com/oldsmux"
	upsmux "example.com/upsmux"
)

type sess interface {
	open() (io.ReadWriteCloser, func(time.Time), error)
	accept() (io.ReadWriteCloser, error)
	close()
}
type U struct{ s *upsmux.Session }

func (x U) open() (io.ReadWriteCloser, func(time.Time), error) {
	st, err := x.s.OpenStream()
	if err != nil {
		return nil, nil, err
	}
	return st, func(t time.Time) { st.SetReadDeadline(t) }, nil
}
func (x U) accept() (io.ReadWriteCloser, error) { return x.s.AcceptStream() }
func (x U) close()                              { x.s.Close() }

type O struct{ s *oldsmux.Session }

func (x O) open() (io.ReadWriteCloser, func(time.Time), error) {
	st, err := x.s.OpenStream()
	if err != nil {
		return nil, nil, err
	}
	return st, func(t time.Time) { st.SetReadDeadline(t) }, nil
}
func (x O) accept() (io.ReadWriteCloser, error) { return x.s.AcceptStream() }
func (x O) close()                              { x.s.Close() }

var lastPort int

func pair(v string) (sess, sess) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	a, _ := net.Dial("tcp", ln.Addr().String())
	b := <-acc
	lastPort = a.LocalAddr().(*net.TCPAddr).Port
	if v == "up" {
		c := upsmux.DefaultConfig()
		c.Version = 2
		c.MaxFrameSize = 16 << 10
		c.MaxReceiveBuffer = 8 << 20
		c.MaxStreamBuffer = 2 << 20
		x, _ := upsmux.Client(a, c)
		y, _ := upsmux.Server(b, c)
		return U{x}, U{y}
	}
	c := oldsmux.DefaultConfig()
	c.Version = 2
	c.MaxFrameSize = 16 << 10
	c.MaxReceiveBuffer = 8 << 20
	c.MaxStreamBuffer = 2 << 20
	c.MinStreamBuffer = 64 << 10
	c.StreamLagTarget = 128 << 10
	x, _ := oldsmux.Client(a, c)
	y, _ := oldsmux.Server(b, c)
	return O{x}, O{y}
}

func main() {
	v := flag.String("v", "up", "up | old")
	n := flag.Int("n", 40, "runs")
	flag.Parse()
	fails := 0
	for i := 0; i < *n; i++ {
		edge, exit := pair(*v)
		go func() {
			st, err := exit.accept()
			if err != nil {
				return
			}
			buf := make([]byte, 32<<10)
			for k := 0; k < 96<<20; k += len(buf) {
				if _, err := st.Write(buf); err != nil {
					return
				}
			}
			st.Close()
		}()
		st, dl, err := edge.open()
		if err != nil {
			panic(err)
		}
		st.Write([]byte{0})
		dl(time.Now().Add(10 * time.Second))
		got, err := io.Copy(io.Discard, st)
		if got != 96<<20 {
			fails++
			fmt.Printf("%s run %d: read %d, %v\n", *v, i, got, err)
			hp := fmt.Sprintf(":%04X", lastPort)
			data, _ := os.ReadFile("/proc/net/tcp")
			for _, l := range strings.Split(string(data), "\n") {
				if strings.Contains(l, hp) {
					fmt.Println("  /proc/net/tcp:", strings.Join(strings.Fields(l)[1:5], " "))
				}
			}
		}
		edge.close()
		exit.close()
	}
	fmt.Printf("%s: %d of %d runs short or stalled\n", *v, fails, *n)
}
