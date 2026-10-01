package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/encap"
)

// cleanupCmd (`hs2 cleanup`) removes what a stopped daemon left in the kernel:
// the icmp tunnel's reply rules (nft tables / iptables rules) whose owner is no
// longer running, and the global ping-reply setting a dead daemon turned off.
// Rules of tunnels that still run are never touched. The installer runs it
// after deleting, uninstalling or upgrading tunnels.
//
// Rules from older binaries carry no owner PID; they are removed too, but only
// when no older hs2 binary is still running (one could own them).
func cleanupCmd(args []string) {
	legacy := !olderHs2Running()
	removed := encap.SweepStaleEchoGuards(legacy)
	for _, r := range removed {
		fmt.Println("removed:", r)
	}
	if len(removed) == 0 {
		fmt.Println("nothing left behind")
	}
	if !legacy {
		fmt.Println("note: an older hs2 binary is still running; its rules (no owner recorded) were left alone")
	}
}

// olderHs2Running reports whether a running hs2 process uses another binary
// than this one — typically the previous version, whose file was replaced by
// an upgrade (its /proc/<pid>/exe then reads "… (deleted)").
func olderHs2Running() bool {
	self, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return true // cannot tell: be careful
	}
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	for _, d := range dirs {
		if filepath.Base(d) == fmt.Sprint(os.Getpid()) {
			continue
		}
		pid, err := strconv.Atoi(filepath.Base(d))
		if err != nil || !encap.IsHs2Daemon(pid) {
			continue
		}
		exe, err := os.Readlink(d + "/exe")
		if err == nil && exe != self {
			return true
		}
	}
	return false
}
