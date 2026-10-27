package engine

import (
	"fmt"
	"testing"
	"time"
)

func TestDbgTwoWay(t *testing.T) {
	r := 72 * time.Millisecond
	dn := &budgetDir{c: 100 * bMbit, rate: 100 * bMbit}
	up := &budgetDir{c: 20 * bMbit, rate: 20 * bMbit}
	for i := 0; i < 160; i++ {
		wd, bd := dn.inflight(r, up.q)
		wu, bu := up.inflight(r, dn.q)
		qd, qu := dn.q, up.q
		dn.settle(wd, r, qu)
		up.settle(wu, r, qd)
		dn.read(r, up.q, bd, up, 0, 0, true)
		up.read(r, dn.q, bu, dn, 0, 0, true)
		if i%4 == 0 || i < 20 {
			fmt.Printf("%3d dn %5.1f q %5.2f B %7.0f %v %s | up %5.1f q %5.2f B %7.0f %v %s\n", i, dn.rate/bMbit, msOf(dn.q), dn.b, bd, dn.core.mode, up.rate/bMbit, msOf(up.q), up.b, bu, up.core.mode)
		}
	}
}
