package main

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/aidenappl/lattice-runner/client"
)

// captureHandler keeps every record it is given.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func capture(t *testing.T) *captureHandler {
	t.Helper()
	h := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

func onlyRecord(t *testing.T, h *captureHandler) slog.Record {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) != 1 {
		t.Fatalf("logged %d records, want 1", len(h.records))
	}
	return h.records[0]
}

func attrs(r slog.Record) map[string]string {
	m := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	return m
}

func frameOf(r slog.Record) runtime.Frame {
	f, _ := runtime.CallersFrames([]uintptr{r.PC}).Next()
	return f
}

// The helpers log on behalf of their callers, so the source Monitor shows must
// be the line that called the helper, not the helper itself.
func TestLoggingHelpersAreAttributedToTheirCaller(t *testing.T) {
	tests := []struct {
		name  string
		call  func(ws *client.WSClient) int
		level slog.Level
		msg   string
		want  map[string]string
	}{
		{
			name: "wsSend",
			call: func(ws *client.WSClient) int {
				_, _, line, _ := runtime.Caller(0)
				wsSend(context.Background(), ws, "bad", make(chan int)) // unmarshalable: SendJSON fails
				return line + 1
			},
			level: slog.LevelError,
			msg:   "ws send failed",
			want:  map[string]string{"component": "runner", "message_type": "bad"},
		},
		{
			name: "wsSendReliable",
			call: func(ws *client.WSClient) int {
				_, _, line, _ := runtime.Caller(0)
				wsSendReliable(context.Background(), ws, "bad", make(chan int))
				return line + 1
			},
			level: slog.LevelError,
			msg:   "ws reliable send failed",
			want:  map[string]string{"component": "runner", "message_type": "bad"},
		},
		{
			name: "sendLifecycleLog",
			call: func(ws *client.WSClient) int {
				_, _, line, _ := runtime.Caller(0)
				sendLifecycleLog(context.Background(), ws, "web-1", "stop", "looking up container…")
				return line + 1
			},
			level: slog.LevelInfo,
			msg:   "lifecycle event",
			want:  map[string]string{"component": "lifecycle", "container": "web-1", "action": "stop", "detail": "looking up container…"},
		},
		{
			name: "sendLifecycleWarn",
			call: func(ws *client.WSClient) int {
				_, _, line, _ := runtime.Caller(0)
				sendLifecycleWarn(context.Background(), ws, "web-1", "remove", "container not found")
				return line + 1
			},
			level: slog.LevelWarn,
			msg:   "lifecycle event",
			want:  map[string]string{"component": "lifecycle", "action": "remove", "detail": "container not found"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := capture(t)
			ws := client.NewWSClient("ws://127.0.0.1:1/ws", "token", time.Second)

			line := tt.call(ws)

			r := onlyRecord(t, h)
			if r.Level != tt.level || r.Message != tt.msg {
				t.Errorf("record = %s %q, want %s %q", r.Level, r.Message, tt.level, tt.msg)
			}
			f := frameOf(r)
			if f.Line != line {
				t.Errorf("source = %s:%d (%s), want line %d of the caller", f.File, f.Line, f.Function, line)
			}
			got := attrs(r)
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("attr %s = %q, want %q (all: %v)", k, got[k], v, got)
				}
			}
		})
	}
}

// lifecycleShim stands in for sendLifecycleLog/sendLifecycleWarn: it calls
// wsSendAt with the skip lifecycleLog uses. lifecycleLog's own payload always
// marshals, so a failing send is driven through the same skip here.
func lifecycleShim(ctx context.Context, ws *client.WSClient, payload any) {
	lifecycleSendShim(ctx, ws, payload)
}

func lifecycleSendShim(ctx context.Context, ws *client.WSClient, payload any) {
	wsSendAt(ctx, lifecycleCallerSkip, ws, "lifecycle_log", payload)
}

// A send that fails inside lifecycleLog must be attributed to the caller of
// sendLifecycleLog/sendLifecycleWarn, not to lifecycleLog.
func TestLifecycleSendFailureIsAttributedToTheOriginalCaller(t *testing.T) {
	h := capture(t)
	ws := client.NewWSClient("ws://127.0.0.1:1/ws", "token", time.Second)

	_, _, line, _ := runtime.Caller(0)
	lifecycleShim(context.Background(), ws, make(chan int)) // unmarshalable: SendJSON fails

	r := onlyRecord(t, h)
	if r.Level != slog.LevelError || r.Message != "ws send failed" {
		t.Errorf("record = %s %q, want ERROR %q", r.Level, r.Message, "ws send failed")
	}
	if f := frameOf(r); f.Line != line+1 {
		t.Errorf("source = %s:%d (%s), want line %d of the caller", f.File, f.Line, f.Function, line+1)
	}
	if got := attrs(r)["message_type"]; got != "lifecycle_log" {
		t.Errorf("message_type = %q, want lifecycle_log", got)
	}
}
