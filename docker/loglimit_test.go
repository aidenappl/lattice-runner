package docker

import (
	"strings"
	"testing"
	"time"
)

func TestLineLimiter(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		perSecond  int
		lines      []time.Duration // offsets from t0
		wantSent   int
		wantMarker string // substring of the one marker expected, "" for none
	}{
		{"under the cap", 3, []time.Duration{0, 10, 20}, 3, ""},
		{"disabled", 0, []time.Duration{0, 0, 0, 0, 0}, 5, ""},
		{"over the cap, marker on next window", 2,
			[]time.Duration{0, 1, 2, 3, 4, time.Second + 5}, 3, "3 log lines suppressed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newLineLimiter(tt.perSecond)
			sent, markers := 0, []string{}
			for _, off := range tt.lines {
				ok, m := l.allow(t0.Add(off * time.Millisecond / time.Millisecond))
				if ok {
					sent++
				}
				if m != "" {
					markers = append(markers, m)
				}
			}
			if sent != tt.wantSent {
				t.Fatalf("sent %d, want %d", sent, tt.wantSent)
			}
			if tt.wantMarker == "" && len(markers) > 0 {
				t.Fatalf("unexpected markers %v", markers)
			}
			if tt.wantMarker != "" && (len(markers) != 1 || !strings.Contains(markers[0], tt.wantMarker)) {
				t.Fatalf("markers = %v, want one containing %q", markers, tt.wantMarker)
			}
		})
	}
}

func TestLineLimiterFlushReportsTrailingSuppression(t *testing.T) {
	l := newLineLimiter(1)
	now := time.Now()
	l.allow(now)
	l.allow(now)
	l.allow(now)
	if m := l.flush(); !strings.Contains(m, "2 log lines suppressed") {
		t.Fatalf("flush = %q", m)
	}
	if m := l.flush(); m != "" {
		t.Fatalf("second flush = %q, want empty", m)
	}
	var nilLimiter *lineLimiter
	if ok, _ := nilLimiter.allow(now); !ok || nilLimiter.flush() != "" {
		t.Fatal("a nil limiter must allow everything")
	}
}
