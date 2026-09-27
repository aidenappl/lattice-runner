package client

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
)

func TestMessageType(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"typed", `{"type":"heartbeat","payload":{"a":1}}`, "heartbeat"},
		{"empty type", `{"type":""}`, "untyped"},
		{"no type", `{"payload":{}}`, "untyped"},
		{"not an object", `[1,2,3]`, "untyped"},
		{"invalid json", `{`, "untyped"},
		{"non-string type", `{"type":42}`, "untyped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := messageType([]byte(tt.in)); got != tt.want {
				t.Errorf("messageType(%s) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// recordingHandler keeps every record it is handed.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func TestSendJSONQueueFull(t *testing.T) {
	rec := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })

	c := &WSClient{send: make(chan []byte, 1)}

	if err := c.SendJSON(OutgoingMessage{Type: "heartbeat"}); err != nil {
		t.Fatalf("first SendJSON: %v", err)
	}
	if len(rec.records) != 0 {
		t.Fatalf("a queued message logged %d records, want 0", len(rec.records))
	}

	err := c.SendJSON(OutgoingMessage{Type: "container_sync"})
	if !errors.Is(err, ErrSendQueueFull) {
		t.Fatalf("SendJSON on a full queue = %v, want ErrSendQueueFull", err)
	}
	if len(c.send) != 1 {
		t.Fatalf("queue length = %d, want 1 (the drop must not enqueue)", len(c.send))
	}

	if len(rec.records) != 1 {
		t.Fatalf("drop logged %d records, want exactly 1", len(rec.records))
	}
	r := rec.records[0]
	if r.Level != slog.LevelWarn {
		t.Errorf("level = %v, want WARN", r.Level)
	}
	if r.Message != "ws send queue full, dropping message" {
		t.Errorf("message = %q, want a fixed message", r.Message)
	}
	want := map[string]string{
		"component":    "ws",
		"message_type": "container_sync",
		"queue_len":    "1",
		"queue_cap":    "1",
	}
	got := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		got[a.Key] = a.Value.String()
		return true
	})
	for k, v := range want {
		if got[k] != v {
			t.Errorf("attr %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestSendJSONMarshalError(t *testing.T) {
	c := &WSClient{send: make(chan []byte, 1)}
	err := c.SendJSON(map[string]any{"bad": make(chan int)})
	if err == nil || errors.Is(err, ErrSendQueueFull) {
		t.Fatalf("SendJSON(unmarshalable) = %v, want a marshal error", err)
	}
	if len(c.send) != 0 {
		t.Fatalf("queue length = %d, want 0", len(c.send))
	}
}
