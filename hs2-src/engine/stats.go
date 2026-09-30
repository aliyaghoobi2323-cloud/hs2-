package engine

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tlscarrier"
)

// Per-link send-side statistics from the EXIT (kindStats).
//
// Downloads — the bulk of the traffic — are SENT by the exit, so only the exit
// can see whether a link's download direction is limited by the path: its smux
// writer blocking on the socket (linkMeter.wrBlocked) and its socket's TCP_INFO
// chrono counters. The edge, which sizes the pool, opens one kindStats stream
// per link and polls the exit for a cumulative record whenever the link moved
// data; it turns consecutive records into a "download pressed" signal.
//
// Wire (after the kind byte):
//
//	edge -> exit  [ver=1]                       once
//	exit -> edge  [ver=1][recLen][caps]         once (caps bit0 tcp_info, bit1 meter)
//	edge -> exit  [seq u32]                     per poll
//	exit -> edge  record, exactly recLen bytes  per poll (fields past the known
//	              64 bytes are skipped, so the record can grow compatibly)
//
//	record: 0 seq u32 | 4 flags u16 (bit0 chrono valid, bit1 tcp_info ok) |
//	        6 reserved u16 | 8 monoNs u64 | 16 txBytes u64 | 24 txBlockedNs u64 |
//	        32 busyUs u64 | 40 rwndLimUs u64 | 48 sndbufLimUs u64 | 56 deliveryRate u64
//
// An older exit closes the unknown stream kind; the edge notices (the link is
// still alive) and marks stats unsupported for that link — the pool then sizes
// by activity and upload pressure only, which never over-grows.
const (
	statsVer              = 1
	statsRecLen           = 64
	statsHandshakeTimeout = 5 * time.Second

	statsPending     int32 = 0
	statsOK          int32 = 1
	statsUnsupported int32 = 2

	statsFlagChrono  = 1 << 0
	statsFlagTCPInfo = 1 << 1
)

// procStart anchors the exit's monotonic clock in records.
var procStart = time.Now()

// statsRec is one decoded exit record plus when the edge received it.
type statsRec struct {
	seq                                               uint32
	flags                                             uint16
	mono, tx, txBlocked, busy, rwnd, sndbuf, delivery uint64
	at                                                time.Time
}

func (r *statsRec) chrono() bool { return r.flags&statsFlagChrono != 0 }

func putStatsRec(b []byte, r statsRec) {
	binary.BigEndian.PutUint32(b[0:], r.seq)
	binary.BigEndian.PutUint16(b[4:], r.flags)
	binary.BigEndian.PutUint16(b[6:], 0)
	binary.BigEndian.PutUint64(b[8:], r.mono)
	binary.BigEndian.PutUint64(b[16:], r.tx)
	binary.BigEndian.PutUint64(b[24:], r.txBlocked)
	binary.BigEndian.PutUint64(b[32:], r.busy)
	binary.BigEndian.PutUint64(b[40:], r.rwnd)
	binary.BigEndian.PutUint64(b[48:], r.sndbuf)
	binary.BigEndian.PutUint64(b[56:], r.delivery)
}

func parseStatsRec(b []byte) statsRec {
	return statsRec{
		seq:       binary.BigEndian.Uint32(b[0:]),
		flags:     binary.BigEndian.Uint16(b[4:]),
		mono:      binary.BigEndian.Uint64(b[8:]),
		tx:        binary.BigEndian.Uint64(b[16:]),
		txBlocked: binary.BigEndian.Uint64(b[24:]),
		busy:      binary.BigEndian.Uint64(b[32:]),
		rwnd:      binary.BigEndian.Uint64(b[40:]),
		sndbuf:    binary.BigEndian.Uint64(b[48:]),
		delivery:  binary.BigEndian.Uint64(b[56:]),
	}
}

// statsUnsupportedOnce logs the older-exit fallback once per process.
var statsUnsupportedOnce sync.Once

