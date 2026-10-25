package engine

import (
	"context"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// The stats record past 64 bytes (WINDOW_CORE §8.1): its layout, and the
// four pairings of an edge and an exit from before and after it.

// extRec is a record with every field set, past 64 bytes too.
func extRec(seq uint32) statsRec {
	s := uint64(seq)
	return statsRec{seq: seq, flags: statsFlagChrono | statsFlagTCPInfo, mono: s * 1e9, tx: s << 20,
		txBlocked: s * 7e8, busy: s * 1e6, rwnd: s * 3e5, sndbuf: s * 2e5, delivery: s * 12345,
		srtt: 150000 + seq, minRTT: 149000, retrans: s * 17, segsOut: s * 1000, rttVar: 900 + seq}
}

// firstOnly is r as a record of the first 64 bytes carries it.
func firstOnly(r statsRec) statsRec {
	r.srtt, r.minRTT, r.rttVar, r.retrans, r.segsOut = 0, 0, 0, 0, 0
	return r
}

// The fields past 64 bytes sit at their fixed offsets, big-endian, the
// reserved word zero; a record of 64 bytes carries none of them (and writing
// one does not run past it), and a longer one than this version knows is
// parsed as far as it knows.
func TestStatsRecordExtendedLayout(t *testing.T) {
	r := statsRec{seq: 1, mono: 2, srtt: 0x01020304, minRTT: 0x05060708, retrans: 0x090a0b0c0d0e0f10,
		segsOut: 0x1112131415161718, rttVar: 0x191a1b1c}
	b := make([]byte, statsRecLen)
	for i := range b {
		b[i] = 0xFF
	}
	putStatsRec(b, r)
	be := binary.BigEndian
	if be.Uint32(b[64:]) != r.srtt || be.Uint32(b[68:]) != r.minRTT || be.Uint64(b[72:]) != r.retrans ||
		be.Uint64(b[80:]) != r.segsOut || be.Uint32(b[88:]) != r.rttVar || be.Uint32(b[92:]) != 0 {
		t.Fatalf("fields past 64 misplaced: % x", b[64:])
	}
	if statsRecLen != 96 || statsRecMin != 64 {
		t.Fatalf("record %d bytes, the least accepted %d: want 96 and 64", statsRecLen, statsRecMin)
	}
	if got := parseStatsRec(b); got != r {
		t.Fatalf("round trip: %+v, want %+v", got, r)
	}
	if got := parseStatsRec(b[:statsRecMin]); got != firstOnly(r) {
		t.Fatalf("64 bytes of it: %+v, want %+v", got, firstOnly(r))
	}
	short := make([]byte, statsRecMin, statsRecLen)
	putStatsRec(short, r)
	if got := parseStatsRec(short); got != firstOnly(r) || be.Uint32(short[:statsRecLen][64:]) != 0 {
		t.Fatalf("a 64-byte record: %+v, want %+v and nothing past it", got, firstOnly(r))
	}
	long := append(append([]byte(nil), b...), 0xEE, 0xEE, 0xEE, 0xEE)
	if got := parseStatsRec(long); got != r {
		t.Fatalf("a longer record: %+v, want %+v", got, r)
	}
}

// fixedExit answers the stats handshake with recLen and each poll with
// extRec in recLen bytes: 96 as this version's exit, 64 as an older one.
func fixedExit(recLen byte) statsExit {
	return func(_ context.Context, _ *smux.Session, st *smux.Stream, _ *linkMeter) {
		defer st.Close()
		var hs [2]byte // kind, version
		if _, err := io.ReadFull(st, hs[:]); err != nil || hs[0] != kindStats {
			return
		}
		st.Write([]byte{statsVer, recLen, 3})
		var p [4]byte
		for {
			if _, err := io.ReadFull(st, p[:]); err != nil {
				return
			}
			rec := make([]byte, recLen)
			putStatsRec(rec, extRec(binary.BigEndian.Uint32(p[:])))
			if _, err := st.Write(rec); err != nil {
				return
			}
		}
	}
}

// oldEdge plays an edge from before the record grew over a stream of sess:
// it accepts a recLen of 64 or more, reads whole records of it and decodes
// the first 64 bytes. It returns the recLen and the records of n polls.
func oldEdge(t *testing.T, sess *smux.Session, n int) (int, []statsRec) {
	t.Helper()
	st, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(5 * time.Second))
	st.Write([]byte{kindStats, statsVer})
	var hdr [3]byte
	if _, err := io.ReadFull(st, hdr[:]); err != nil || hdr[0] != statsVer || hdr[1] < 64 {
		t.Fatalf("handshake % x, %v: an older edge would give up", hdr, err)
	}
	var recs []statsRec
	buf := make([]byte, hdr[1])
	for seq := uint32(1); seq <= uint32(n); seq++ {
		var p [4]byte
		binary.BigEndian.PutUint32(p[:], seq)
		st.Write(p[:])
		if _, err := io.ReadFull(st, buf); err != nil {
			t.Fatalf("record %d: %v", seq, err)
		}
		recs = append(recs, parseStatsRec(buf[:64]))
	}
	return int(hdr[1]), recs
}

