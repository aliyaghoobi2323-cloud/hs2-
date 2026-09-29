package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A probe forwarded to the builtin backend must get the ubiquitous nginx default
// page with an nginx Server header, so it looks like the most common site on the
// internet rather than a bespoke placeholder.
func TestBuiltinBackendLooksLikeNginx(t *testing.T) {
	addr, err := startBuiltinBackend()
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Server"); got != "nginx" {
		t.Fatalf("Server header = %q, want nginx", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Welcome to nginx!") {
		t.Fatalf("body is not the nginx default page: %q", body)
	}
	if resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("unexpected content-type %q", resp.Header.Get("Content-Type"))
	}
}

// A non-root path returns a plain 404, like a default static server.
func TestBuiltinBackend404(t *testing.T) {
	addr, err := startBuiltinBackend()
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/secret")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("Server"); got != "nginx" {
		t.Fatalf("Server header = %q, want nginx", got)
	}
}
