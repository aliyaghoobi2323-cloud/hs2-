//go:build linux

package main

import "syscall"

// diskUsedPct: the fullest of the given mount points (used % as df shows it).
func diskUsedPct(paths ...string) (float64, string) {
	best, where := -1.0, ""
	for _, p := range paths {
		var fs syscall.Statfs_t
		if syscall.Statfs(p, &fs) != nil || fs.Blocks == 0 {
			continue
		}
		used := float64(fs.Blocks - fs.Bfree)
		avail := float64(fs.Bavail)
		if used+avail <= 0 {
			continue
		}
		if pct := 100 * used / (used + avail); pct > best {
			best, where = pct, p
		}
	}
	return best, where
}
