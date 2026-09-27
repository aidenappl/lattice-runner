package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	monitor "github.com/aidenappl/go-monitor"
	"github.com/aidenappl/lattice-runner/client"
	"github.com/gorilla/websocket"
)

const (
	testCommandID = "0123456789abcdef"
	testRequestID = "fedcba9876543210"
	testTraceID   = "4bf92f35-77b3-4da6-a3ce-929d0e0e4736"
)

// wsHarness connects a real WSClient to a local server and returns every
// message the runner sends, decoded, in order.
func wsHarness(t *testing.T) (*client.WSClient, func() (client.OutgoingMessage, []byte)) {
	t.Helper()
	msgs := make(chan []byte, 64)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, b, err := conn.ReadMessage()
			if err != nil {
				return
			}
			msgs <- b
		}
	}))
	ctx, cancel := context.WithCancel(context.Background())
	ws := client.NewWSClient("ws"+strings.TrimPrefix(srv.URL, "http"), "token", 50*time.Millisecond)
	go ws.Connect(ctx)
	t.Cleanup(func() {
		cancel()
		ws.Close()
		srv.Close()
	})
	next := func() (client.OutgoingMessage, []byte) {
		t.Helper()
		select {
		case b := <-msgs:
			var m client.OutgoingMessage
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("decode %s: %v", b, err)
			}
			return m, b
		case <-time.After(5 * time.Second):
			t.Fatal("no message received")
		}
		return client.OutgoingMessage{}, nil
	}
	return ws, next
}

// eventsWithMessage filters recorded events to one log message, ignoring the
// websocket client's own connection logs.
func eventsWithMessage(rec *monitor.Recorder, msg string) []monitor.Event {
	var out []monitor.Event
	for _, ev := range rec.Events() {
		if d, ok := ev.Data.(map[string]any); ok && d["message"] == msg {
			out = append(out, ev)
		}
	}
	return out
}

