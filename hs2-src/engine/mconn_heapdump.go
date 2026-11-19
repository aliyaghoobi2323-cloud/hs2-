package engine

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"unsafe"
	"weak"

	"github.com/xtaci/smux"
)

// mem69 (HS2_MEM_OBJS diagnosis): every msub held weakly, so a heap dump can
// be searched for what still references the dead ones and their sessions.
var memMSubs struct {
	mu sync.Mutex
	m  map[weak.Pointer[msub]]struct{}
}

func memRegMSub(s *msub) {
	if !smux.CountObjects.Load() {
		return
	}
	memMSubs.mu.Lock()
	if memMSubs.m == nil {
		memMSubs.m = map[weak.Pointer[msub]]struct{}{}
	}
	memMSubs.m[weak.Make(s)] = struct{}{}
	memMSubs.mu.Unlock()
}

// MemHeapDump (diagnosis, HS2_MEM_OBJS with HS2_PPROF): after a GC, writes
// prefix+".labels" (address and kind of every session, mConn and msub still
// in memory) and prefix+".dump" (runtime/debug.WriteHeapDump).
//
// b376: it is refused when the target filesystem has less free space than
// the heap (heapDumpSpaceCheck), and on any error both files are removed, so
// a failed dump never leaves a partial file holding key material behind.
// b378: a dump shorter than the live heap, or one that left the filesystem
// (nearly) full, is treated as truncated: removed, and an error.
func MemHeapDump(prefix string) (err error) {
	if !smux.CountObjects.Load() {
		return fmt.Errorf("HS2_MEM_OBJS is off")
	}
	runtime.GC()
	if err := heapDumpSpaceCheck(filepath.Dir(prefix)); err != nil {
		return err
	}
	var made []string
	defer func() {
		if err != nil {
			for _, p := range made {
				os.Remove(p)
			}
		}
	}()
	lf, err := os.OpenFile(prefix+".labels", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // sec370: owner only, never over an existing file
	if err != nil {
		return err
	}
	made = append(made, prefix+".labels")
	lw := &errWriter{w: lf}
	closed, open := smux.SessionAddrs()
	for _, a := range closed {
		fmt.Fprintf(lw, "%x sessdead\n", a)
	}
	for _, a := range open {
		fmt.Fprintf(lw, "%x sessopen\n", a)
	}
	memMConns.mu.Lock()
	var ms []*mConn
	for w := range memMConns.m {
		if m := w.Value(); m != nil {
			ms = append(ms, m)
		}
	}
	memMConns.mu.Unlock()
	for _, m := range ms {
		m.mu.Lock()
		k := "mconnlive"
		if m.ended {
			k = "mconnended"
		}
		m.mu.Unlock()
		fmt.Fprintf(lw, "%x %s\n", uintptr(unsafe.Pointer(m)), k)
	}
	memMSubs.mu.Lock()
	var ss []*msub
	for w := range memMSubs.m {
		if s := w.Value(); s != nil {
			ss = append(ss, s)
		} else {
			delete(memMSubs.m, w)
		}
	}
	memMSubs.mu.Unlock()
	for _, s := range ss {
		s.m.mu.Lock()
		k := "msublive"
		if s.dead {
			k = "msubdead"
		}
		s.m.mu.Unlock()
		fmt.Fprintf(lw, "%x %s\n", uintptr(unsafe.Pointer(s)), k)
	}
	ms, ss = nil, nil
	if cerr := lf.Close(); lw.err != nil || cerr != nil {
		if lw.err != nil {
			return lw.err
		}
		return cerr
	}
	df, err := os.OpenFile(prefix+".dump", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	made = append(made, prefix+".dump")
	minSize := heapDumpMinSize()
	debug.WriteHeapDump(df.Fd()) // reports no error: Sync/Close below catch some, the size and space checks the rest
	serr := df.Sync()
	fi, ferr := df.Stat()
	if cerr := df.Close(); serr != nil || cerr != nil || ferr != nil {
		if serr != nil {
			return serr
		}
		if ferr != nil {
			return ferr
		}
		return cerr
	}
	// b378: WriteHeapDump drops write errors, so an ENOSPC that raced the
	// space check would leave a truncated dump with err == nil. A dump holds
	// every live object, so it is at least about the live heap; shorter, or
	// the filesystem now full, means it was cut: removed, and an error.
	if fi.Size() < int64(minSize) {
		return fmt.Errorf("heap dump truncated: %d bytes, the live heap is about %d (disk full?) — removed", fi.Size(), minSize)
	}
	if free, err := heapDumpFreeAfter(filepath.Dir(prefix)); err == nil && free < heapDumpFreeFloor {
		return fmt.Errorf("heap dump probably truncated: %s has %d bytes free after it — removed", filepath.Dir(prefix), free)
	}
	return nil
}

// heapDumpMinSize (b378): the smallest plausible dump, 3/4 of the live heap
// (HeapAlloc right after the GC; the dump itself is a stop-the-world copy of
// every live object plus headers, so it is larger; the margin covers a GC
// cycle freeing objects between this read and the dump).
var heapDumpMinSize = func() uint64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc / 4 * 3
}

// heapDumpFreeAfter / heapDumpFreeFloor (b378): free bytes left on the dump's
// filesystem after writing it; under 1 MB the write very likely hit ENOSPC.
var heapDumpFreeAfter = func(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

const heapDumpFreeFloor = 1 << 20

// errWriter keeps the first write error.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(b []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(b)
	e.err = err
	return n, err
}

// heapDumpSpaceCheck: the dump is about the size of the heap; refuse when the
// filesystem holding dir has less free space than the runtime's Sys memory.
var heapDumpSpaceCheck = func(dir string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return err
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	free := uint64(st.Bavail) * uint64(st.Bsize)
	if free < ms.Sys {
		return fmt.Errorf("heap dump refused: %s has %d MB free, the heap needs about %d MB", dir, free>>20, ms.Sys>>20)
	}
	return nil
}
