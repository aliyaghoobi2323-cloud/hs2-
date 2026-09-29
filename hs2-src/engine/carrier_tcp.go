package engine

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// The TCP carrier is a single reliable byte stream that carries hs2 frames. It
// is deliberately the dumbest part of the system: dial (or accept), run the
// handshake, then copy frames until the socket breaks. Everything that makes
// the tunnel survive a broken carrier lives ABOVE it, in the engine: the TUN
// device and the retransmit-less session are not owned by the carrier and are
// not tied to its lifetime.

// dialCarrier connects to the listen side and completes the handshake as
// initiator. On success it returns a live session and the connection.
func dialCarrier(ctx context.Context, addr, bindIP string, local core.StaticKey, remoteStatic, psk []byte, sessID uint64) (*core.Session, net.Conn, error) {
	d := net.Dialer{Timeout: 8 * time.Second}
	if bindIP != "" {
		ip := net.ParseIP(bindIP)
		if ip == nil {
			return nil, nil, fmt.Errorf("invalid source IP %q", bindIP)
		}
		d.LocalAddr = &net.TCPAddr{IP: ip}
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	ini, err := core.NewInitiator(local, remoteStatic, psk)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	m1, err := ini.WriteMessage1()
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if err := writeBlock(conn, m1); err != nil {
		conn.Close()
		return nil, nil, err
	}
	m2, err := readBlock(conn)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	secret, err := ini.ReadMessage2(m2)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	sess, err := core.NewSession(secret, true, sessID)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return sess, conn, nil
}

// acceptCarrier runs the responder side on an accepted connection. On a
// rejected first message it returns an error and the caller hands the raw
// connection to the decoy (decoy is added later; for now the conn is closed).
func acceptCarrier(conn net.Conn, resp *core.Responder, sessID uint64) (*core.Session, error) {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	m1, err := readBlock(conn)
	if err != nil {
		return nil, err
	}
	hs, err := resp.ReadMessage1(m1)
	if err != nil {
		return nil, err // caller -> decoy
	}
	m2, secret, err := resp.WriteMessage2(hs)
	if err != nil {
		return nil, err
	}
	if err := writeBlock(conn, m2); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Time{})
	return core.NewSession(secret, false, sessID)
}

// Handshake messages are length-delimited with a 2-byte prefix. This is only
// for the handshake, which happens once per connection; data frames use the
// session's own masked framing.
// Handshake framing with shaping: outer length covers innerlen(2)+msg+pad, so
// the number of bytes on the wire varies per connection while the receiver
// still recovers the exact Noise message from the inner length. This is what
// stops the handshake being "always 112 bytes then 48".
func writeBlock(w io.Writer, b []byte) error {
	pad := make([]byte, core.HandshakePad())
	rand.Read(pad)
	inner := make([]byte, 2+len(b)+len(pad))
	binary.BigEndian.PutUint16(inner[:2], uint16(len(b)))
	copy(inner[2:], b)
	copy(inner[2+len(b):], pad)
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(len(inner)))
	if _, err := w.Write(p[:]); err != nil {
		return err
	}
	_, err := w.Write(inner)
	return err
}

func readBlock(r io.Reader) ([]byte, error) {
	var p [2]byte
	if _, err := io.ReadFull(r, p[:]); err != nil {
		return nil, err
	}
	outer := binary.BigEndian.Uint16(p[:])
	inner := make([]byte, outer)
	if _, err := io.ReadFull(r, inner); err != nil {
		return nil, err
	}
	if len(inner) < 2 {
		return nil, io.ErrUnexpectedEOF
	}
	n := binary.BigEndian.Uint16(inner[:2])
	if int(n)+2 > len(inner) {
		return nil, io.ErrUnexpectedEOF
	}
	return inner[2 : 2+n], nil
}
