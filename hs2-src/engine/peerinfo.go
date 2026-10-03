package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"time"
)

// Peer info (kindInfo): on every link the two servers tell each other their
// link-pool ceiling (the max_links each one actually runs with), so BOTH can
// show the effective ceiling exactly — in direct mode the Iran server's (the
// exit accepts whatever the edge dials), in reverse the lower of the two (the
// exit clamps the edge's target to its own max) — and name the side that
// limits it. It is display-only: nothing here feeds the autopilot or the pool.
//
// Wire (after the kind byte), one exchange per link, then the stream closes:
//
//	edge -> exit  [ver][n u8][n bytes]
//	exit -> edge  [ver][n u8][n bytes]
//
//	v1 payload: maxLinks u16 (0 = none). A reader takes the fields it knows from
//	the first n bytes and skips the rest, so the payload can grow compatibly.
//
// The edge opens the stream (it is the smux client in both directions). An
// older exit closes the unknown kind at once: the edge reads EOF and leaves the
// exit's ceiling unknown. An older edge never opens it, so a newer exit leaves
// the edge's ceiling unknown. Either way nothing else changes.
const (
	infoVer     = 1
	infoTimeout = 5 * time.Second
)

// infoRetry / infoTries: a reply that did not come in time (a congested link)
// is retried a few times while the link is up. Variables so tests can shorten.
var (
	infoRetry = 10 * time.Second
	infoTries = 3
)

var errInfoVersion = errors.New("peer info: bad version")

// encodeInfo is one side's message: [ver][n=2][maxLinks u16].
func encodeInfo(maxLinks int) []byte {
	if maxLinks < 0 {
		maxLinks = 0
	}
	if maxLinks > 0xffff {
		maxLinks = 0xffff
	}
	b := []byte{infoVer, 2, 0, 0}
	binary.BigEndian.PutUint16(b[2:], uint16(maxLinks))
	return b
}

// readInfo reads one [ver][n][n bytes] message and returns the ceiling it
// carries (0 when the payload is too short to hold it). Any version >= 1 is
// read the same way: a later version only appends fields.
func readInfo(r io.Reader) (int, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, err
	}
	if h[0] < 1 {
		return 0, errInfoVersion
	}
	p := make([]byte, int(h[1]))
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, err
	}
	if len(p) < 2 {
		return 0, nil
	}
	return int(binary.BigEndian.Uint16(p)), nil
}

// openInfo runs the EDGE side for one link: send this side's ceiling, read the
// exit's, and store it on the link's meter (LinkManager.Stats reports it).
func openInfo(ctx context.Context, l Link, myMax int) {
	mtr := linkMeterOf(l)
	ro, ok := l.(rawStreamOpener)
	if mtr == nil || !ok {
		return
	}
	for try := 0; try < infoTries && ctx.Err() == nil && l.Alive(); try++ {
		if try > 0 && !sleepCtx(ctx, infoRetry) {
			return
		}
		peer, err := exchangeInfo(ro, myMax)
		if err == nil {
			mtr.peerMax.Store(uint32(peer))
			return
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errInfoVersion) {
			return // an older exit closed the unknown kind (or the link died)
		}
	}
}

// exchangeInfo opens one kindInfo stream, sends ours and reads theirs.
func exchangeInfo(ro rawStreamOpener, myMax int) (int, error) {
	st, err := ro.OpenRawStream()
	if err != nil {
		return 0, err
	}
	defer st.Close()
	st.SetDeadline(time.Now().Add(infoTimeout))
	if _, err := st.Write(append([]byte{kindInfo}, encodeInfo(myMax)...)); err != nil {
		return 0, err
	}
	return readInfo(st)
}

// serveInfo runs the EXIT side: read the edge's ceiling, store it, answer with
// this side's.
func serveInfo(st io.ReadWriteCloser, myMax int, store func(int)) {
	defer st.Close()
	if d, ok := st.(interface{ SetDeadline(time.Time) error }); ok {
		d.SetDeadline(time.Now().Add(infoTimeout))
	}
	peer, err := readInfo(st)
	if err != nil {
		return
	}
	if store != nil {
		store(peer)
	}
	st.Write(encodeInfo(myMax))
}
