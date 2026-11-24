package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/sdnotify"
)

// systemd notifications (zdu350 §7, slot S2, correction R3).
//
// Every ZDU binary sends READY=1 whenever $NOTIFY_SOCKET is set, independent
// of HS2_ZDU, so a unit with the 30-zdu.conf drop-in (Type=notify) never
// depends on the key. Without $NOTIFY_SOCKET (today's Type=simple units)
// every call here is a no-op and logs nothing, so behaviour and logs are
// exactly the pre-ZDU ones.
//
// READY means "the local side is up": TUN open, carrier listener and user
// ports bound, link pool started. It does not wait for a link: a peer that is
// down (or the hourly xray restart on the Kharej side) must never hold the
// unit in "activating" until TimeoutStartSec kills it. Link health is the
// live handover's probation (slot S6), not READY's job.

// zduFeatures lists the feature flags `hs2 version --features` prints. The
// safe-deploy script reads it: "zdu" = this binary sends READY under
// Type=notify, so 30-zdu.conf may stay installed; a binary without it (rel69p
// and older) needs the drop-in removed before it starts (R3).
// "zduwin" (b430): `hs2 zdu-window` answers the end-of-hour check.
var zduFeatures = []string{"zdu", "zduwin"}

// featuresLine is the second line of `hs2 version --features`.
func featuresLine() string { return "features: " + strings.Join(zduFeatures, " ") }

var (
	readyOnce    sync.Once
	stoppingOnce sync.Once
	notifyLogf   = log.Printf
)

// notifyReady sends READY=1 (once per process) with a STATUS line naming what
// is up. Safe to call from several start paths.
func notifyReady(what string) {
	zduTakeReady(notifyLogf) // ZDU S6: a new process answers the old one first (no-op otherwise)
	readyOnce.Do(func() {
		sent, err := sdnotify.Notify("READY=1\nSTATUS=hs2 up: " + strings.ReplaceAll(what, "\n", " "))
		switch {
		case err != nil:
			notifyLogf("systemd: READY not sent: %v", err)
		case sent:
			notifyLogf("systemd: READY sent (%s)", what)
		}
	})
}

// notifyStopping sends STOPPING=1 (once) when shutdown begins.
func notifyStopping() {
	stoppingOnce.Do(func() {
		sent, err := sdnotify.Notify("STOPPING=1\nSTATUS=hs2 stopping")
		switch {
		case err != nil:
			notifyLogf("systemd: STOPPING not sent: %v", err)
		case sent:
			notifyLogf("systemd: STOPPING sent")
		}
	})
}

// exitOnCancel is runCmd's hard shutdown guarantee: when a signal cancels ctx,
// tell systemd STOPPING=1 (no-op without $NOTIFY_SOCKET), then exit after
// grace no matter what a goroutine is doing.
func exitOnCancel(ctx context.Context, grace time.Duration, exit func()) {
	<-ctx.Done()
	notifyStopping()
	time.Sleep(grace)
	exit()
}
