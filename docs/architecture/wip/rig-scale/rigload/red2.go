package main

import (
	"io"
	"net"
	"time"
)

// The R1 red team's second pass (item 5, exhaustion): two more user kinds.
//
//	hold   slow loris: N connections to the bulk port, connection k opened
//	       at StartS + k/Wave s (Wave a second, 100 by default), each
//	       sends nothing at all and is held open until the flow's stop; one
//	       the far end closes early counts as a short end (and is not opened
//	       again)
//	churn  N workers, each opening a connection to the echo port, sending
//	       the header and 32 bytes, reading the 32 back and closing, over
//	       and over until the flow's stop: thousands of connections a
//	       second through the tunnel (each counts in conns; a failed one in
//	       dial errors or short ends)

func (c *client) hold(us *ustate, k int, f Flow) {
	stop := c.stopOf(f)
	addr := c.target(us, false)
	wave := f.Wave
	if wave <= 0 {
		wave = 100
	}
	sleepUntil(c.at(f.StartS + float64(k)/float64(wave)))
	conn, err := c.dial(us, addr)
	if err != nil {
		us.dialErrs.Add(1)
		c.err("dial hold %s: %v", addr, err)
		return
	}
	us.conns.Add(1)
	defer conn.Close()
	conn.SetReadDeadline(stop)
	var b [1]byte
	_, err = conn.Read(b[:])
	if time.Until(stop) > 200*time.Millisecond {
		us.shortEnds.Add(1)
		c.note("hold connection ended before its stop (%s)", errKind(err))
	}
}

func (c *client) churn(us *ustate, fi int, f Flow) {
	stop := c.stopOf(f)
	addr := c.target(us, true)
	sleepUntil(c.at(f.StartS).Add(phase(100 * time.Millisecond)))
	msg := make([]byte, 32)
	got := make([]byte, 32)
	for time.Until(stop) > 100*time.Millisecond {
		conn, err := c.dial(us, addr)
		if err != nil {
			us.dialErrs.Add(1)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		us.conns.Add(1)
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err = conn.Write(header('E', us.idx, fi)); err == nil {
			if _, err = conn.Write(msg); err == nil {
				_, err = io.ReadFull(conn, got)
			}
		}
		conn.Close()
		if err != nil {
			us.shortEnds.Add(1)
			c.note("churn connection failed (%s)", errKind(err))
		}
	}
}

// errKind is a short name of a connection's error for the notes.
func errKind(err error) string {
	switch {
	case err == nil:
		return "none"
	case err == io.EOF:
		return "EOF"
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return "timeout"
	}
	return "error"
}

// udpflood: from StartS to the flow's stop, Rate datagrams a second (1000)
// to the user port's UDP side, each from a new source port (a fresh
// socket), 16 bytes: no spoofing, one client (each counts in conns).
func (c *client) udpflood(us *ustate, f Flow) {
	stop := c.stopOf(f)
	raddr, err := net.ResolveUDPAddr("udp", c.target(us, false))
	if err != nil {
		c.err("udpflood: %v", err)
		return
	}
	var laddr *net.UDPAddr
	if us.u.Src != "" {
		laddr = &net.UDPAddr{IP: net.ParseIP(us.u.Src)}
	}
	rate := f.Rate
	if rate <= 0 {
		rate = 1000
	}
	sleepUntil(c.at(f.StartS))
	b := make([]byte, 16)
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	acc := 0.0
	for time.Until(stop) > 0 {
		<-t.C
		for acc += rate / 100; acc >= 1; acc-- {
			uc, err := net.DialUDP("udp", laddr, raddr)
			if err != nil {
				us.dialErrs.Add(1)
				continue
			}
			uc.Write(b)
			uc.Close()
			us.conns.Add(1)
		}
	}
}
