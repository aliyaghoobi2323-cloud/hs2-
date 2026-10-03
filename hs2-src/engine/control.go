package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
	"github.com/xtaci/smux"
)

// Per-link control channel (phase 3). Each edge link carries one extra smux
// stream, tagged kindCtrl, that the EDGE opens and drives. Every controlInterval
// the edge sends a ping and the exit answers with a pong that echoes the ping and
// appends the exit's own TCP retransmit counter. From the pair the edge learns:
//
//   - RTT: now - the timestamp it put in the ping (activity-independent latency).
//   - download-direction loss: the exit's retransmits are the loss on the path
//     FROM the exit TO the edge, which the edge's own TCP_INFO cannot see.
//
// Both feed the health logic, so a link that is bad only in the download
// direction — invisible to phase 1's local (upload) retransmit signal — is now
// caught too. The channel is edge-driven and additive: against an un-upgraded
// exit the control stream is simply closed (its serveStream has no kindCtrl
// case), the edge notices and stops, and the link keeps working on local signals.
//
// Wire records (after the one kindCtrl tag byte):
//
//	ping (edge->exit): [seq:8][edgeNanos:8]
//	pong (exit->edge): [seq:8][edgeNanos:8][exitRetrans:8]

const (
	ctrlPingLen = 16
	ctrlPongLen = 24
)

// openControl runs the edge side of the control channel for one link until the
// link dies or ctx ends. It is wired via LinkManager.OnLink, so it covers every
// link in both directions (the edge is always the smux client).
func openControl(ctx context.Context, l Link, logf func(string, ...any)) {
	mtr := linkMeterOf(l)
	ro, ok := l.(rawStreamOpener)
	if mtr == nil || !ok {
		return // link without metering or raw streams: no control channel
	}
	st, err := ro.OpenRawStream()
	if err != nil {
		return
	}
	defer st.Close()
	if _, err := st.Write([]byte{kindCtrl}); err != nil {
		return
	}
	var seq uint64
	ping := make([]byte, ctrlPingLen)
	pong := make([]byte, ctrlPongLen)
	moved := mtr.rdBytes.Load() + mtr.wrBytes.Load()
	// The cadence is the fixed controlInterval tick it always was while the
	// link carries traffic: the exit's download retransmits arrive with each
	// pong, and the health logic's loss rule was tuned against exactly this
	// beat (a jittered one would make it judge one pong interval's
	// retransmits against one 2 s tick's bytes). An idle link only answers
	// every 3rd–5th tick (~9–15 s): at hundreds of mostly idle links a ping on
	// every tick of every link is a few hundred messages a second, and an
	// idle link still hears the exit's smux keepalive every 4–8 s.
	t := time.NewTicker(controlInterval)
	defer t.Stop()
	skip := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := mtr.rdBytes.Load() + mtr.wrBytes.Load()
		active := now-moved >= activeBytes
		moved = now
		if !active && skip > 0 {
			skip--
			continue
		}
		if active {
			skip = 0
		} else {
			skip = 2 + rand.IntN(3) // the next 2–4 ticks stay quiet
		}
		seq++
		binary.BigEndian.PutUint64(ping[0:], seq)
		binary.BigEndian.PutUint64(ping[8:], uint64(time.Now().UnixNano()))
		st.SetWriteDeadline(time.Now().Add(controlInterval))
		if _, err := st.Write(ping); err != nil {
			return // link/stream gone; the pool reaps the link on its own
		}
		// Read until this ping's pong. A pong that missed its deadline (the
		// link was congested or briefly wedged) arrives later and is skipped
		// here, so one slow answer no longer ends the control channel for the
		// rest of the link's life.
		st.SetReadDeadline(time.Now().Add(2 * controlInterval))
		got := false
		for {
			n, err := io.ReadFull(st, pong)
			if err != nil {
				if n > 0 || !isTimeout(err) {
					return // gone, or out of step mid-record
				}
				break // no answer in time: try again next tick
			}
			if binary.BigEndian.Uint64(pong[0:]) == seq {
				got = true
				break
			}
		}
		if !got {
			continue
		}
		sent := int64(binary.BigEndian.Uint64(pong[8:]))
		exitRetrans := binary.BigEndian.Uint64(pong[16:])
		rtt := time.Now().UnixNano() - sent
		if rtt > 0 {
			mtr.rttMicros.Store(uint64(rtt / 1000))
		}
		mtr.peerRetrans.Store(exitRetrans)
		mtr.peerSeen.Store(true)
	}
}

// isTimeout reports a deadline error (smux's own, or a net.Error's).
func isTimeout(err error) bool {
	if errors.Is(err, smux.ErrTimeout) {
		return true
	}
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

// serveControl runs the exit side: it answers each ping with a pong carrying the
// exit's TCP retransmit counter for this link's socket, so the edge can see the
// download-direction loss. car provides that socket.
func serveControl(ctx context.Context, st io.ReadWriteCloser, car *tlscarrier.Carrier) {
	defer st.Close()
	tcp := func() uint64 {
		if car == nil {
			return 0
		}
		n, _ := retransmits(car.TCPConn())
		return n
	}
	ping := make([]byte, ctrlPingLen)
	pong := make([]byte, ctrlPongLen)
	for ctx.Err() == nil {
		if _, err := io.ReadFull(st, ping); err != nil {
			return
		}
		copy(pong[0:], ping[0:16]) // echo seq + edgeNanos
		binary.BigEndian.PutUint64(pong[16:], tcp())
		if _, err := st.Write(pong); err != nil {
			return
		}
	}
}
