package engine

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
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
func MemHeapDump(prefix string) error {
	if !smux.CountObjects.Load() {
		return fmt.Errorf("HS2_MEM_OBJS is off")
	}
	runtime.GC()
	lf, err := os.Create(prefix + ".labels")
	if err != nil {
		return err
	}
	closed, open := smux.SessionAddrs()
	for _, a := range closed {
		fmt.Fprintf(lf, "%x sessdead\n", a)
	}
	for _, a := range open {
		fmt.Fprintf(lf, "%x sessopen\n", a)
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
		fmt.Fprintf(lf, "%x %s\n", uintptr(unsafe.Pointer(m)), k)
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
		fmt.Fprintf(lf, "%x %s\n", uintptr(unsafe.Pointer(s)), k)
	}
	ms, ss = nil, nil
	if err := lf.Close(); err != nil {
		return err
	}
	df, err := os.Create(prefix + ".dump")
	if err != nil {
		return err
	}
	debug.WriteHeapDump(df.Fd())
	return df.Close()
}
