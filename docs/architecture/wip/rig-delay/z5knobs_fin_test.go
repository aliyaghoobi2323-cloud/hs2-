package engine

// delay rig (scratch), built into the final tree: z5QueueOf reads the
// session's QueueDelay for the tracer.

import (
	"reflect"
	"time"
	"unsafe"

	"github.com/xtaci/smux"
)

func init() {
	z5QueueOf = func(s *smux.Session) float64 {
		v := reflect.ValueOf(s).Elem().FieldByName("config")
		cfg := reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface().(*smux.Config)
		if cfg.QueueDelay == nil {
			return -1
		}
		return float64(cfg.QueueDelay()) / float64(time.Millisecond)
	}
}
