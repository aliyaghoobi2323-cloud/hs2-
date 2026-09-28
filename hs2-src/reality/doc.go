// Package reality implements the probe-resistance half of a Reality-style
// carrier: telling an authorised client (who holds the shared key) apart from
// anyone else, and making "anyone else" — a browser, a scanner, the censor's
// active probe — receive exactly what the real cover site would send.
//
// The threat this defeats is ACTIVE PROBING. A tunnel that merely wraps itself
// in TLS still fails the censor's behavioural test: the probe connects, speaks
// HTTPS, and asks "do you serve real content like the site you claim to be?"
// A plain proxy answers wrongly (or not at all) and is burned. The Reality
// answer is to carry authorised traffic on a hidden signal inside an otherwise
// genuine TLS flow, and to forward everything that lacks that signal to the
// real cover site, so a probe's own request is answered by the real site.
//
// SECURITY NOTE (read before trusting this): the whole scheme rests on the
// authorised signal being (a) unforgeable without the key and (b) invisible to
// someone without it. Getting either wrong silently removes the protection.
// This package is therefore written to be adversary-tested: reality_test.go
// plays the prober and asserts it cannot tell a covered server from the real
// site, and cannot complete the hidden handshake without the key.
package reality
