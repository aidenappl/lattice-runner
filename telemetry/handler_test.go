package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"log/slog"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	monitor "github.com/aidenappl/go-monitor"
)

// lockedBuffer is stderr for tests that log from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// bridge installs the production handler chain the way installLogBridge does,
// with stderr captured, and restores the standard logger afterwards.
func bridge(t *testing.T) (*monitor.Recorder, *lockedBuffer) {
	t.Helper()
	rec, out, _ := bridgeWith(t, slog.LevelInfo)
	return rec, out
}

func bridgeWith(t *testing.T, level slog.Level) (*monitor.Recorder, *lockedBuffer, *runnerHandler) {
	t.Helper()
	rec := record(t)
	prevOut, prevFlags, prevDefault := log.Writer(), log.Flags(), slog.Default()
	out := &lockedBuffer{}
	h := newRunnerHandler(newLineHandler(out, level), monitor.NewSlogHandler(nil, &monitor.SlogOptions{Level: level}))
	log.SetFlags(log.Lshortfile)
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() {
		slog.SetDefault(prevDefault)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return rec, out, h
}

func only(t *testing.T, rec *monitor.Recorder) monitor.Event {
	t.Helper()
	evs := rec.Events()
	if len(evs) != 1 {
		t.Fatalf("recorded %d events, want 1: %v", len(evs), evs)
	}
	return evs[0]
}

func TestLogPrintfIsAttributedToItsCaller(t *testing.T) {
	rec, _ := bridge(t)

	_, _, line, _ := runtime.Caller(0)
	log.Printf("deploy: pulling image %s", "registry.appleby.cloud/web:latest")

	e := only(t, rec)
	d := e.Data.(map[string]any)
	t.Logf("event %s level=%s source=%v:%v %v", e.Name, e.Level, d["source_file"], d["source_line"], d["source_func"])
	if d["source_file"] != "handler_test.go" || d["source_func"] != "TestLogPrintfIsAttributedToItsCaller" || d["source_line"] != line+1 {
		t.Errorf("source = %v:%v %v, want handler_test.go:%d TestLogPrintfIsAttributedToItsCaller", d["source_file"], d["source_line"], d["source_func"], line+1)
	}
	if e.Name != "deploy.log.info" || e.Level != monitor.LevelInfo {
		t.Errorf("name=%q level=%q", e.Name, e.Level)
	}
}

var helperLine int

func logFromHelper(msg string) {
	_, _, helperLine, _ = runtime.Caller(0)
	log.Printf("%s", msg)
}

// A log.Printf inside a helper is attributed to the helper, not to whoever
// called it: the bridge records the frame that called log.Printf. Helpers that
// log on behalf of a caller need their own slog.Record with the caller's PC.
func TestLogPrintfInAHelperIsAttributedToTheHelper(t *testing.T) {
	rec, _ := bridge(t)

	logFromHelper("ws: send queue full")

	d := only(t, rec).Data.(map[string]any)
	t.Logf("source=%v:%v %v", d["source_file"], d["source_line"], d["source_func"])
	if d["source_file"] != "handler_test.go" || d["source_func"] != "logFromHelper" || d["source_line"] != helperLine+1 {
		t.Errorf("source = %v:%v %v, want handler_test.go:%d logFromHelper", d["source_file"], d["source_line"], d["source_func"], helperLine+1)
	}
}

func TestLogLinesKeepTheirEventNamesAndLevels(t *testing.T) {
	tests := []struct {
		name, line                 string
		event, level, message, cmp string
	}{
		{"colon component error", "db_restore: restore failed for pg: exit 1", "db_restore.log.error", monitor.LevelError, "db_restore: restore failed for pg: exit 1", "db_restore"},
		{"bracket component stripped", "[lifecycle] web: restarted — ok", "lifecycle.log.info", monitor.LevelInfo, "web: restarted — ok", "lifecycle"},
		{"no component", "failed to stop web: timeout", "runner.log.error", monitor.LevelError, "failed to stop web: timeout", ""},
		{"warn", "ws: send queue full (256/256), dropping container_logs message", "ws.log.warn", monitor.LevelWarn, "ws: send queue full (256/256), dropping container_logs message", "ws"},
		{"soft failure", "deploy: stop failed for web: timeout, trying kill", "deploy.log.warn", monitor.LevelWarn, "deploy: stop failed for web: timeout, trying kill", "deploy"},
		{"info", "db_snapshot: snapshot completed for pg (size=1024 bytes)", "db_snapshot.log.info", monitor.LevelInfo, "db_snapshot: snapshot completed for pg (size=1024 bytes)", "db_snapshot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, _ := bridge(t)
			log.Printf("%s", tt.line)
			e := only(t, rec)
			d := e.Data.(map[string]any)
			if e.Name != tt.event || e.Level != tt.level {
				t.Errorf("event %q at %q, want %q at %q", e.Name, e.Level, tt.event, tt.level)
			}
			if d["message"] != tt.message || d["component"] != tt.cmp || d["worker"] != "worker-6" {
				t.Errorf("data = %v", d)
			}
		})
	}
}

