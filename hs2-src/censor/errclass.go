package censor

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
)

// Pure helpers: they map an error the caller already has to a class. They
// read no detector state.

// ConnectStage classifies a failed TCP connect.
func ConnectStage(err error) DialStage {
	var ne net.Error
	switch {
	case err == nil:
		return DialOK
	case errors.Is(err, syscall.ECONNREFUSED):
		return DialConnRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return DialConnUnreach
	case errors.As(err, &ne) && ne.Timeout():
		return DialConnTimeout
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "refused"):
		return DialConnRefused
	case strings.Contains(s, "unreachable"), strings.Contains(s, "no route"):
		return DialConnUnreach
	case strings.Contains(s, "timeout"), strings.Contains(s, "timed out"):
		return DialConnTimeout
	}
	return DialConnOther
}

// HandshakeStage classifies a failed TLS handshake after TCP connected.
func HandshakeStage(err error) DialStage {
	var ne net.Error
	switch {
	case err == nil:
		return DialOK
	case errors.As(err, &ne) && ne.Timeout():
		return DialHelloTimeout
	case errors.Is(err, syscall.ECONNRESET), strings.Contains(err.Error(), "connection reset"):
		return DialHelloReset
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return DialHelloEOF
	}
	return DialHelloEOF
}

// AcceptStage classifies a listener's failed TLS handshake.
func AcceptStage(err error) AcceptOutcome {
	var ne net.Error
	var re tls.RecordHeaderError
	switch {
	case err == nil:
		return AcceptOK
	case errors.As(err, &re):
		return AcceptNotTLS
	case errors.As(err, &ne) && ne.Timeout():
		return AcceptNoHello
	case errors.Is(err, syscall.ECONNRESET), strings.Contains(err.Error(), "connection reset"):
		return AcceptReset
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return AcceptEOF
	}
	return AcceptNotTLS
}

// DeathOf classifies a link's first read/write error. Our own Close (retire,
// drain, spare, ZDU peerOld) surfaces as net.ErrClosed and is DeathLocal.
func DeathOf(err error) DeathClass {
	var ne net.Error
	switch {
	case err == nil:
		return DeathOther
	case errors.Is(err, net.ErrClosed):
		return DeathLocal
	case errors.Is(err, io.EOF):
		return DeathEOF
	case errors.As(err, &ne) && ne.Timeout():
		return DeathTimeout
	case errors.Is(err, syscall.ECONNABORTED):
		return DeathAbort
	case errors.Is(err, syscall.ECONNRESET):
		return DeathRST
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return DeathUnreach
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "connection reset"), strings.Contains(s, "broken pipe"):
		return DeathRST
	case strings.Contains(s, "unreachable"), strings.Contains(s, "no route"):
		return DeathUnreach
	case strings.Contains(s, "use of closed"):
		return DeathLocal
	}
	return DeathOther
}