// An edge and an exit of either version: a new pair carries the fields
// past 64 bytes; a new edge reads an older exit's 64-byte records as before,
// with those fields 0; an older edge skips them in a new exit's records and
// stays aligned record after record; and two older ones are as they were.
func TestStatsRecordPairings(t *testing.T) {
	ctx := context.Background()
	t.Run("new edge, new exit", func(t *testing.T) {
		p := newStatsPair(t, ctx, nil, fixedExit(statsRecLen))
		p.startStats(t, ctx, nil)
		waitStatsState(t, p.edge.m, statsOK, statsHandshakeTimeout)
		for seq := uint32(1); seq <= 3; seq++ {
			got := pollRecord(t, p.edge.m, seq)
			got.at = time.Time{}
			if got != extRec(seq) {
				t.Fatalf("record %d: %+v, want %+v", seq, got, extRec(seq))
			}
		}
		// the exit of this version says so in its handshake
		q := newStatsPair(t, ctx, nil, currentExit(""))
		if recLen, recs := oldEdge(t, q.edge.sess, 2); recLen != statsRecLen || recs[1].seq != 2 {
			t.Fatalf("this version's exit: recLen %d, records %+v", recLen, recs)
		}
	})
	t.Run("new edge, older exit", func(t *testing.T) {
		p := newStatsPair(t, ctx, nil, fixedExit(statsRecMin))
		p.startStats(t, ctx, nil)
		waitStatsState(t, p.edge.m, statsOK, statsHandshakeTimeout)
		for seq := uint32(1); seq <= 3; seq++ {
			got := pollRecord(t, p.edge.m, seq)
			got.at = time.Time{}
			if got != firstOnly(extRec(seq)) {
				t.Fatalf("record %d: %+v, want %+v", seq, got, firstOnly(extRec(seq)))
			}
		}
	})
	for _, c := range []struct {
		name string
		exit statsExit
		len  int
	}{
		{"older edge, new exit", fixedExit(statsRecLen), statsRecLen},
		{"older edge, new exit (serveStats)", currentExit(""), statsRecLen},
		{"older edge, older exit", fixedExit(statsRecMin), statsRecMin},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newStatsPair(t, ctx, nil, c.exit)
			recLen, recs := oldEdge(t, p.edge.sess, 4)
			if recLen != c.len {
				t.Fatalf("recLen %d, want %d", recLen, c.len)
			}
			for i, r := range recs {
				seq := uint32(i + 1)
				if r.seq != seq {
					t.Fatalf("record %d read as seq %d: out of step", seq, r.seq)
				}
				if c.len == statsRecLen && r.mono == 0 {
					t.Fatalf("record %d without its clock: %+v", seq, r)
				}
				if c.name != "older edge, new exit (serveStats)" && r != firstOnly(extRec(seq)) {
					t.Fatalf("record %d: %+v, want %+v", seq, r, firstOnly(extRec(seq)))
				}
			}
		})
	}
}
