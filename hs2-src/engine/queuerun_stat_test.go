package engine

import (
	"testing"
	"time"
)

// prod73: the queue controller's status counters follow a lane's cap.
func TestQCtlStat(t *testing.T) {
	var s qCtlStat
	l := &qLane{st: &s}
	t0 := time.Now()
	l.setCap(0, t0)
	l.setCap(1e6, t0.Add(2*time.Second))
	l.setCap(1e6, t0.Add(4*time.Second))
	l.setCap(5e5, t0.Add(6*time.Second))
	if s.starts.Load() != 1 || s.changes.Load() != 1 || s.capped.Load() != 1 || s.rateBps.Load() != 5e5 {
		t.Fatalf("capped: starts %d changes %d capped %d rate %d", s.starts.Load(), s.changes.Load(), s.capped.Load(), s.rateBps.Load())
	}
	l.setCap(0, t0.Add(8*time.Second))
	if s.releases.Load() != 1 || s.capped.Load() != 0 || s.rateBps.Load() != 0 || s.cappedMs.Load() != 6000 {
		t.Fatalf("released: releases %d capped %d rate %d ms %d", s.releases.Load(), s.capped.Load(), s.rateBps.Load(), s.cappedMs.Load())
	}
}
