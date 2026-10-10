// Package smux is a multiplexing library for Golang.
//
// It relies on an underlying connection to provide reliability and ordering, such as TCP or KCP,
// and provides stream-oriented multiplexing over a single channel.
package smux

import (
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// Config is used to tune the Smux session
type Config struct {
	// SMUX Protocol version, support 1,2
	Version int

	// Disabled keepalive
	KeepAliveDisabled bool

	// KeepAliveInterval is how often to send a NOP command to the remote
	KeepAliveInterval time.Duration

	// KeepAliveTimeout is how long the session
	// will be closed if no data has arrived
	KeepAliveTimeout time.Duration

	// MaxFrameSize is used to control the maximum
	// frame size to sent to the remote
	MaxFrameSize int

	// MaxReceiveBuffer is used to control the maximum
	// number of data in the buffer pool
	MaxReceiveBuffer int

	// MaxStreamBuffer is used to control the maximum
	// number of data per stream
	MaxStreamBuffer int

	// MinStreamBuffer turns on the adaptive per-stream receive window
	// (protocol version 2) when positive; the wire format does not change,
	// a peer of any version simply honours the window it is told.
	//
	// An adaptive stream starts at a 256 KiB window (the window a peer
	// assumes before any update; clamped to [MinStreamBuffer,
	// MaxStreamBuffer]) and resizes it at each window update from the least
	// backlog its reader left unread since the previous one: over
	// StreamLagTarget it shrinks by the excess, down to MinStreamBuffer;
	// under a quarter of the target it doubles, up to MaxStreamBuffer,
	// unless the session ran out of MaxReceiveBuffer meanwhile. A reader
	// slower than its share of the link then holds about StreamLagTarget of
	// the session's MaxReceiveBuffer instead of up to MaxStreamBuffer, while
	// one that keeps up still gets the full window.
	//
	// The costs: a stream reaches the full window one doubling per update,
	// so a large transfer starts a round trip or two later than with the
	// fixed window; and what a reader was already granted (up to
	// MaxStreamBuffer, if it kept up until then) still sits in the session's
	// buffer while it drains at the reader's new, slower pace.
	// Zero (the default) keeps the fixed window of MaxStreamBuffer.
	MinStreamBuffer int

	// StreamLagTarget is the unread backlog, in bytes, an adaptive stream
	// aims for (see MinStreamBuffer). Must be positive when MinStreamBuffer
	// is, zero otherwise.
	StreamLagTarget int
}

// DefaultConfig is used to return a default configuration
func DefaultConfig() *Config {
	return &Config{
		Version:           1,
		KeepAliveInterval: 10 * time.Second,
		KeepAliveTimeout:  30 * time.Second,
		MaxFrameSize:      32768,
		MaxReceiveBuffer:  4194304,
		MaxStreamBuffer:   65536,
	}
}

// VerifyConfig is used to verify the sanity of configuration
func VerifyConfig(config *Config) error {
	if !(config.Version == 1 || config.Version == 2) {
		return errors.New("unsupported protocol version")
	}
	if !config.KeepAliveDisabled {
		if config.KeepAliveInterval == 0 {
			return errors.New("keep-alive interval must be positive")
		}
		if config.KeepAliveTimeout < config.KeepAliveInterval {
			return fmt.Errorf("keep-alive timeout must be larger than keep-alive interval")
		}
	}
	if config.MaxFrameSize <= 0 {
		return errors.New("max frame size must be positive")
	}
	if config.MaxFrameSize > 65535 {
		return errors.New("max frame size must not be larger than 65535")
	}
	if config.MaxReceiveBuffer <= 0 {
		return errors.New("max receive buffer must be positive")
	}
	if config.MaxStreamBuffer <= 0 {
		return errors.New("max stream buffer must be positive")
	}
	if config.MaxStreamBuffer > config.MaxReceiveBuffer {
		return errors.New("max stream buffer must not be larger than max receive buffer")
	}
	if config.MaxStreamBuffer > math.MaxInt32 {
		return errors.New("max stream buffer cannot be larger than 2147483647")
	}
	if config.MinStreamBuffer < 0 || config.StreamLagTarget < 0 {
		return errors.New("min stream buffer and stream lag target must not be negative")
	}
	if config.MinStreamBuffer > 0 {
		if config.Version != 2 {
			return errors.New("adaptive stream window requires protocol version 2")
		}
		if config.MinStreamBuffer < 2*config.MaxFrameSize {
			return errors.New("min stream buffer must be at least twice the max frame size")
		}
		if config.MinStreamBuffer > config.MaxStreamBuffer {
			return errors.New("min stream buffer must not be larger than max stream buffer")
		}
		if config.StreamLagTarget == 0 {
			return errors.New("stream lag target must be positive when min stream buffer is set")
		}
	} else if config.StreamLagTarget != 0 {
		return errors.New("stream lag target requires min stream buffer")
	}
	return nil
}

// Server is used to initialize a new server-side connection.
func Server(conn io.ReadWriteCloser, config *Config) (*Session, error) {
	if config == nil {
		config = DefaultConfig()
	}
	if err := VerifyConfig(config); err != nil {
		return nil, err
	}
	return newSession(config, conn, false), nil
}

// Client is used to initialize a new client-side connection.
func Client(conn io.ReadWriteCloser, config *Config) (*Session, error) {
	if config == nil {
		config = DefaultConfig()
	}

	if err := VerifyConfig(config); err != nil {
		return nil, err
	}
	return newSession(config, conn, true), nil
}