// openStats runs the EDGE side for one link: handshake, then one poll per
// signal on mtr.statsPoll (the sampler signals only when the link moved data,
// so an idle link carries no extra timing beat), with a reader goroutine that
// stores each record in mtr.peer. It returns when the link or ctx ends.
func openStats(ctx context.Context, l Link, logf func(string, ...any)) {
	mtr := linkMeterOf(l)
	ro, ok := l.(rawStreamOpener)
	if mtr == nil || !ok || mtr.statsPoll == nil {
		return
	}
	st, err := ro.OpenRawStream()
	if err != nil {
		return
	}
	defer st.Close()
	if _, err := st.Write([]byte{kindStats, statsVer}); err != nil {
		return
	}
	st.SetReadDeadline(time.Now().Add(statsHandshakeTimeout))
	var hdr [3]byte
	if _, err := io.ReadFull(st, hdr[:]); err != nil || hdr[0] != statsVer || int(hdr[1]) < statsRecLen {
		// An older exit closes the unknown kind at once; a dying link fails the
		// same read. Tell them apart by whether the link is still up a moment
		// later, so a link lost during the handshake is not misreported.
		time.Sleep(200 * time.Millisecond)
		if l.Alive() {
			mtr.statsState.Store(statsUnsupported)
			statsUnsupportedOnce.Do(func() {
				if logf != nil {
					logf("mtcp: the other server does not report link stats (older hs2) — download pressure unknown; sizing by activity and upload pressure until it is upgraded")
				}
			})
		}
		return
	}
	st.SetReadDeadline(time.Time{})
	recLen := int(hdr[1])
	mtr.statsState.Store(statsOK)

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, recLen)
		for {
			if _, err := io.ReadFull(st, buf); err != nil {
				return
			}
			r := parseStatsRec(buf)
			r.at = time.Now()
			mtr.peer.Store(&r)
		}
	}()
	var seq uint32
	var p [4]byte
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-mtr.statsPoll:
		}
		seq++
		binary.BigEndian.PutUint32(p[:], seq)
		st.SetWriteDeadline(time.Now().Add(statsHandshakeTimeout))
		if _, err := st.Write(p[:]); err != nil {
			return
		}
	}
}

// serveStats runs the EXIT side: answer the handshake, then one record per
// poll with this link's cumulative download-side counters (its meter: payload
// sent and time its writer waited for the socket) and its socket's TCP_INFO.
func serveStats(ctx context.Context, st io.ReadWriteCloser, car *tlscarrier.Carrier, mtr *linkMeter) {
	defer st.Close()
	var ver [1]byte
	if _, err := io.ReadFull(st, ver[:]); err != nil {
		return
	}
	var caps byte
	if _, ok := tcpStats(tcpConnOf(car)); ok {
		caps |= 1
	}
	if mtr != nil {
		caps |= 2
	}
	if _, err := st.Write([]byte{statsVer, statsRecLen, caps}); err != nil {
		return
	}
	type deadliner interface{ SetWriteDeadline(time.Time) error }
	var p [4]byte
	rec := make([]byte, statsRecLen)
	for ctx.Err() == nil {
		if _, err := io.ReadFull(st, p[:]); err != nil {
			return
		}
		r := statsRec{seq: binary.BigEndian.Uint32(p[:]), mono: uint64(time.Since(procStart))}
		if mtr != nil {
			r.tx = mtr.wrBytes.Load()
			r.txBlocked = uint64(mtr.wrBlocked.Load())
		}
		if ts, ok := tcpStats(tcpConnOf(car)); ok {
			r.flags |= statsFlagTCPInfo
			if ts.chronoValid {
				r.flags |= statsFlagChrono
			}
			r.busy, r.rwnd, r.sndbuf, r.delivery = ts.busyUs, ts.rwndUs, ts.sndbufUs, ts.deliveryRate
		}
		putStatsRec(rec, r)
		if d, ok := st.(deadliner); ok {
			d.SetWriteDeadline(time.Now().Add(statsHandshakeTimeout))
		}
		if _, err := st.Write(rec); err != nil {
			return
		}
	}
}

func tcpConnOf(car *tlscarrier.Carrier) *net.TCPConn {
	if car == nil {
		return nil
	}
	return car.TCPConn()
}

// pollStats asks the exit for a fresh record without blocking (at most one poll
// outstanding per link).
func pollStats(mtr *linkMeter) {
	if mtr == nil || mtr.statsPoll == nil || mtr.statsState.Load() != statsOK {
		return
	}
	select {
	case mtr.statsPoll <- struct{}{}:
	default:
	}
}
