package main

import (
	"os"
	"path/filepath"
	"testing"
)

// rel53: the Go upgrade path (menu → upgrade binary) reported "upgraded" while a
// drop-in's ExecStart= kept the tunnel on another binary.
func TestExeIsBin(t *testing.T) {
	d := t.TempDir()
	bin := filepath.Join(d, "hs2")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(d, "hs2-link")
	if err := os.Symlink(bin, link); err != nil {
		t.Skip("no symlinks:", err)
	}
	cases := []struct {
		exe, bin string
		want     bool
	}{
		{"", bin, true},                        // no process: nothing to say
		{bin, bin, true},                       // the upgraded binary
		{bin + " (deleted)", bin, false},       // still the old inode: not restarted
		{bin, link, true},                      // $BIN is a symlink to it
		{"/usr/local/bin/hs2-r31", bin, false}, // a drop-in runs the old build
	}
	for _, c := range cases {
		if got := exeIsBin(c.exe, c.bin); got != c.want {
			t.Errorf("exeIsBin(%q, %q) = %v, want %v", c.exe, c.bin, got, c.want)
		}
	}
}
