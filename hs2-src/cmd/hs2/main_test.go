package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A probe forwarded to the builtin backend gets a plain, unremarkable static
// site — NOT the nginx default welcome page (a honeypot signature), and NOT a
// "Server: nginx" header (our TLS terminator is Go's, so claiming nginx only
// contradicts it; a Go server like Caddy omits the header).
func TestBuiltinBackendServesCover(t *testing.T) {
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
	if got := resp.Header.Get("Server"); got != "" {
		t.Fatalf("Server header = %q, want none", got)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("unexpected content-type %q", ct)
	}
	// http.ServeContent should give real static-server headers.
	if resp.Header.Get("Last-Modified") == "" {
		t.Fatal("no Last-Modified header (expected from http.ServeContent)")
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if strings.Contains(s, "nginx") {
		t.Fatalf("cover page still mentions nginx: %q", s)
	}
	if !strings.Contains(s, "Oakline") || !strings.Contains(s, "<!doctype html>") {
		t.Fatalf("cover page is not the expected static site: %q", s)
	}
}

// If-Modified-Since on an unchanged page gets a 304, like any static server
// (confirms we serve via http.ServeContent rather than a bare Write).
func TestBuiltinBackendConditional304(t *testing.T) {
	addr, err := startBuiltinBackend()
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "http://"+addr+"/", nil)
	req.Header.Set("If-Modified-Since", "Fri, 01 Jan 2100 00:00:00 GMT")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", resp.StatusCode)
	}
}

// A non-root path returns a plain Go 404, like a default static server.
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
	if got := resp.Header.Get("Server"); got != "" {
		t.Fatalf("Server header = %q, want none", got)
	}
}
