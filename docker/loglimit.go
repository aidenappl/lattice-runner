package docker

import (
	"fmt"
	"time"
)

// DEFAULT_LOG_LINES_PER_SECOND is the per-container cap on log lines streamed
// to lattice-api (LOG_LINES_PER_SECOND; 0 disables the cap).
//
// One container used to be able to saturate the worker socket: on 2026-11-08
// both ClickHouse servers on worker-6 lost write access to their log file and
// printed every log message to stderr as an 11-line stack trace, 200–400 lines/s
// each. The runner's send queue overflowed (31k "ws send queue full"), dropping
// status and health messages along with the logs, and lattice-api's log queue
// backed up behind them.
const DEFAULT_LOG_LINES_PER_SECOND = 200

// lineLimiter caps one container's lines per one-second window. Lines over the
// cap are counted, not sent; the next line allowed after the window rolls over
// is preceded by one marker line saying how many were suppressed, and so is the
// end of the stream (flush), so a burst never vanishes silently.
type lineLimiter struct {
	perSecond   int
	windowStart time.Time
	inWindow    int
	suppressed  int
}

func newLineLimiter(perSecond int) *lineLimiter {
	return &lineLimiter{perSecond: perSecond}
}

// allow reports whether a line read at now may be sent. When it may, and lines
// were suppressed before it, marker is the summary to send first.
func (l *lineLimiter) allow(now time.Time) (ok bool, marker string) {
	if l == nil || l.perSecond <= 0 {
		return true, ""
	}
	if now.Sub(l.windowStart) >= time.Second || now.Before(l.windowStart) {
		l.windowStart = now
		l.inWindow = 0
	}
	if l.inWindow >= l.perSecond {
		l.suppressed++
		return false, ""
	}
	l.inWindow++
	return true, l.flush()
}

// flush returns the pending marker, if any, and clears the count.
func (l *lineLimiter) flush() string {
	if l == nil || l.suppressed == 0 {
		return ""
	}
	n := l.suppressed
	l.suppressed = 0
	return fmt.Sprintf("[lattice] %d log lines suppressed (rate limit %d lines/s per container)", n, l.perSecond)
}
