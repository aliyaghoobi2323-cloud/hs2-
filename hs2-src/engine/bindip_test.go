package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hosseintaghipoursori-alt/hs2-tunnel/core"
)

// A typo in bind_local_ip must fail loudly, never fall back to the default
// source: on a multi-IP server that default may be the filtered address.
func TestDialCarrierRejectsInvalidBindIP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := dialCarrier(ctx, "127.0.0.1:9", "10.20.0.1533", core.StaticKey{}, nil, nil, 0)
	if err == nil || !strings.Contains(err.Error(), "10.20.0.1533") {
		t.Fatalf("dialCarrier err = %v, want invalid-IP error", err)
	}
}
