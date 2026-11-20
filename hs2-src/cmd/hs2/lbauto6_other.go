//go:build !linux

package main

// auto6Watch: no address notifications here; the periodic check does it.
func auto6Watch(stop <-chan struct{}) {}

// No rtnetlink here: auto6DefaultIface falls back to /proc (none outside
// Linux, so "auto6" resolves to nothing; "auto6:IFACE" still works).
func auto6RouteGetIface() string    { return "" }
func auto6MainDefaultIface() string { return "" }
