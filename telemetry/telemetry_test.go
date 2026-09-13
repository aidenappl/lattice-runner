package telemetry

import (
	"fmt"
	"strings"
	"testing"

	monitor "github.com/aidenappl/go-monitor"
)

func TestParseLine(t *testing.T) {
	tests := []struct {
		name, line             string
		caller, component, msg string
	}{
		{"colon component", "2026/09/12 16:55:07 main.go:1834: db_restore: restore failed for pg: exit 1\n", "main.go:1834", "db_restore", "db_restore: restore failed for pg: exit 1"},
		{"bracket component", "2026/09/12 16:55:07 main.go:40: [lifecycle] web: restarted — ok\n", "main.go:40", "lifecycle", "web: restarted — ok"},
		{"no component", "2026/09/12 16:55:07 main.go:900: failed to stop web: timeout\n", "main.go:900", "", "failed to stop web: timeout"},
		{"no flags", "ws: connection failed: dial tcp: refused\n", "", "ws", "ws: connection failed: dial tcp: refused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caller, component, msg := parseLine(tt.line)
			if caller != tt.caller || component != tt.component || msg != tt.msg {
				t.Errorf("parseLine = (%q, %q, %q), want (%q, %q, %q)", caller, component, msg, tt.caller, tt.component, tt.msg)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	tests := []struct{ msg, want string }{
		{"deployment failed: image not found", monitor.LevelError},
		{"db_snapshot: snapshot failed for pg: exit status 1", monitor.LevelError},
		{"docker connect attempt 3/30 failed: dial unix: no such file", monitor.LevelWarn},
		{"deploy: stop failed for web: timeout, trying kill", monitor.LevelWarn},
		{"deploy: network edge may already exist: Error response from daemon", monitor.LevelWarn},
		{"invalid deploy spec: missing stack", monitor.LevelWarn},
		{"ws: send queue full", monitor.LevelWarn},
		{"deploy: pulling image registry.appleby.cloud/web:latest", monitor.LevelInfo},
		{"db_snapshot: snapshot completed for pg (size=1024 bytes)", monitor.LevelInfo},
	}
	for _, tt := range tests {
		if got := classify(tt.msg); got != tt.want {
			t.Errorf("classify(%q) = %s, want %s", tt.msg, got, tt.want)
		}
	}
}

func record(t *testing.T) *monitor.Recorder {
	t.Helper()
	w := "worker-6"
	worker.Store(&w)
	rec := monitor.StartRecording()
	t.Cleanup(rec.Stop)
	return rec
}

func TestLogLinesBecomeEventsStampedWithTheWorker(t *testing.T) {
	rec := record(t)

	_, _ = logTee{}.Write([]byte("2026/09/12 16:55:07 main.go:1834: db_restore: restore failed for pg: exit 1\n"))

	evs := rec.Named("db_restore.log.error")
	if len(evs) != 1 {
		t.Fatalf("recorded %d db_restore.log.error events, want 1 (all: %d)", len(evs), len(rec.Events()))
	}
	d := evs[0].Data.(map[string]any)
	if evs[0].Level != monitor.LevelError || d["worker"] != "worker-6" || d["caller"] != "main.go:1834" {
		t.Errorf("level=%q data=%v", evs[0].Level, d)
	}
}

func TestPanicLogLinesAreNotDuplicated(t *testing.T) {
	rec := record(t)

	_, _ = logTee{}.Write([]byte("2026/09/12 16:55:07 main.go:221: [message-handler] PANIC for event \"deploy\": boom\ngoroutine 1 [running]:\n"))

	if n := len(rec.Events()); n != 0 {
		t.Errorf("a PANIC line produced %d events; ReportPanic already reports it with its stack", n)
	}
}

func TestRecoverReportsThePanicWithItsStackAndWorker(t *testing.T) {
	rec := record(t)

	func() {
		defer Recover("handler:deploy", map[string]any{"command_id": "c1"})
		var m map[string]int
		m["x"] = 1
	}()

	evs := rec.Named("panic.recovered")
	if len(evs) != 1 {
		t.Fatalf("recorded %d panic.recovered events, want 1", len(evs))
	}
	d := evs[0].Data.(map[string]any)
	if d["goroutine"] != "handler:deploy" || d["command_id"] != "c1" || d["worker"] != "worker-6" {
		t.Errorf("data = %v", d)
	}
	if !strings.Contains(fmt.Sprint(d["error"]), "nil map") || !strings.Contains(fmt.Sprint(d["stacktrace"]), "telemetry_test.go") {
		t.Errorf("error/stacktrace missing: %v", d)
	}
}
