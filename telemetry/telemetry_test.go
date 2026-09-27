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
		{"ws: read error, will reconnect: websocket: close 1006 (abnormal closure): unexpected EOF", monitor.LevelWarn},
		{"ws: connection failed, will reconnect: dial: read tcp 10.0.0.1:1->10.0.0.2:443: i/o timeout", monitor.LevelWarn},
		{"deploy: pulling image registry.appleby.cloud/web:latest", monitor.LevelInfo},
		{"db_snapshot: snapshot completed for pg (size=1024 bytes)", monitor.LevelInfo},

		// Upgrade: fixed text, output length only.
		{"upgrade completed; runner will restart via systemd (output 48213 bytes)", monitor.LevelInfo},
		{"upgrade failed: exit status 1 (output 5120 bytes)", monitor.LevelError},
		{"upgrade download failed: exit status 22 (output 57 bytes)", monitor.LevelError},
		{"upgrade ABORTED: hash mismatch — expected 9f86d081884c7d65, got 60303ae22b998861", monitor.LevelError},
		{"upgrade ABORTED: no expected_hash provided by orchestrator — refusing to run unverified script", monitor.LevelError},

		// stop_all / start_all summaries.
		{"stop_all complete: 3 stopped, all succeeded", monitor.LevelInfo},
		{"stop_all complete: 0 stopped, all succeeded", monitor.LevelInfo},
		{"stop_all complete: 2 stopped, 1 could not be stopped", monitor.LevelWarn},
		{"start_all complete: 5 started, all succeeded", monitor.LevelInfo},
		{"start_all complete: 4 started, 2 could not be started", monitor.LevelWarn},
		// The old form "stop_all complete: stopped=3 failed=0" classified as error
		// because "failed=0" matches \bfailed\b; that is why the summary changed.

		// Queue drops: logged once, by the client, with the type.
		{"ws: send queue full (256/256), dropping container_logs message", monitor.LevelWarn},
		{"ws: send queue full (256/256), dropping untyped message", monitor.LevelWarn},
		{"ws: reliable send timed out after 10s (queue 256/256), dropping exec_output message", monitor.LevelWarn},

		// Write-pump failures reconnect.
		{"ws: write error, will reconnect: write tcp 10.0.0.1:1->10.0.0.2:443: write: broken pipe", monitor.LevelWarn},
		{"ws: ping write error, will reconnect: websocket: close sent", monitor.LevelWarn},

		{"scheduled snapshot for instance 12 failed preflight: no backup destination configured", monitor.LevelError},
		{"scheduled snapshot for instance 12 failed preflight: backup destination has no type/config", monitor.LevelError},
		{"deploy: stack \"web\" already has in-flight deployment 41, rejecting deployment 42", monitor.LevelWarn},
		{"pull failed for registry.appleby.cloud/web:latest: 502 Bad Gateway — proceeding with recreate anyway", monitor.LevelWarn},
		{"web: remove — stop returned: context deadline exceeded (proceeding with force remove)", monitor.LevelWarn},

		// Negative cases: nearby words that must not change level.
		{"canary check aborting soon", monitor.LevelInfo},
		{"could not be reached", monitor.LevelInfo},
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

func TestNonRunnerPanicLinesAreEmitted(t *testing.T) {
	tests := []struct{ name, line string }{
		{"postgres stderr", "2026/09/12 16:55:07 main.go:2400: db_logs: pg: PANIC:  could not write to file \"pg_wal/xlogtemp.31\": No space left on device\n"},
		{"bracketed without stack", "2026/09/12 16:55:07 main.go:2400: [db_logs] PANIC: could not locate a valid checkpoint record\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := record(t)
			_, _ = logTee{}.Write([]byte(tt.line))
			if n := len(rec.Events()); n != 1 {
				t.Errorf("recorded %d events for %q, want 1", n, tt.line)
			}
		})
	}
}

func TestRunnerPanicLinesAreNotDuplicated(t *testing.T) {
	tests := []struct{ name, line string }{
		{"Recover", "2026/09/12 16:55:07 telemetry.go:187: [handler:deploy] PANIC (recovered): boom\ngoroutine 7 [running]:\n"},
		{"safeGo", "2026/09/12 16:55:07 main.go:3284: [ws-connect] PANIC: boom\ngoroutine 9 [running]:\n"},
		{"message handler", "2026/09/12 16:55:07 main.go:229: [message-handler] PANIC for event \"x\": boom\ngoroutine 5 [running]:\n"},
		{"multi-line panic value", "2026/09/12 16:55:07 telemetry.go:187: [handler:deploy] PANIC (recovered): first line\nsecond line\ngoroutine 7 [running]:\n"},
		{"safeGoResilient", "2026/09/12 16:55:07 main.go:3321: [heartbeat] PANIC (recovered, restarting loop after backoff): boom\ngoroutine 3 [running]:\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := record(t)
			_, _ = logTee{}.Write([]byte(tt.line))
			if n := len(rec.Events()); n != 0 {
				t.Errorf("a runner PANIC line produced %d events; ReportPanic already reports it", n)
			}
		})
	}
}

func TestEventIsStampedWithTheWorker(t *testing.T) {
	rec := record(t)

	Event(monitor.LevelError, "runner.upgrade.failed", map[string]any{"error": "exit status 1", "output": "tail"})

	evs := rec.Named("runner.upgrade.failed")
	if len(evs) != 1 {
		t.Fatalf("recorded %d runner.upgrade.failed events, want 1", len(evs))
	}
	d := evs[0].Data.(map[string]any)
	if evs[0].Level != monitor.LevelError || d["worker"] != "worker-6" || d["output"] != "tail" {
		t.Errorf("level=%q data=%v", evs[0].Level, d)
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