func TestSlogRecordsKeepTheirOwnLevel(t *testing.T) {
	tests := []struct {
		name         string
		log          func()
		event, level string
		check        func(t *testing.T, d map[string]any)
	}{
		{
			name:  "info with failure words is not reclassified",
			log:   func() { slog.Info("deploy: stop failed for web", "container", "web") },
			event: "deploy.log.info", level: monitor.LevelInfo,
			check: func(t *testing.T, d map[string]any) {
				if d["container"] != "web" {
					t.Errorf("data = %v", d)
				}
			},
		},
		{
			name:  "warn with component attr",
			log:   func() { slog.Warn("disk low", "component", "metrics") },
			event: "metrics.log.warn", level: monitor.LevelWarn,
		},
		{
			name:  "component from With",
			log:   func() { slog.Default().With("component", "scheduler").Error("tick missed") },
			event: "scheduler.log.error", level: monitor.LevelError,
		},
		{
			name: "explicit event name is kept",
			log: func() {
				slog.ErrorContext(context.Background(), "deploy failed", "event", "deployment.failed", "error", "boom")
			},
			event: "deployment.failed", level: monitor.LevelError,
			check: func(t *testing.T, d map[string]any) {
				if d["worker"] != "worker-6" || d["error"] != "boom" {
					t.Errorf("data = %v", d)
				}
			},
		},
		{
			name:  "fatal",
			log:   func() { slog.Log(context.Background(), slog.LevelError+4, "unrecoverable", "component", "boot") },
			event: "boot.log.fatal", level: monitor.LevelFatal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, _ := bridge(t)
			tt.log()
			e := only(t, rec)
			if e.Name != tt.event || e.Level != tt.level {
				t.Errorf("event %q at %q, want %q at %q", e.Name, e.Level, tt.event, tt.level)
			}
			d := e.Data.(map[string]any)
			if d["worker"] != "worker-6" || d["source_file"] != "handler_test.go" {
				t.Errorf("data = %v", d)
			}
			if tt.check != nil {
				tt.check(t, d)
			}
		})
	}
}

func TestDebugIsNotShippedAtInfo(t *testing.T) {
	rec, out := bridge(t)
	slog.Debug("noisy", "component", "metrics")
	if n := len(rec.Events()); n != 0 {
		t.Errorf("debug shipped %d events at level info", n)
	}
	if out.String() != "" {
		t.Errorf("debug reached stderr at level info: %q", out.String())
	}

	rec, _, _ = bridgeWith(t, slog.LevelDebug)
	slog.Debug("noisy", "component", "metrics")
	if evs := rec.Named("metrics.log.debug"); len(evs) != 1 {
		t.Errorf("with MONITOR_DEBUG, recorded %d metrics.log.debug events, want 1", len(evs))
	}
}

