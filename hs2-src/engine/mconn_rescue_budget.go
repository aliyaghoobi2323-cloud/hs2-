package engine

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
)

// ---- rel67b: the records rings' big budget (rrBigMax) sized from RAM -------
//
// cd152 fixed rrBigMax at 64 MB (HS2_MCONN_RESCUE_REC_BIGMB). In production
// (rel66b/rel67, ~4.2k conns on the exit) the budget sat at its cap while a
// 2 Mbit/s call ring costs ~0.75 MB and the exit has 23 GB of RAM. With the
// key unset the budget is now
//
//	max(min(256 MB, 10% of RAM), 2% of RAM), at most 1 GB
//
// (23 GB → ~470 MB, 4 GB → 256 MB, 1 GB → ~102 MB), RAM being MemTotal
// (/proc/meminfo, read once at start) or the process's cgroup memory limit
// when that is smaller (cgroup v2 memory.max, v1 memory.limit_in_bytes, the
// smallest along the cgroup's path). An explicit HS2_MCONN_RESCUE_REC_BIGMB
// wins as before. mb174's tiers (calls 100 %, sustained 60 %, bursts 40 %)
// are shares of this budget. Without a readable MemTotal: the old 64 MB.

const (
	rrBigDefMB   = 64
	rrBigFloorMB = 256
	rrBigCapMB   = 1024
	rrBigPct     = 10
	rrBigMinPct  = 2
)

// rrBigBudget: the default budget (bytes) for ram bytes of memory.
func rrBigBudget(ram int64) int64 {
	if ram <= 0 {
		return rrBigDefMB << 20
	}
	b := max(min(int64(rrBigFloorMB)<<20, ram*rrBigPct/100), ram*rrBigMinPct/100)
	return min(b, int64(rrBigCapMB)<<20)
}

// rrBigFrom: the budget and where it came from, for env (the key's value,
// set: whether present), memTotal and the cgroup limit (0: none).
func rrBigFrom(env string, set bool, memTotal, cgLimit int64) (int64, string) {
	if set {
		if v, err := strconv.Atoi(strings.TrimSpace(env)); err == nil && v > 0 {
			return int64(v) << 20, "env HS2_MCONN_RESCUE_REC_BIGMB"
		}
	}
	ram, src := memTotal, "MemTotal"
	if cgLimit > 0 && (ram <= 0 || cgLimit < ram) {
		ram, src = cgLimit, "cgroup limit"
	}
	if ram <= 0 {
		return rrBigDefMB << 20, "default (no MemTotal)"
	}
	return rrBigBudget(ram), fmt.Sprintf("auto %d MB %s", ram>>20, src)
}

var rrBigSrc string

func rrBigInit() int64 {
	v, set := os.LookupEnv("HS2_MCONN_RESCUE_REC_BIGMB")
	var mt, cg int64
	if !set || envIntDef("HS2_MCONN_RESCUE_REC_BIGMB", 0) == 0 {
		mt = rescueMemTotal()
		cg = cgroupMemLimit(os.ReadFile)
	}
	b, src := rrBigFrom(v, set, mt, cg)
	rrBigSrc = src
	return b
}

// RescueRecBudgetLine: the records rings' budgets, for the startup log.
func RescueRecBudgetLine() string {
	return fmt.Sprintf("mconn rescue-rec: big ring budget %d MB (%s; calls 100%% sustained %d%% bursts %d%%), ring %d KB..%d KB, records budget %d MB",
		rrBigMax>>20, rrBigSrc, rrBigSusPct, rrBigBurstPct, rrRingCap>>10, rrRingMax>>10, rrMemMax>>20)
}

// cgroupMemLimit: the smallest memory limit on this process's cgroup path
// (v2 memory.max, v1 memory.limit_in_bytes), 0 when none.
func cgroupMemLimit(read func(string) ([]byte, error)) int64 {
	b, err := read("/proc/self/cgroup")
	if err != nil {
		return 0
	}
	var best int64
	take := func(file string) {
		d, err := read(file)
		if err != nil {
			return
		}
		s := string(bytes.TrimSpace(d))
		if s == "" || s == "max" {
			return
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v <= 0 || v >= 1<<62 { // v1 "unlimited" is ~2^63
			return
		}
		if best == 0 || v < best {
			best = v
		}
	}
	walk := func(root, p, file string) {
		p = path.Clean("/" + p)
		for {
			take(path.Join(root, p, file))
			if p == "/" {
				return
			}
			p = path.Dir(p)
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		fs := strings.SplitN(sc.Text(), ":", 3)
		if len(fs) != 3 {
			continue
		}
		if fs[0] == "0" && fs[1] == "" {
			walk("/sys/fs/cgroup", fs[2], "memory.max")
			continue
		}
		for _, c := range strings.Split(fs[1], ",") {
			if c == "memory" {
				walk("/sys/fs/cgroup/memory", fs[2], "memory.limit_in_bytes")
			}
		}
	}
	return best
}
