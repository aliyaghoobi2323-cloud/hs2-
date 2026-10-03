package engine

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/tun"
)

// Engine binds a persistent TUN device to a Carrier that may come and go.
//
// The invariant that makes the tunnel self-healing: the TUN device is opened
// once, in Run, and is NEVER closed because a carrier failed. When a carrier
// dies the engine swaps in a fresh one while the same tun0, the same address,
// and the same routes stay exactly where they were. A TCP connection running
// THROUGH the tunnel sees a pause, not a reset.
//
// The engine is carrier-agnostic: it drives a CarrierDialer (dial side) or a
// CarrierListener (listen side) and moves frames between the TUN and whatever
// Carrier is currently live. Whether that carrier protects bytes with Noise or
// with TLS is the carrier's business, not the engine's.

const (
	keepaliveEvery = 5 * time.Second
	deadAfter      = 15 * time.Second
)

type Config struct {
	Iface     string
	LocalCIDR string
	PeerIP    string
	MTU       int
}

type Engine struct {
	cfg     Config
	dev     *tun.Device
	carrier atomic.Pointer[Carrier]
	sendMu  sync.Mutex
	log     func(string, ...any)
}

func New(cfg Config, logf func(string, ...any)) *Engine {
	if cfg.MTU == 0 {
		cfg.MTU = 1380
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Engine{cfg: cfg, log: logf}
}

// RunDial opens the TUN once, then keeps a dialled Carrier alive under it.
func (e *Engine) RunDial(ctx context.Context, dialer CarrierDialer) error {
	if err := e.openTUN(); err != nil {
		return err
	}
	defer e.dev.Close()
	go e.tunToCarrier(ctx)

	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		car, err := dialer.Dial(ctx)
		if err != nil {
			e.log("dial failed: %v (retry in %s)", err, backoff)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			if backoff < 8*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 500 * time.Millisecond
		e.log("carrier up")
		e.installAndPump(ctx, car)
		e.log("carrier down, TUN stays up; reconnecting")
	}
	return nil
}

// RunListen opens the TUN once, then serves accepted Carriers under it. The
// newest accepted carrier replaces the current one.
func (e *Engine) RunListen(ctx context.Context, ln CarrierListener) error {
	if err := e.openTUN(); err != nil {
		return err
	}
	defer e.dev.Close()
	// Closed before this returns (not only from the goroutine that unblocks
	// Accept): the process may exit as soon as RunListen returns.
	defer ln.Close()
	go e.tunToCarrier(ctx)
	go func() { <-ctx.Done(); ln.Close() }()

	for ctx.Err() == nil {
		car, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		if old := e.carrier.Load(); old != nil {
			e.log("new carrier replaces previous")
			(*old).Close()
		}
		e.log("carrier up")
		go e.installAndPump(ctx, car)
	}
	return nil
}

func (e *Engine) openTUN() error {
	dev, err := tun.Open(e.cfg.Iface, e.cfg.LocalCIDR, e.cfg.PeerIP, e.cfg.MTU)
	if err != nil {
		return err
	}
	e.dev = dev
	e.log("tun %s up: %s peer %s mtu %d", dev.Name(), e.cfg.LocalCIDR, e.cfg.PeerIP, e.cfg.MTU)
	return nil
}

// installAndPump makes car the active carrier, starts keepalives, and pumps
// carrier->TUN until the carrier dies. On exit the carrier is cleared so the
// TUN read pump drops packets until the next carrier arrives.
func (e *Engine) installAndPump(ctx context.Context, car Carrier) {
	e.carrier.Store(&car)
	defer func() {
		e.carrier.CompareAndSwap(&car, nil)
		car.Close()
	}()

	kctx, kcancel := context.WithCancel(ctx)
	defer kcancel()
	go e.keepalive(kctx, car)

	for ctx.Err() == nil {
		ft, payload, err := car.ReadFrame()
		if err != nil {
			return
		}
		switch ft {
		case core.TypeData:
			e.dev.Write(payload)
		case core.TypePing:
			e.sendVia(car, core.TypePong, nil)
		case core.TypePong, core.TypeClose:
		}
	}
}

func (e *Engine) keepalive(ctx context.Context, car Carrier) {
	for {
		d := time.Duration(core.KeepaliveJitter(int(keepaliveEvery/time.Millisecond))) * time.Millisecond
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
			if err := e.sendVia(car, core.TypePing, make([]byte, core.KeepalivePad())); err != nil {
				car.Close()
				return
			}
		}
	}
}

// tunToCarrier reads IP packets from the TUN forever and sends them on whatever
// carrier is currently live. No carrier => drop (nowhere to go); TCP resends.
func (e *Engine) tunToCarrier(ctx context.Context) {
	buf := make([]byte, e.cfg.MTU+64)
	for ctx.Err() == nil {
		n, err := e.dev.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		if c := e.carrier.Load(); c != nil {
			if err := e.sendVia(*c, core.TypeData, buf[:n]); err != nil {
				(*c).Close()
			}
		}
	}
}

// sendVia serialises sends across the data pump, keepalive, and pong reply so a
// carrier's frames never interleave on the wire.
func (e *Engine) sendVia(car Carrier, ftype byte, payload []byte) error {
	e.sendMu.Lock()
	defer e.sendMu.Unlock()
	return car.SendFrame(ftype, payload)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// acceptBackoff paces an accept loop through errors that persist — out of
// file descriptors (EMFILE, ENFILE), out of buffers — instead of spinning a
// core on them (or giving up on the listener): 5 ms doubling to 1 s, reset by
// the next accepted connection, and one log line a minute.
type acceptBackoff struct {
	d     time.Duration
	logAt time.Time
}

// wait sleeps after a failed Accept; false if ctx ended (or the listener was
// closed for good) and the loop should stop.
func (b *acceptBackoff) wait(ctx context.Context, err error, logf func(string, ...any), what string) bool {
	if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
		return false
	}
	if b.d == 0 {
		b.d = 5 * time.Millisecond
	} else if b.d *= 2; b.d > time.Second {
		b.d = time.Second
	}
	if logf != nil && time.Since(b.logAt) >= time.Minute {
		b.logAt = time.Now()
		logf("%s: accept failed: %v (retrying; check the open-files limit if this repeats)", what, err)
	}
	return sleepCtx(ctx, b.d)
}

// ok resets the backoff after a successful Accept.
func (b *acceptBackoff) ok() { b.d = 0 }
