package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// HS2_PPROF serves profiles on a loopback address and refuses anything else.
func TestPprofLoopbackOnly(t *testing.T) {
	t.Setenv("HS2_PPROF", "0.0.0.0:0")
	startPprof() // refused: logs, serves nothing (nothing to observe but no panic)
	t.Setenv("HS2_PPROF", "127.0.0.1:16061")
	startPprof()
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		if resp, err = http.Get("http://127.0.0.1:16061/debug/pprof/"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := make([]byte, 4096)
	n, _ := resp.Body.Read(b)
	if resp.StatusCode != 200 || !strings.Contains(string(b[:n]), "profile") {
		t.Fatalf("status %d: %q", resp.StatusCode, b[:n])
	}
}
