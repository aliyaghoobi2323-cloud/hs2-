package engine

import (
	"context"
	"encoding/binary"
	"io"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
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
	t := time.NewTicker(controlInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		seq++
		binary.BigEndian.PutUint64(ping[0:], seq)
		binary.BigEndian.PutUint64(ping[8:], uint64(time.Now().UnixNano()))
		st.SetWriteDeadline(time.Now().Add(controlInterval))
		if _, err := st.Write(ping); err != nil {
			return // link/stream gone; the pool reaps the link on its own
		}
		st.SetReadDeadline(time.Now().Add(2 * controlInterval))
		if _, err := io.ReadFull(st, pong); err != nil {
			return
		}
		if binary.BigEndian.Uint64(pong[0:]) != seq {
			continue // stale/out-of-order; ignore
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
