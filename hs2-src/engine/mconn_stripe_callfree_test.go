package engine

import (
	"testing"
	"time"
)

func TestStripeCallFreeMarks(t *testing.T) {
	old := stripeCallFree
	defer func() { stripeCallFree = old; callLinks.at = nil }()
	a, b := &linkMeter{}, &linkMeter{}
	now := time.Now()

	stripeCallFree = false
	callLinks.at = map[*linkMeter]time.Time{a: now.Add(time.Hour)}
	if callLinkOn(a, now) {
		t.Fatal("key off: no link is a call link")
	}

	stripeCallFree = true
	callLinks.at = map[*linkMeter]time.Time{a: now.Add(callFreeTTL)}
	if !callLinkOn(a, now) || callLinkOn(b, now) || callLinkOn(nil, now) {
		t.Fatal("only the marked link is a call link")
	}
	if callLinkOn(a, now.Add(callFreeTTL+time.Millisecond)) {
		t.Fatal("a mark expires after callFreeTTL")
	}
}

func TestStripeCallFreeMarkOnlyWhileCall(t *testing.T) {
	old := stripeCallFree
	defer func() { stripeCallFree = old; callLinks.at = nil }()
	stripeCallFree = true
	callLinks.at = nil
	m := &mConn{}
	m.callOn = false
	m.callMarkLocked(time.Now())
	if len(callLinks.at) != 0 {
		t.Fatal("a connection not copied as a call marks nothing")
	}
}