func TestStderrKeepsTheStandardLoggerFormat(t *testing.T) {
	_, out := bridge(t)

	_, _, line, _ := runtime.Caller(0)
	log.Printf("[lifecycle] web: restarted — ok")
	slog.Warn("disk low", "component", "metrics", "free", "2 GB")

	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	t.Logf("stderr:\n%s", out.String())
	if len(got) != 2 {
		t.Fatalf("stderr has %d lines, want 2 (no duplicates): %q", len(got), out.String())
	}
	want := []*regexp.Regexp{
		regexp.MustCompile(fmt.Sprintf(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} handler_test\.go:%d: \[lifecycle\] web: restarted — ok$`, line+1)),
		regexp.MustCompile(fmt.Sprintf(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} handler_test\.go:%d: WARN disk low component=metrics free="2 GB"$`, line+2)),
	}
	for i, re := range want {
		if !re.MatchString(got[i]) {
			t.Errorf("line %d = %q, want %s", i, got[i], re)
		}
	}
}

func TestWarnLimiter(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	l := newWarnLimiter(3, time.Minute, func() time.Time { return now })

	steps := []struct {
		advance        time.Duration
		key            string
		ok             bool
		suppressedWant int
	}{
		{0, "a", true, 0},
		{0, "a", true, 0},
		{0, "a", true, 0},
		{0, "a", false, 0},
		{0, "b", true, 0}, // keys are independent
		{10 * time.Second, "a", false, 0},
		{50 * time.Second, "a", true, 2}, // new window: carries the count
		{0, "a", true, 0},
	}
	for i, s := range steps {
		now = now.Add(s.advance)
		ok, n := l.allow(s.key)
		if ok != s.ok || n != s.suppressedWant {
			t.Errorf("step %d (%s): allow = (%v, %d), want (%v, %d)", i, s.key, ok, n, s.ok, s.suppressedWant)
		}
	}
}

func TestWarnLimiterBoundsItsKeys(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	l := newWarnLimiter(1, time.Minute, func() time.Time { return now })
	for i := 0; i < warnLimiterSweepAt*2; i++ {
		l.allow(fmt.Sprintf("k%d", i))
	}
	if n := len(l.keys); n > warnLimiterSweepAt {
		t.Errorf("limiter holds %d keys, want at most %d", n, warnLimiterSweepAt)
	}
}

func TestRepeatedWarningsAreRateLimitedAndErrorsAreNot(t *testing.T) {
	rec, out, h := bridgeWith(t, slog.LevelInfo)
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	h.lim.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("ws: send queue full (256/256), dropping container_logs message")
			log.Printf("docker: connect failed: dial unix: no such file")
		}()
	}
	wg.Wait()

	if n := len(rec.Named("ws.log.warn")); n != WARN_LIMIT {
		t.Errorf("recorded %d ws.log.warn events, want %d", n, WARN_LIMIT)
	}
	if n := len(rec.Named("docker.log.error")); n != 25 {
		t.Errorf("recorded %d docker.log.error events, want all 25: errors are never limited", n)
	}
	if n := strings.Count(out.String(), "send queue full"); n != 25 {
		t.Errorf("stderr has %d queue-full lines, want all 25: the limit is for Monitor only", n)
	}

	mu.Lock()
	now = now.Add(WARN_WINDOW)
	mu.Unlock()
	rec.Reset()
	log.Printf("ws: send queue full (256/256), dropping container_logs message")
	evs := rec.Named("ws.log.warn")
	if len(evs) != 1 {
		t.Fatalf("recorded %d ws.log.warn events after the window, want 1", len(evs))
	}
	if d := evs[0].Data.(map[string]any); d["suppressed"] != int64(5) {
		t.Errorf("suppressed = %#v, want 5 (data %v)", d["suppressed"], d)
	}
}