// recordEvents routes slog through Monitor's handler into a recorder.
func recordEvents(t *testing.T) *monitor.Recorder {
	t.Helper()
	rec := monitor.StartRecording()
	prev := slog.Default()
	slog.SetDefault(slog.New(monitor.NewSlogHandler(nil, &monitor.SlogOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		rec.Stop()
	})
	return rec
}

func deployEnvelope() client.Envelope {
	return client.Envelope{
		Type:      "deploy",
		CommandID: testCommandID,
		RequestID: testRequestID,
		TraceID:   testTraceID,
		Payload:   map[string]any{"deployment_id": float64(42)},
	}
}

func TestCommandContextCarriesIDsIntoHandlerEvents(t *testing.T) {
	rec := recordEvents(t)
	ctx := commandContext(context.Background(), deployEnvelope())

	slog.InfoContext(ctx, "deploy starting deployment", "component", "deploy")

	evs := eventsWithMessage(rec, "deploy starting deployment")
	if len(evs) != 1 {
		t.Fatalf("recorded %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.RequestID != testRequestID {
		t.Errorf("request_id = %q, want %q", ev.RequestID, testRequestID)
	}
	if ev.TraceID != testTraceID {
		t.Errorf("trace_id = %q, want %q", ev.TraceID, testTraceID)
	}
	if ev.JobID != "de00000000000042" {
		t.Errorf("job_id = %q, want the deployment's job id de00000000000042", ev.JobID)
	}
	if !monitor.ValidCorrelationID(ev.JobID) {
		t.Errorf("job_id %q would be rejected by monitor-core", ev.JobID)
	}
}

func TestCommandContextRequestIDFallback(t *testing.T) {
	tests := []struct {
		name string
		env  client.Envelope
		want string
	}{
		{"request id wins", client.Envelope{CommandID: testCommandID, RequestID: testRequestID}, testRequestID},
		{"command id when no request id", client.Envelope{CommandID: testCommandID}, testCommandID},
		{"command id when request id invalid", client.Envelope{CommandID: testCommandID, RequestID: "not an id!"}, testCommandID},
		{"nothing valid", client.Envelope{CommandID: "exec-session"}, ""},
		{"legacy envelope", client.Envelope{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := commandContext(context.Background(), tt.env)
			if got := monitor.RequestID(ctx); got != tt.want {
				t.Errorf("request_id = %q, want %q", got, tt.want)
			}
		})
	}
	if got := monitor.TraceID(commandContext(context.Background(), client.Envelope{TraceID: "bad trace"})); got != "" {
		t.Errorf("invalid trace_id kept as %q", got)
	}
}

func TestCommandJobID(t *testing.T) {
	tests := []struct {
		name string
		env  client.Envelope
		want string
	}{
		{"deploy", client.Envelope{Type: "deploy", CommandID: testCommandID, Payload: map[string]any{"deployment_id": float64(42)}}, "de00000000000042"},
		{"deployment ping", client.Envelope{Type: "deployment_ping", Payload: map[string]any{"deployment_id": float64(7)}}, "de00000000000007"},
		{"snapshot", client.Envelope{Type: "db_snapshot", Payload: map[string]any{"snapshot_id": float64(9), "database_instance_id": float64(3)}}, "ba00000000000009"},
		{"snapshot string id", client.Envelope{Type: "db_mirror_snapshot", Payload: map[string]any{"snapshot_id": "11"}}, "ba00000000000011"},
		{"restore", client.Envelope{Type: "db_restore", Payload: map[string]any{"restore_id": float64(5), "snapshot_id": float64(9)}}, "bd00000000000005"},
		{"restore via snapshot id", client.Envelope{Type: "db_restore", Payload: map[string]any{"snapshot_id": float64(9)}}, "bd00000000000009"},
		{"database instance", client.Envelope{Type: "db_start", Payload: map[string]any{"database_instance_id": float64(3)}}, "db00000000000003"},
		{"schedule instance", client.Envelope{Type: "db_update_schedule", Payload: map[string]any{"instance_id": float64(4)}}, "db00000000000004"},
		{"command id", client.Envelope{Type: "stop", CommandID: testCommandID}, testCommandID},
		{"zero deployment falls back", client.Envelope{Type: "deploy", CommandID: testCommandID, Payload: map[string]any{"deployment_id": float64(0)}}, testCommandID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commandJobID(tt.env); got != tt.want {
				t.Errorf("job_id = %q, want %q", got, tt.want)
			}
		})
	}

	// No unit and no usable command id: a fresh, valid id per command.
	a := commandJobID(client.Envelope{Type: "stop", CommandID: "exec-session"})
	b := commandJobID(client.Envelope{Type: "stop"})
	if a == "" || a == b || !monitor.ValidCorrelationID(a) || !monitor.ValidCorrelationID(b) {
		t.Errorf("minted job ids %q, %q: want distinct valid ids", a, b)
	}
}

func TestRepliesUnderACommandContextEchoItsIDs(t *testing.T) {
	ws, next := wsHarness(t)
	env := client.Envelope{
		Type:      "db_start",
		CommandID: testCommandID,
		RequestID: testRequestID,
		TraceID:   testTraceID,
		Payload:   map[string]any{"database_instance_id": float64(3), "request_id": "payload-level-id", "idempotency_key": "k1"},
	}
	ctx := commandContext(context.Background(), env)

	wsSend(ctx, ws, "container_status", client.OutgoingMessage{Type: "container_status", Status: "running"})
	wsSendReliable(ctx, ws, "worker_action_status", client.OutgoingMessage{Type: "worker_action_status"})
	sendDbReply(ctx, ws, env, "db_status", map[string]any{"status": "success"})
	sendLifecycleLog(ctx, ws, "db-1", "db_start", "started")

	for _, want := range []string{"container_status", "worker_action_status", "db_status", "lifecycle_log"} {
		m, raw := next()
		if m.Type != want {
			t.Fatalf("type = %q, want %q", m.Type, want)
		}
		if m.CommandID != testCommandID || m.RequestID != testRequestID || m.TraceID != testTraceID {
			t.Errorf("%s ids = (%q, %q, %q), want all three echoed: %s", m.Type, m.CommandID, m.RequestID, m.TraceID, raw)
		}
		if want == "db_status" {
			// The payload-level correlation echo is unchanged.
			if m.Payload["request_id"] != "payload-level-id" || m.Payload["idempotency_key"] != "k1" {
				t.Errorf("db reply payload lost its correlation echo: %v", m.Payload)
			}
		}
	}

	// A command_id set explicitly (exec session ids) is never overwritten.
	wsSend(ctx, ws, "exec_output", client.OutgoingMessage{Type: "exec_output", CommandID: "exec-session"})
	if m, _ := next(); m.CommandID != "exec-session" || m.RequestID != testRequestID {
		t.Errorf("explicit command id = %q (request_id %q), want exec-session kept and request_id stamped", m.CommandID, m.RequestID)
	}
}

func TestLegacyEnvelopeRepliesCarryNoNewIDs(t *testing.T) {
	rec := recordEvents(t)
	ws, next := wsHarness(t)
	env := client.Envelope{Type: "stop", Payload: map[string]any{"container_name": "web"}}
	ctx := commandContext(context.Background(), env)

	slog.InfoContext(ctx, "stopping container", "component", "runner")
	wsSend(ctx, ws, "container_status", client.OutgoingMessage{Type: "container_status", Status: "stopped"})

	m, raw := next()
	if m.CommandID != "" || m.RequestID != "" || m.TraceID != "" {
		t.Errorf("legacy reply stamped with ids: %s", raw)
	}
	for _, key := range []string{`"command_id"`, `"request_id"`, `"trace_id"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("legacy reply JSON contains %s: %s", key, raw)
		}
	}
	evs := eventsWithMessage(rec, "stopping container")
	if len(evs) != 1 {
		t.Fatalf("recorded %d events, want 1", len(evs))
	}
	if evs[0].RequestID != "" || evs[0].TraceID != "" {
		t.Errorf("legacy command event has request_id %q trace_id %q, want none", evs[0].RequestID, evs[0].TraceID)
	}
	if !monitor.ValidCorrelationID(evs[0].JobID) || evs[0].JobID == "" {
		t.Errorf("legacy command job_id = %q, want a valid minted id", evs[0].JobID)
	}
}

func TestUnsolicitedSendsCarryNoIDs(t *testing.T) {
	ws, next := wsHarness(t)
	tests := []struct {
		name string
		ctx  context.Context
	}{
		{"root context", context.Background()},
		{"hub cycle", cycleContext(context.Background())},
		{"connected event", dispatchContext(context.Background(), client.Envelope{Type: "connected", CommandID: testCommandID})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wsSend(tt.ctx, ws, "heartbeat", client.OutgoingMessage{Type: "heartbeat"})
			m, raw := next()
			if m.CommandID != "" || m.RequestID != "" || m.TraceID != "" {
				t.Errorf("unsolicited message stamped with ids: %s", raw)
			}
		})
	}
}

func TestCycleContextsHaveDistinctJobIDs(t *testing.T) {
	a := monitor.JobID(cycleContext(context.Background()))
	b := monitor.JobID(cycleContext(context.Background()))
	if a == "" || a == b || !monitor.ValidCorrelationID(a) {
		t.Errorf("cycle job ids %q, %q: want distinct valid ids", a, b)
	}
}

func TestStampPayload(t *testing.T) {
	ctx := commandContext(context.Background(), deployEnvelope())

	p := &client.OutgoingMessage{Type: "x"}
	stampPayload(ctx, p)
	if p.CommandID != testCommandID || p.RequestID != testRequestID || p.TraceID != testTraceID {
		t.Errorf("pointer message not stamped: %+v", p)
	}

	other := map[string]any{"type": "x"}
	if got, ok := stampPayload(ctx, other).(map[string]any); !ok || len(got) != 1 {
		t.Errorf("non-message payload changed: %v", got)
	}
	var nilMsg *client.OutgoingMessage
	if got := stampPayload(ctx, nilMsg); got != nilMsg {
		t.Errorf("nil message changed: %v", got)
	}
}
