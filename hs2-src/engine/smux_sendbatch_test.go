package engine

import "testing"

func TestSendBatchFromEnv(t *testing.T) {
	for _, c := range []struct {
		v    string
		want int
	}{
		{"", 0}, {"0", 0}, {"-5", 0}, {"x", 0},
		{"1", sendBatchDefault},
		{"100", SmuxFrameSize + 8},
		{"65536", 65536},
		{" 65536 ", 65536},
		{"99999999", sendBatchMax},
	} {
		if got := sendBatchFromEnv(c.v); got != c.want {
			t.Errorf("sendBatchFromEnv(%q) = %d, want %d", c.v, got, c.want)
		}
	}
}
