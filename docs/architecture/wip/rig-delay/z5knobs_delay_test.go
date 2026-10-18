package engine

// delay rig knobs (scratch), built only into trees with the queue-delay
// gain: Z5_QSIG (SmuxQueueSignal: rcv, srtt, ctrl, off), Z5_GAIN /
// Z5_GMIN (the gain's bounds), Z5_QLO / Z5_QHI (queue thresholds, fractions
// of the round trip).

import (
	"os"
	"reflect"
	"strconv"
	"time"
	"unsafe"

	"github.com/xtaci/smux"
)

func init() {
	if v := os.Getenv("Z5_QSIG"); v != "" {
		SmuxQueueSignal = v
	}
	f := func(k string) float64 {
		v, _ := strconv.ParseFloat(os.Getenv(k), 64)
		return v
	}
	smux.SetNeedGain(f("Z5_GAIN"), f("Z5_GMIN"), f("Z5_QLO"), f("Z5_QHI"))
	smux.SetNeedSteps(int(f("Z5_FINE")), f("Z5_USHARE"), f("Z5_MMAX"), f("Z5_MMIN"))
	if os.Getenv("Z5_STEPOLD") == "1" {
		smux.SetStepOld(true)
	}
	if v := f("Z5_STEPDIV"); v > 0 {
		smux.SetStepDiv(v)
	}
	if v := f("Z5_PGAP"); v > 0 {
		smux.SetPauseGap(v)
	}
	if v := f("Z5_PSHARE"); v > 0 {
		smux.SetPauseShare(v)
	}
	if os.Getenv("Z5_QC") == "0" {
		smux.SetComputedQueue(false)
	}
	if os.Getenv("Z5_PAUSE") == "0" {
		smux.SetPauses(false)
	}
	if v := f("Z5_QTAU"); v > 0 {
		linkQueueTau = time.Duration(v * float64(time.Millisecond))
	}
	if v := os.Getenv("Z5_QOVER"); v != "" {
		smux.SetQueueOver(f("Z5_QOVER"))
	}
	z5QueueOf = func(s *smux.Session) float64 {
		v := reflect.ValueOf(s).Elem().FieldByName("config")
		cfg := reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Interface().(*smux.Config)
		if cfg.QueueDelay == nil {
			return -1
		}
		return float64(cfg.QueueDelay()) / float64(time.Millisecond)
	}
}
