//go:build !linux

package main

import "testing"

func trustTestDir(t *testing.T, d string) { t.Skip("sidecar trust is Linux-only") }
