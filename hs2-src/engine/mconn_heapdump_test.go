package engine

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xtaci/smux"
)

// b376: a failed heap dump leaves no partial file behind; a good one is 0600.
func TestMemHeapDumpCleansUpOnError(t *testing.T) {
	old := smux.CountObjects.Load()
	smux.CountObjects.Store(true)
	defer smux.CountObjects.Store(old)
	d := t.TempDir()

	p := filepath.Join(d, "ok")
	if err := MemHeapDump(p); err != nil {
		t.Fatalf("dump: %v", err)
	}
	for _, s := range []string{".labels", ".dump"} {
		fi, err := os.Stat(p + s)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", s, fi, err)
		}
	}

	// the .dump already exists (O_EXCL): the .labels written before is removed
	p = filepath.Join(d, "excl")
	os.WriteFile(p+".dump", []byte("x"), 0o600)
	if err := MemHeapDump(p); err == nil {
		t.Fatal("dump over an existing file accepted")
	}
	if _, err := os.Stat(p + ".labels"); err == nil {
		t.Fatal("partial .labels left behind")
	}

	// not enough space: refused before anything is written
	defer func(f func(string) error) { heapDumpSpaceCheck = f }(heapDumpSpaceCheck)
	heapDumpSpaceCheck = func(string) error { return errors.New("full") }
	p = filepath.Join(d, "full")
	if err := MemHeapDump(p); err == nil {
		t.Fatal("dump with no space accepted")
	}
	if m, _ := filepath.Glob(p + "*"); len(m) != 0 {
		t.Fatalf("files left: %v", m)
	}
}

// b378: WriteHeapDump hides ENOSPC; a dump shorter than the live heap or one
// that leaves the disk full is removed with an error. A real dump is at least
// the live heap.
func TestMemHeapDumpTruncated(t *testing.T) {
	old := smux.CountObjects.Load()
	smux.CountObjects.Store(true)
	defer smux.CountObjects.Store(old)
	d := t.TempDir()
	keep := make([]byte, 8<<20) // some live heap
	p := filepath.Join(d, "real")
	if err := MemHeapDump(p); err != nil {
		t.Fatalf("dump: %v", err)
	}
	fi, _ := os.Stat(p + ".dump")
	if min := heapDumpMinSize(); fi == nil || fi.Size() < int64(min) || fi.Size() < int64(len(keep)) {
		t.Fatalf("real dump %v smaller than the live heap (min %d)", fi, min)
	}
	runtime.KeepAlive(keep)

	defer func(f func() uint64) { heapDumpMinSize = f }(heapDumpMinSize)
	heapDumpMinSize = func() uint64 { return 1 << 40 }
	p = filepath.Join(d, "short")
	if err := MemHeapDump(p); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("short dump: %v", err)
	}
	if m, _ := filepath.Glob(p + "*"); len(m) != 0 {
		t.Fatalf("files left: %v", m)
	}
	heapDumpMinSize = func() uint64 { return 0 }

	defer func(f func(string) (uint64, error)) { heapDumpFreeAfter = f }(heapDumpFreeAfter)
	heapDumpFreeAfter = func(string) (uint64, error) { return 4096, nil }
	p = filepath.Join(d, "nofree")
	if err := MemHeapDump(p); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("disk full after dump: %v", err)
	}
	if m, _ := filepath.Glob(p + "*"); len(m) != 0 {
		t.Fatalf("files left: %v", m)
	}
}
