package engine

import "testing"

func TestSmallFirstFromEnv(t *testing.T) {
	for v, want := range map[string]int{"": 0, "0": 0, "x": 0, "-3": 0, "1": smallFirstDefault, "1500": 1500, "999999": SmuxFrameSize} {
		if got := smallFirstFromEnv(v); got != want {
			t.Errorf("smallFirstFromEnv(%q) = %d, want %d", v, got, want)
		}
	}
	if c := newSmuxConfig(); c.SmallFirst != smuxSmallFirst {
		t.Fatalf("newSmuxConfig SmallFirst = %d, want %d", c.SmallFirst, smuxSmallFirst)
	}
}
