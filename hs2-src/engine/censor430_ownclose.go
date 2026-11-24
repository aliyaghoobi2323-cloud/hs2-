package engine

import (
	"net"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/censor"
)

// b430: own closes the Kharej end sees as a reset. b414 counted an Iran own
// abort only when the link's socket ended in ECONNABORTED (killed on this
// host). A link hs2 closes itself (retire, LB or probation drop, the quiet
// watchdog, smux keepalive, shrink, ZDU) normally ends in a FIN, which the
// Kharej end reads as EOF. But Linux sends an RST instead of a FIN when the
// socket still holds unread data at close (tcp_close), which a busy
// download link nearly always does: the Kharej end then saw a reset no
// report claimed, and judged it. Such a close is now counted as an own abort
// at the point hs2 closes the link, from the kernel's unread byte count,
// whatever error the link's reader then sees (net.ErrClosed, classed local).
// Detect-only: a read-only ioctl before our close; nothing else changes.

// censorOwnClose is called once, before hs2 itself closes a watched link
// (no socket error came first). Edge side, detector on only.
func (w *watchConn) censorOwnClose() {
	if w.raw == nil || !censorIsEdge.Load() {
		return
	}
	if censor.Active() {
		if w.ownClosed.CompareAndSwap(false, true) {
			tc := censorTCPOf(w.raw)
			if !censorCountRstClose(tc) {
				censorCountStallClose(tc)
			}
		}
	}
}

// Close is hs2's own close of the link (smux Session.Close closes its conn
// through here). After a socket error (fail) the death was classed there.
func (w *watchConn) Close() error {
	if w.why.Load() == nil {
		w.censorOwnClose()
	}
	return w.Conn.Close()
}

// censorCountRstClose counts hs2's close of tc as an own abort on its local
// (Iran) IP when the kernel will send it as an RST: unread data now. It
// reports whether it counted.
func censorCountRstClose(tc *net.TCPConn) bool {
	if censorUnread(tc) <= 0 {
		return false
	}
	censor.NoteOwnRstClose()
	censorNoteOwnAbort(censorLocalIP(tc))
	return true
}

// o432: a close with nothing unread goes out as a FIN, but when the path has
// stopped answering (the reason hs2 drops a stuck or degraded link) the FIN
// is likely lost too, and the Kharej end, still sending, has its next data
// answered by our closed socket with an RST before any FIN reaches it
// (TcpExtTCPAbortOnData on Iran). Lab o432 (6 stuck busy links cut both
// ways, hs2's stuck drop closed them): 0 counted, Kharej held and released 5.
// A path counts as stalled when our own segments sit unacknowledged through
// at least ownStallBackoff retransmission timeouts in a row; with the path
// merely lossy (30 % random) that is rare, and the degraded drops there
// reached Kharej as FINs (0 held).
const ownStallBackoff = 2

// ownCloseStalled reports whether a close with this TCP_INFO snapshot is
// one the peer likely sees as a reset (see ownStallBackoff).
func ownCloseStalled(st tcpStat, ok bool) bool {
	return ok && st.unacked > 0 && st.backoff >= ownStallBackoff
}

// censorCountStallClose counts hs2's FIN close of tc on a stalled path as an
// own abort on its local (Iran) IP. It reports whether it counted.
func censorCountStallClose(tc *net.TCPConn) bool {
	if tc == nil || !ownCloseStalled(tcpStats(tc)) {
		return false
	}
	censor.NoteOwnStallClose()
	censorNoteOwnAbort(censorLocalIP(tc))
	return true
}
