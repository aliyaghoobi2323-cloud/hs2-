//go:build !linux

package main

// diskUsedPct: unknown off linux (the watchdog runs on linux servers).
func diskUsedPct(paths ...string) (float64, string) { return -1, "" }
