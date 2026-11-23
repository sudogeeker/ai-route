package httpx

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"", 0}, {"0", 0}, {"-1", 0}, {"37", 37 * time.Second},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"Sat, 24 Oct 2026 11:00:00 GMT", 0},
	} {
		if got := RetryAfter(test.value, now); got != test.want {
			t.Errorf("RetryAfter(%q)=%s, want %s", test.value, got, test.want)
		}
	}
}
