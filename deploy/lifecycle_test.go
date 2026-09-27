package deploy

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

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

func recordAttrs(r slog.Record) map[string]string {
	m := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	return m
}

// A failed deploy is one Monitor issue: exactly one error-level record, the
// deployment.failed event, carrying what an operator needs to act on it.
func TestFailedDeployLogsExactlyOneError(t *testing.T) {
	tests := []struct {
		name string
		spec DeploymentSpec
		step string
	}{
		{
			name: "invalid container name",
			spec: DeploymentSpec{DeploymentID: 42, StackName: "shop", Strategy: "rolling",
				Containers: []ContainerSpec{{Name: "bad;name", Image: "nginx"}}},
			step: STEP_VALIDATE,
		},
		{
			name: "missing image",
			spec: DeploymentSpec{DeploymentID: 43, StackName: "shop", Strategy: "blue-green",
				Containers: []ContainerSpec{{Name: "web"}, {Name: "api", Image: "api"}}},
			step: STEP_VALIDATE,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingHandler{}
			prev := slog.Default()
			slog.SetDefault(slog.New(rec))
			t.Cleanup(func() { slog.SetDefault(prev) })

			var progress []string
			e := NewExecutor(nil, func(_ int, status, _ string, _ map[string]any) { progress = append(progress, status) })
			if err := e.Execute(context.Background(), tt.spec); err == nil {
				t.Fatal("Execute succeeded, want an error")
			}

			var errs, started []slog.Record
			for _, r := range rec.records {
				if r.Level >= slog.LevelError {
					errs = append(errs, r)
				}
				if recordAttrs(r)["event"] == EVENT_DEPLOYMENT_STARTED {
					started = append(started, r)
				}
			}
			if len(started) != 1 || started[0].Level != slog.LevelInfo {
				t.Fatalf("got %d deployment.started records, want 1 at info", len(started))
			}
			if len(errs) != 1 {
				t.Fatalf("logged %d error records, want exactly 1", len(errs))
			}
			a := recordAttrs(errs[0])
			want := map[string]string{
				"event":         EVENT_DEPLOYMENT_FAILED,
				"failed_step":   tt.step,
				"stack":         tt.spec.StackName,
				"strategy":      tt.spec.Strategy,
				"deployment_id": slog.IntValue(tt.spec.DeploymentID).String(),
			}
			for k, v := range want {
				if a[k] != v {
					t.Errorf("%s = %q, want %q", k, a[k], v)
				}
			}
			for _, k := range []string{"duration_ms", "error", "container_count"} {
				if _, ok := a[k]; !ok {
					t.Errorf("deployment.failed has no %s", k)
				}
			}
			for _, r := range rec.records {
				if recordAttrs(r)["event"] == EVENT_DEPLOYMENT_SUCCEEDED {
					t.Error("a failed deploy logged deployment.succeeded")
				}
			}
		})
	}
}
