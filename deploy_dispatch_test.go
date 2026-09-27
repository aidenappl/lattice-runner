package main

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"testing"

	"github.com/aidenappl/lattice-runner/client"
	"github.com/aidenappl/lattice-runner/deploy"
)

// problemRecords are the recorded records at warn or above: a deploy the
// runner fails must leave exactly one, its deployment.failed.
func problemRecords(h *captureHandler) []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.records {
		if r.Level >= slog.LevelWarn {
			out = append(out, r)
		}
	}
	return out
}

// onlyDeploymentFailed asserts that exactly one warn-or-above record was
// logged, an error-level deployment.failed, and returns its attrs.
func onlyDeploymentFailed(t *testing.T, h *captureHandler) (slog.Record, map[string]string) {
	t.Helper()
	recs := problemRecords(h)
	if len(recs) != 1 {
		for _, r := range recs {
			t.Logf("record: %s %q %v", r.Level, r.Message, attrs(r))
		}
		t.Fatalf("logged %d warn+ records, want exactly one deployment.failed", len(recs))
	}
	r := recs[0]
	a := attrs(r)
	if r.Level != slog.LevelError || a["event"] != deploy.EVENT_DEPLOYMENT_FAILED {
		t.Fatalf("record = %s event %q, want ERROR %s", r.Level, a["event"], deploy.EVENT_DEPLOYMENT_FAILED)
	}
	return r, a
}

func TestInvalidDeploySpecReportsOneDeploymentFailed(t *testing.T) {
	ws, next := wsHarness(t)
	h := capture(t)
	env := client.Envelope{
		Type:      "deploy",
		CommandID: testCommandID,
		Payload: map[string]any{
			"deployment_id": float64(42),
			"stack_name":    "web",
			"strategy":      "rolling",
			"containers":    "not a list",
		},
	}

	spec, ok := parseDeployCommand(dispatchContext(context.Background(), env), ws, env)
	if ok || spec != nil {
		t.Fatalf("parseDeployCommand = %v, %v; want a rejected spec", spec, ok)
	}

	r, a := onlyDeploymentFailed(t, h)
	want := map[string]string{
		"component":     "deploy",
		"deployment_id": "42",
		"stack":         "web",
		"strategy":      "rolling",
		"duration_ms":   "0",
		"failed_step":   deploy.STEP_PARSE,
	}
	for k, v := range want {
		if a[k] != v {
			t.Errorf("%s = %q, want %q", k, a[k], v)
		}
	}
	if a["error"] == "" {
		t.Error("deployment.failed has no error")
	}
	if _, ok := a["container_count"]; ok {
		t.Errorf("container_count = %q from a payload with no container list", a["container_count"])
	}
	if f := frameOf(r); !strings.HasSuffix(f.Function, ".parseDeployCommand") {
		t.Errorf("source = %s, want parseDeployCommand", f.Function)
	}

	m, raw := next()
	if m.Type != "deployment_progress" || m.Status != "failed" || m.CommandID != testCommandID {
		t.Errorf("reply = %s, want a failed deployment_progress for the command", raw)
	}
}

func TestDeployFieldsFromPayload(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    map[string]any
	}{
		{"empty", map[string]any{}, map[string]any{"component": "deploy"}},
		{
			"all present",
			map[string]any{"deployment_id": float64(7), "stack_name": "api", "strategy": "canary", "containers": []any{1, 2}},
			map[string]any{"component": "deploy", "deployment_id": 7, "stack": "api", "strategy": "canary", "container_count": 2},
		},
		{
			"wrong types dropped",
			map[string]any{"deployment_id": "seven", "stack_name": 3, "strategy": nil, "containers": "x"},
			map[string]any{"component": "deploy"},
		},
		{"fractional id dropped", map[string]any{"deployment_id": 1.5}, map[string]any{"component": "deploy"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := deployFieldsFromPayload(tt.payload)
			got := map[string]any{}
			for i := 0; i+1 < len(fields); i += 2 {
				got[fields[i].(string)] = fields[i+1]
			}
			if len(got) != len(tt.want) {
				t.Fatalf("fields = %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("%s = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestStackConflict(t *testing.T) {
	spec := deploy.DeploymentSpec{DeploymentID: 8, StackName: "web"}
	tests := []struct {
		name   string
		states map[int]*deploymentRunState
		spec   deploy.DeploymentSpec
		want   int
	}{
		{"none", map[int]*deploymentRunState{}, spec, 0},
		{"in flight on the stack", map[int]*deploymentRunState{7: {DeploymentID: 7, StackName: "web", InProgress: true}}, spec, 7},
		{"finished on the stack", map[int]*deploymentRunState{7: {DeploymentID: 7, StackName: "web"}}, spec, 0},
		{"in flight on another stack", map[int]*deploymentRunState{7: {DeploymentID: 7, StackName: "api", InProgress: true}}, spec, 0},
		{"itself", map[int]*deploymentRunState{8: {DeploymentID: 8, StackName: "web", InProgress: true}}, spec, 0},
		{"no stack name", map[int]*deploymentRunState{7: {DeploymentID: 7, InProgress: true}}, deploy.DeploymentSpec{DeploymentID: 8}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stackConflict(tt.states, tt.spec); got != tt.want {
				t.Errorf("stackConflict = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDeployStackConflictReportsOneDeploymentFailed(t *testing.T) {
	ws, next := wsHarness(t)
	h := capture(t)
	env := deployEnvelope()
	spec := deploy.DeploymentSpec{
		DeploymentID: 8,
		StackName:    "web",
		Strategy:     "canary",
		Containers:   []deploy.ContainerSpec{{Name: "a"}, {Name: "b"}},
	}
	states := map[int]*deploymentRunState{7: {DeploymentID: 7, StackName: "web", InProgress: true}}

	conflict := stackConflict(states, spec)
	_, _, line, _ := runtime.Caller(0)
	rejectDeployConflict(dispatchContext(context.Background(), env), ws, env, spec, conflict)

	r, a := onlyDeploymentFailed(t, h)
	want := map[string]string{
		"component":                 "deploy",
		"deployment_id":             "8",
		"stack":                     "web",
		"strategy":                  "canary",
		"container_count":           "2",
		"duration_ms":               "0",
		"failed_step":               deploy.STEP_CONFLICT,
		"conflicting_deployment_id": "7",
	}
	for k, v := range want {
		if a[k] != v {
			t.Errorf("%s = %q, want %q", k, a[k], v)
		}
	}
	if a["error"] == "" {
		t.Error("deployment.failed has no error")
	}
	if f := frameOf(r); f.Line != line+1 {
		t.Errorf("source = %s:%d (%s), want line %d of the caller", f.File, f.Line, f.Function, line+1)
	}

	m, raw := next()
	if m.Type != "deployment_progress" || m.Status != "failed" || m.CommandID != testCommandID {
		t.Errorf("reply = %s, want a failed deployment_progress for the command", raw)
	}
}
