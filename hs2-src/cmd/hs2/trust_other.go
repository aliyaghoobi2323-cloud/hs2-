//go:build !linux

package main

import (
	"errors"
	"os"
)

var trustExempt = func(dir string) bool { return false }

// sidecarTrust: the dir-fd/filesystem checks are Linux-only; elsewhere a
// sidecar is never trusted (pass --sha256 HEX).
func sidecarTrust(src string, srcFI os.FileInfo) (string, string, bool) {
	if _, err := os.Lstat(src + ".sha256"); errors.Is(err, os.ErrNotExist) {
		return "", "", false
	}
	return "", "sidecar trust needs Linux", true
}
