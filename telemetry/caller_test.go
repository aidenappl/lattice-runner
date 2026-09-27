package telemetry

import (
	"context"
	"log/slog"
	"runtime"
	"testing"
)

// logOnBehalf stands in for a helper like wsSend: it logs for its caller.
func logOnBehalf(ctx context.Context, msg string) {
	LogAt(ctx, 1, slog.LevelWarn, msg, "component", "runner", "container", "web-1")
}

func TestLogAtIsAttributedToTheHelpersCaller(t *testing.T) {
	rec, _ := bridge(t)

	_, _, line, _ := runtime.Caller(0)
	logOnBehalf(context.Background(), "ws send failed")

	e := only(t, rec)
	d := e.Data.(map[string]any)
	if d["source_file"] != "caller_test.go" || d["source_func"] != "TestLogAtIsAttributedToTheHelpersCaller" || d["source_line"] != line+1 {
		t.Errorf("source = %v:%v %v, want caller_test.go:%d TestLogAtIsAttributedToTheHelpersCaller", d["source_file"], d["source_line"], d["source_func"], line+1)
	}
	if e.Name != "runner.log.warn" || e.Level != "warn" {
		t.Errorf("event = %s level=%s, want runner.log.warn at warn", e.Name, e.Level)
	}
	if d["message"] != "ws send failed" || d["container"] != "web-1" {
		t.Errorf("data = %v, want the message and attrs unchanged", d)
	}
}

func TestLogAtSkipZeroIsTheDirectCaller(t *testing.T) {
	rec, _ := bridge(t)

	_, _, line, _ := runtime.Caller(0)
	LogAt(context.Background(), 0, slog.LevelInfo, "direct", "component", "lifecycle")

	e := only(t, rec)
	d := e.Data.(map[string]any)
	if d["source_func"] != "TestLogAtSkipZeroIsTheDirectCaller" || d["source_line"] != line+1 {
		t.Errorf("source = %v:%v %v, want TestLogAtSkipZeroIsTheDirectCaller:%d", d["source_file"], d["source_line"], d["source_func"], line+1)
	}
	if e.Name != "lifecycle.log.info" {
		t.Errorf("event = %s, want lifecycle.log.info", e.Name)
	}
}

func TestLogAtBelowLevelIsDropped(t *testing.T) {
	rec, _ := bridge(t)

	LogAt(context.Background(), 0, slog.LevelDebug, "noise", "component", "runner")

	if n := len(rec.Events()); n != 0 {
		t.Errorf("recorded %d events at debug with level info, want 0", n)
	}
}
