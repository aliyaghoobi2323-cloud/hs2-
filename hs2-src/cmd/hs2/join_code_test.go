package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sec370 1c: the code is read from stdin, a file or a prompt; argv still
// works and warns; one line only is consumed from stdin.
func TestJoinCodeSource(t *testing.T) {
	const code = "hs2://v2.ABCdef_123"
	noPrompt := func() (string, error) { t.Fatal("prompted"); return "", nil }
	var warn bytes.Buffer

	// argv: works, warns
	got, err := joinCodeSource(code, "", strings.NewReader(""), false, noPrompt, &warn)
	if err != nil || got != code || !strings.Contains(warn.String(), "WARN") {
		t.Fatalf("argv: %q %v warn=%q", got, err, warn.String())
	}
	warn.Reset()

	// "-" = stdin; the rest of stdin is left for the wizard
	in := strings.NewReader("\n  '" + code + "'  \r\nnext answer\n")
	got, err = joinCodeSource("-", "", in, false, noPrompt, &warn)
	if err != nil || got != code || warn.Len() != 0 {
		t.Fatalf("stdin: %q %v warn=%q", got, err, warn.String())
	}
	if rest, _ := io.ReadAll(in); string(rest) != "next answer\n" {
		t.Fatalf("stdin over-read: rest %q", rest)
	}

	// no code, not a terminal: stdin
	got, err = joinCodeSource("", "", strings.NewReader("hs2 tunnel join '"+code+"'\n"), false, noPrompt, &warn)
	if err != nil || got != code {
		t.Fatalf("stdin (pasted whole command): %q %v", got, err)
	}

	// no code, a terminal: the prompt
	got, err = joinCodeSource("", "", strings.NewReader(""), true, func() (string, error) { return code, nil }, &warn)
	if err != nil || got != code {
		t.Fatalf("prompt: %q %v", got, err)
	}

	// --code-file
	f := filepath.Join(t.TempDir(), "code")
	os.WriteFile(f, []byte(code+"\n"), 0o600)
	got, err = joinCodeSource("", f, strings.NewReader(""), true, noPrompt, &warn)
	if err != nil || got != code || warn.Len() != 0 {
		t.Fatalf("file: %q %v", got, err)
	}
	got, err = joinCodeSource("", "-", strings.NewReader(code), false, noPrompt, &warn)
	if err != nil || got != code {
		t.Fatalf("file -: %q %v", got, err)
	}

	// errors: both, empty, missing file
	if _, err := joinCodeSource(code, f, strings.NewReader(""), false, noPrompt, &warn); err == nil {
		t.Fatal("argv + --code-file accepted")
	}
	if _, err := joinCodeSource("", "", strings.NewReader("\n\n"), false, noPrompt, &warn); err == nil {
		t.Fatal("empty stdin accepted")
	}
	if _, err := joinCodeSource("", filepath.Join(t.TempDir(), "nope"), strings.NewReader(""), false, noPrompt, &warn); err == nil {
		t.Fatal("missing file accepted")
	}
	// a huge line is cut, never read without bound
	big := strings.Repeat("A", joinCodeMax+100)
	got, _ = readCodeLine(strings.NewReader(big))
	if len(got) != joinCodeMax {
		t.Fatalf("cap: %d", len(got))
	}
}
