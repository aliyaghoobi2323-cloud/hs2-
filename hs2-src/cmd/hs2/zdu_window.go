package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/engine"
	"github.com/hosseintaghipoursori-alt/hs2-tunnel/handoff"
)

// b430: one end-of-hour rule for hs2-zdu-upgrade.sh and the process. The
// script refused only 2 min before the xray window while the process (its
// SIGUSR2 handler) refuses READY timeout + probation + margin (2m30s by
// default) before it, so between HH:56:30 and HH:57:59 the script started a
// handover the process then refused. Now the script asks the running binary
// (`hs2 zdu-window`, feature "zduwin"), which answers with the very check
// zduOld.handover makes.

// zduHandoverSpan is how long after SIGUSR2 a handover may still be before
// its commit: the READY wait (at least the warm wait), the probation and a
// margin. The window must not open inside it.
func zduHandoverSpan(ready, probation time.Duration) time.Duration {
	return max(ready, zduWarmMax) + probation + zduSpanMargin
}

// zduProbationEnv is the probation the process uses (HS2_ZDU_PROBATION, lab
// drills only; a bad or non-positive value keeps the default).
func zduProbationEnv() time.Duration {
	if v := os.Getenv("HS2_ZDU_PROBATION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return zduProbation
}

// zduWindowLine is the verdict for a handover asked for at now, slack later
// (the script's own remaining steps before its SIGUSR2): "OK ..." or the
// refusal ("window ..."), as the script prints it after "REFUSED ".
func zduWindowLine(now time.Time, slack, ready, probation time.Duration) string {
	u := now.UTC()
	sm := engine.ZDUStormMin()
	if engine.ZDURefused(u) || engine.ZDURefused(u.Add(slack)) {
		return fmt.Sprintf("window HH:%02d-HH:%02d UTC (minute %d)", (sm+59)%60, (sm+2)%60, u.Minute())
	}
	span := zduHandoverSpan(ready, probation)
	if engine.ZDURefusedSpan(u, span+slack) {
		return fmt.Sprintf("window opens in <= %v (minute %d): READY + probation + margin (%v) + %v would run into the restart storm",
			span+slack, u.Minute(), span, slack)
	}
	return fmt.Sprintf("OK span=%v slack=%v storm_min=%d", span, slack, sm)
}

// zduWindowCmd: `hs2 zdu-window [UNIX_NOW [SLACK_SECONDS]]`, run by
// hs2-zdu-upgrade.sh with the running process's HS2_ZDU_STORM_MIN and
// HS2_ZDU_PROBATION. Prints one line; exit 0 OK, 4 refused, 2 usage.
func zduWindowCmd(args []string) {
	now, slack := time.Now(), time.Duration(0)
	if len(args) > 0 {
		n, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || n <= 0 {
			fmt.Println("usage: hs2 zdu-window [UNIX_NOW [SLACK_SECONDS]]")
			os.Exit(2)
		}
		now = time.Unix(n, 0)
	}
	if len(args) > 1 {
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 0 || n > 3600 {
			fmt.Println("usage: hs2 zdu-window [UNIX_NOW [SLACK_SECONDS]]")
			os.Exit(2)
		}
		slack = time.Duration(n) * time.Second
	}
	l := zduWindowLine(now, slack, handoff.ReadyTimeout, zduProbationEnv())
	fmt.Println(l)
	if l[:2] != "OK" {
		os.Exit(4)
	}
}
