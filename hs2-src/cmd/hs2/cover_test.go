package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

var fixedClock = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func coverHash(seed string) string {
	p, _ := buildCover(seed, fixedClock)
	s := sha256.Sum256(p)
	return hex.EncodeToString(s[:])
}

// req 6b: the page is a pure function of the seed — same seed, same bytes, on
// every call (so across restarts and rebuilds). Pinned so any accidental change
// to the generator fails here.
func TestCoverDeterministic(t *testing.T) {
	const seed = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	h1 := coverHash(seed)
	for i := 0; i < 20; i++ {
		if coverHash(seed) != h1 {
			t.Fatal("same seed produced different bytes")
		}
	}
	// Golden: the exact hash for this seed at fixedClock. If the generator's
	// output changes, this fails and forces a conscious decision (every
	// server's page would otherwise change on upgrade day — a correlated fleet
	// event). To change the generator on purpose, add a versioned path rather
	// than editing in place, then update this.
	const golden = "d655972ed474346ac5367e6e7278b91e2ad2f65d5ccadfca6f2f5daa4de6984f"
	if h1 != golden {
		t.Fatalf("generator output drifted: got %s, want %s", h1, golden)
	}
}

// req 6a: two different seeds never share a hash. Also a smoke test that the
// space is large — 500 seeds give 500 distinct pages.
func TestCoverSeedsDiffer(t *testing.T) {
	seen := map[string]string{}
	for i := 0; i < 500; i++ {
		seed := hex.EncodeToString([]byte{byte(i), byte(i >> 8), 0x5a, 0xa5, byte(i * 7), byte(i * 13), byte(i * 31), 0x11})
		h := coverHash(seed)
		if prev, ok := seen[h]; ok {
			t.Fatalf("hash collision: seed %s and %s both -> %s", prev, seed, h)
		}
		seen[h] = seed
	}
}

// req: an empty seed yields the fixed legacy page, byte-for-byte. And that page
// still hashes to what the field test observed on the shipped binary
// (86dfcf86…), so an un-migrated install is not disturbed.
func TestCoverSeedlessIsLegacy(t *testing.T) {
	p, off := buildCover("", fixedClock)
	if off != 37 {
		t.Fatalf("legacy Last-Modified offset = %d, want 37", off)
	}
	if string(p) != string(coverFixedPage) {
		t.Fatal("empty seed did not return the fixed legacy page")
	}
	s := sha256.Sum256(p)
	got := hex.EncodeToString(s[:])
	if !strings.HasPrefix(got, "86dfcf86") {
		t.Fatalf("legacy page hash changed: %s (want 86dfcf86…) — backward compatibility broken", got)
	}
}

// Every generated page must keep the structural invariants of a plausible
// self-contained static site: valid-ish HTML shell, no external requests, no
// JavaScript, an inline favicon, light AND dark CSS, a seed-derived
// Last-Modified offset, and a size that varies.
func TestCoverStructuralInvariants(t *testing.T) {
	extReq := regexp.MustCompile(`(?i)(src|href)\s*=\s*"https?://`)
	sizes := map[int]bool{}
	for i := 0; i < 120; i++ {
		seed := hex.EncodeToString([]byte{0xC0, 0xFF, 0xEE, byte(i), byte(i >> 2), byte(i * 5), byte(i * 9), byte(i)})
		p, off := buildCover(seed, fixedClock)
		s := string(p)
		sizes[len(p)] = true
		if off < 18 || off > 400 {
			t.Fatalf("seed %s: Last-Modified offset %d out of range", seed, off)
		}
		for _, must := range []string{"<!doctype html>", "<html lang=\"en\">", "</html>", "prefers-color-scheme:dark", "data:image/svg+xml,", "id=\"contact\"", "id=\"services\""} {
			if !strings.Contains(s, must) {
				t.Fatalf("seed %s: missing %q", seed, must)
			}
		}
		if strings.Contains(strings.ToLower(s), "<script") {
			t.Fatalf("seed %s: cover page must have no JavaScript", seed)
		}
		if extReq.MatchString(s) {
			t.Fatalf("seed %s: cover page must make no external requests", seed)
		}
		// Balanced section tags (every <section opens and closes).
		if strings.Count(s, "<section") != strings.Count(s, "</section>") {
			t.Fatalf("seed %s: unbalanced <section> tags", seed)
		}
	}
	if len(sizes) < 20 {
		t.Fatalf("page size barely varies across 120 seeds (%d distinct) — too uniform", len(sizes))
	}
}

// Served over HTTP the generated page behaves like a static file server:
// 200 with the right content-type and an ETag, a conditional 304, and a plain
// 404 off the root — the same contract tested for the fixed page.
func TestCoverServedHeaders(t *testing.T) {
	addr, err := startBuiltinBackend("deadbeefcafe0001deadbeefcafe0002")
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Server") != "" {
		t.Fatalf("Server header present: %q", resp.Header.Get("Server"))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type %q", ct)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" || resp.Header.Get("Last-Modified") == "" {
		t.Fatalf("missing ETag or Last-Modified (etag=%q)", etag)
	}

	req, _ := http.NewRequest("GET", "http://"+addr+"/", nil)
	req.Header.Set("If-None-Match", etag)
	r2, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match: status %d, want 304", r2.StatusCode)
	}

	r3, err := c.Get("http://" + addr + "/robots.txt")
	if err != nil {
		t.Fatal(err)
	}
	r3.Body.Close()
	if r3.StatusCode != http.StatusNotFound {
		t.Fatalf("non-root path: status %d, want 404", r3.StatusCode)
	}
}

// The generated backend is interchangeable with the fixed one for the probe
// path: startBuiltinBackend works with a seed and without.
func TestCoverBackendBothModes(t *testing.T) {
	for _, seed := range []string{"", "0011223344556677"} {
		addr, err := startBuiltinBackend(seed)
		if err != nil {
			t.Fatalf("seed %q: %v", seed, err)
		}
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + addr + "/")
		if err != nil {
			t.Fatalf("seed %q: %v", seed, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("seed %q: status %d", seed, resp.StatusCode)
		}
	}
}
