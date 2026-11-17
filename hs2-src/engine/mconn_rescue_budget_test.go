package engine

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestRescueBigBudgetFormula(t *testing.T) {
	const mb, gb = int64(1) << 20, int64(1) << 30
	for _, c := range []struct {
		ram, want int64
	}{
		{512 * mb, 512 * mb / 10},    // 10 %
		{1 * gb, gb / 10},            // ~102 MB
		{2 * gb, 2 * gb / 10},        // ~204 MB
		{4 * gb, 256 * mb},           // the 256 MB floor of the min
		{8 * gb, 256 * mb},           // 2 % = 164 MB < 256 MB
		{16 * gb, 16 * gb * 2 / 100}, // 2 % = ~328 MB
		{23 * gb, 23 * gb * 2 / 100}, // kharej: ~471 MB
		{64 * gb, 1024 * mb},         // 2 % = 1.28 GB, capped at 1 GB
		{0, 64 * mb},                 // unknown: the old default
	} {
		if got := rrBigBudget(c.ram); got != c.want {
			t.Errorf("ram %d MB: budget %d MB, want %d MB", c.ram>>20, got>>20, c.want>>20)
		}
	}
}

func TestRescueBigBudgetSource(t *testing.T) {
	const mb, gb = int64(1) << 20, int64(1) << 30
	if b, src := rrBigFrom("", false, 23*gb, 0); b != 23*gb*2/100 || !strings.Contains(src, "MemTotal") {
		t.Fatalf("no env, 23 GB: %d MB %q", b>>20, src)
	}
	// a cgroup limit smaller than RAM wins
	if b, src := rrBigFrom("", false, 23*gb, 2*gb); b != 2*gb/10 || !strings.Contains(src, "cgroup") {
		t.Fatalf("cgroup 2 GB: %d MB %q", b>>20, src)
	}
	// a cgroup limit above RAM does not
	if b, src := rrBigFrom("", false, 4*gb, 64*gb); b != 256*mb || !strings.Contains(src, "MemTotal") {
		t.Fatalf("cgroup above RAM: %d MB %q", b>>20, src)
	}
	// explicit env wins, whatever RAM says
	if b, src := rrBigFrom("64", true, 23*gb, 0); b != 64*mb || !strings.Contains(src, "env") {
		t.Fatalf("env 64: %d MB %q", b>>20, src)
	}
	if b, _ := rrBigFrom("2048", true, 1*gb, 0); b != 2048*mb {
		t.Fatalf("env 2048: %d MB", b>>20)
	}
	// an invalid env value falls back to auto
	if b, src := rrBigFrom("x", true, 4*gb, 0); b != 256*mb || !strings.Contains(src, "auto") {
		t.Fatalf("env x: %d MB %q", b>>20, src)
	}
	if b, src := rrBigFrom("", false, 0, 0); b != 64*mb || !strings.Contains(src, "default") {
		t.Fatalf("no RAM: %d MB %q", b>>20, src)
	}
	if b, src := rrBigFrom("", false, 0, 1*gb); b != gb/10 || !strings.Contains(src, "cgroup") {
		t.Fatalf("cgroup only: %d MB %q", b>>20, src)
	}
}

func fakeFS(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if v, ok := m[p]; ok {
			return []byte(v), nil
		}
		return nil, fs.ErrNotExist
	}
}

func TestRescueBigBudgetCgroup(t *testing.T) {
	// v2: the unit's own limit and a smaller parent slice limit
	v2 := fakeFS(map[string]string{
		"/proc/self/cgroup": "0::/system.slice/hs2.service\n",
		"/sys/fs/cgroup/system.slice/hs2.service/memory.max": "4294967296\n",
		"/sys/fs/cgroup/system.slice/memory.max":             "2147483648\n",
	})
	if got := cgroupMemLimit(v2); got != 2<<30 {
		t.Fatalf("v2: %d", got)
	}
	// v2 unlimited
	if got := cgroupMemLimit(fakeFS(map[string]string{
		"/proc/self/cgroup": "0::/system.slice/hs2.service\n",
		"/sys/fs/cgroup/system.slice/hs2.service/memory.max": "max\n",
	})); got != 0 {
		t.Fatalf("v2 max: %d", got)
	}
	// v2 in a container (namespace root)
	if got := cgroupMemLimit(fakeFS(map[string]string{
		"/proc/self/cgroup":         "0::/\n",
		"/sys/fs/cgroup/memory.max": "1073741824\n",
	})); got != 1<<30 {
		t.Fatalf("v2 root: %d", got)
	}
	// v1: limit, and v1's "unlimited" value
	if got := cgroupMemLimit(fakeFS(map[string]string{
		"/proc/self/cgroup": "12:cpu,cpuacct:/x\n4:memory:/system.slice/hs2.service\n",
		"/sys/fs/cgroup/memory/system.slice/hs2.service/memory.limit_in_bytes": "3221225472\n",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes":                          "9223372036854771712\n",
	})); got != 3<<30 {
		t.Fatalf("v1: %d", got)
	}
	if got := cgroupMemLimit(func(string) ([]byte, error) { return nil, errors.New("no") }); got != 0 {
		t.Fatalf("unreadable: %d", got)
	}
}

func TestRescueBigBudgetLine(t *testing.T) {
	l := RescueRecBudgetLine()
	if !strings.Contains(l, "big ring budget") || rrBigMax <= 0 || rrBigSrc == "" {
		t.Fatalf("line %q (max %d src %q)", l, rrBigMax, rrBigSrc)
	}
	t.Log(l)
}
