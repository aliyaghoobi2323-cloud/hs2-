package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// b271 Q6: the first retireSumLines events of a minute are logged as
// before; the rest fold into one summary at the minute's end with counts
// and the latest pool state; a new minute starts verbatim again.
func TestRetireSummary(t *testing.T) {
	var lines []string
	b := newRetireSum(func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	var pending func()
	var after time.Duration
	b.afterFunc = func(d time.Duration, f func()) { after, pending = d, f }
	now := time.Now()
	b.note(now, nil, nil, 1, 1, 0) // nothing: no line, no event
	b.note(now, []string{"1"}, []string{"2", "3"}, 10, 8, 2)
	b.note(now.Add(time.Second), nil, []string{"4"}, 10, 7, 3)
	b.note(now.Add(2*time.Second), []string{"5"}, nil, 10, 8, 2)
	if len(lines) != 4 || !strings.Contains(lines[0], "link(s) 1 back in service (10 up: 8 serving, 2 retiring)") ||
		!strings.Contains(lines[1], "link(s) 2,3 retiring") {
		t.Fatalf("verbatim lines: %q", lines)
	}
	// folded
	b.note(now.Add(3*time.Second), nil, []string{"6", "7"}, 11, 6, 5)
	b.note(now.Add(4*time.Second), []string{"8"}, []string{"9"}, 12, 7, 5)
	b.note(now.Add(5*time.Second), []string{"10"}, nil, 12, 8, 4)
	if len(lines) != 4 || pending == nil || after != retireSumWin-3*time.Second {
		t.Fatalf("not folded: %d lines, flush in %v", len(lines), after)
	}
	pending()
	if len(lines) != 5 {
		t.Fatalf("no summary: %q", lines)
	}
	want := "2 link(s) back in service in 2 event(s), 3 link(s) retiring in 2 event(s) — now 12 up: 8 serving, 4 retiring"
	if !strings.Contains(lines[4], want) {
		t.Fatalf("summary %q, want %q", lines[4], want)
	}
	pending()
	if len(lines) != 5 {
		t.Fatal("an empty flush logged")
	}
	// next minute: verbatim again
	b.note(now.Add(retireSumWin+time.Second), []string{"11"}, nil, 12, 9, 3)
	if len(lines) != 6 || !strings.Contains(lines[5], "link(s) 11 back in service") {
		t.Fatalf("new minute not verbatim: %q", lines)
	}
	var nilSum *retireSum
	nilSum.note(now, []string{"1"}, nil, 1, 1, 0) // no panic
}
