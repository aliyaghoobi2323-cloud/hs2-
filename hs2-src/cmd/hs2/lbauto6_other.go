//go:build !linux

package main

// auto6Watch: no address notifications here; the periodic check does it.
func auto6Watch(stop <-chan struct{}) {}
