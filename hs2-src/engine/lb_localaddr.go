package engine

import (
	"errors"
	"io"
	"strings"
	"syscall"
	"time"
)

// b333: a path whose SOURCE address is not on this server.
//
// A dial bound to an address the host no longer has (a Hetzner VM gets a new
// global IPv6 at every boot; the config still names the old one) fails at
// once with "bind: cannot assign requested address". Before b333 such a path
// was treated like any refusing path: never cooled inside the start grace, and
// past it only after lbFailMax failures with the usual cooldown, so the slots
// kept redialing it (31 failed dials in about a minute in production) and the
// log kept a line per batch. Such a failure says nothing about the peer or the
// network — the path cannot work until the address comes back — so it is
// cooled at once with a doubling cooldown (lbLocalCool .. lbLocalCoolMax),
// whatever the start grace or the rest of the pool say, and logged at most
// once per lbLocalLogEvery per path (with the number of failures folded in).
// The primary path is never cooled (as before).

var (
	lbLocalCool     = 15 * time.Second
	lbLocalCoolMax  = 5 * time.Minute
	lbLocalLogEvery = 60 * time.Second
)

// localAddrErr reports whether err is a dial failure because the source
// address is not (or no longer) an address of this host.
func localAddrErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		return true
	}
	return strings.Contains(err.Error(), "cannot assign requested address")
}

// LocalAddrErr is localAddrErr for the command (the auto6 resolver's kick).
func LocalAddrErr(err error) bool { return localAddrErr(err) }

// localAddrFailLocked cools path i after a source-address failure. Called with
// b.mu held; reports whether it handled err.
func (b *LB) localAddrFailLocked(i int, err error) bool {
	p := b.paths[i]
	if p.Primary || !localAddrErr(err) {
		return false
	}
	if b.localLog == nil {
		b.localLog = map[string]time.Time{}
		b.localFailN = map[string]int{}
	}
	name := p.Name()
	b.localFailN[name]++
	sh := b.fails[i] - 1
	if sh < 0 {
		sh = 0
	}
	if sh > 16 {
		sh = 16
	}
	cd := lbLocalCool << sh
	if cd > lbLocalCoolMax || cd <= 0 {
		cd = lbLocalCoolMax
	}
	now := b.now()
	b.until[i] = now.Add(cd)
	if i < len(b.backoff) {
		b.backoff[i] = time.Time{}
	}
	if last, ok := b.localLog[name]; !ok || now.Sub(last) >= lbLocalLogEvery {
		b.log("lb: path %s: source %s is not an address of this server (%d failed dial(s): %v) — path cooled for %s", name, p.Local, b.localFailN[name], err, fmtDur(cd))
		b.localLog[name] = now
		b.localFailN[name] = 0
	}
	return true
}

// CloseLinksFrom closes every live link whose path dials from source address
// local (the address left this host: its links cannot carry traffic any more).
// It returns how many it closed. The slots then redial on the current paths.
func (b *LB) CloseLinksFrom(local string) int {
	b.mu.Lock()
	var cs []io.Closer
	for k, i := range b.held {
		if i < len(b.paths) && b.paths[i].Local == local {
			if c, ok := k.(io.Closer); ok {
				cs = append(cs, c)
				if b.closeWhy == nil {
					b.closeWhy = map[any]string{}
				}
				b.closeWhy[k] = "closed: its source address " + local + " left this server"
			}
		}
	}
	b.mu.Unlock()
	for _, c := range cs {
		c.Close()
	}
	return len(cs)
}

// closeReason is why CloseLinksFrom closed link ("" = it did not), for the
// link-down line (instead of a misleading keepalive reason).
func (b *LB) closeReason(link any) string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closeWhy[link]
}
