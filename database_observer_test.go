package main

import (
	"fmt"
	"net"
	"testing"

	"github.com/aidenappl/lattice-runner/client"
	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

func TestProbeHostPort(t *testing.T) {
	t.Run("free port succeeds", func(t *testing.T) {
		// Ask the OS for an unused port, release it, then prove the probe
		// accepts it.
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatalf("failed to reserve a port: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		if err := ln.Close(); err != nil {
			t.Fatalf("failed to release port: %v", err)
		}

		if err := probeHostPort(port); err != nil {
			t.Errorf("probeHostPort(%d) on a free port returned %v", port, err)
		}
	})

	t.Run("occupied port fails", func(t *testing.T) {
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatalf("failed to reserve a port: %v", err)
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port

		if err := probeHostPort(port); err == nil {
			t.Errorf("probeHostPort(%d) succeeded while the port was held", port)
		}
	})

	t.Run("probe releases the port so the caller can bind it", func(t *testing.T) {
		// The probe must not leave the port held, or the container create it
		// guards would fail for the probe's own listener.
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatalf("failed to reserve a port: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()

		if err := probeHostPort(port); err != nil {
			t.Fatalf("first probe failed: %v", err)
		}
		if err := probeHostPort(port); err != nil {
			t.Fatalf("second probe failed — the first did not release the port: %v", err)
		}
	})
}

func TestDockerHealthToLattice(t *testing.T) {
	tests := []struct {
		name  string
		input *dockerclient.ContainerHealth
		want  string
	}{
		{"nil health", nil, "none"},
		{"no healthcheck configured", &dockerclient.ContainerHealth{Status: ""}, "none"},
		{"healthy", &dockerclient.ContainerHealth{Status: "healthy"}, "healthy"},
		{"unhealthy", &dockerclient.ContainerHealth{Status: "unhealthy"}, "unhealthy"},
		// A cold database inside its start_period reports "starting". That is a
		// legitimate state, not a failure — mapping it to unhealthy would flag
		// every database that takes more than a moment to initialise.
		{"starting", &dockerclient.ContainerHealth{Status: "starting"}, "starting"},
		{"unrecognised", &dockerclient.ContainerHealth{Status: "weird"}, "none"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dockerHealthToLattice(tt.input); got != tt.want {
				t.Errorf("dockerHealthToLattice() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFatalInitSignaturesAreNonEmpty(t *testing.T) {
	if len(fatalInitSignatures) == 0 {
		t.Fatal("no fatal init signatures defined")
	}
	for i, sig := range fatalInitSignatures {
		if sig.needle == "" {
			t.Errorf("signature %d has an empty needle", i)
		}
		if sig.hint == "" {
			t.Errorf("signature %d (%q) has no hint — the whole point is to explain the failure", i, sig.needle)
		}
	}
}

func TestScheduledEnvCarriesInstanceID(t *testing.T) {
	// Scheduled snapshots are not triggered by an incoming command, so they have
	// no envelope to echo. They must still correlate to their instance or their
	// status updates are dropped exactly like manual ones used to be.
	env := scheduledEnv(77)
	got, ok := env.Payload["database_instance_id"]
	if !ok {
		t.Fatal("scheduledEnv did not set database_instance_id")
	}
	if got != 77 {
		t.Errorf("database_instance_id = %v, want 77", got)
	}
}

// TestSendDbReplyCorrelation covers the contract that every reply must satisfy.
// The original implementation built bare payload literals with no instance ID,
// which is why no managed database could ever leave "pending".
func TestSendDbReplyCorrelation(t *testing.T) {
	tests := []struct {
		name      string
		env       client.Envelope
		payload   map[string]any
		wantPhase string
		wantID    any
	}{
		{
			name: "echoes correlation from the command",
			env: client.Envelope{
				Type: "db_create",
				Payload: map[string]any{
					"database_instance_id": float64(12),
					"request_id":           "req-1",
					"idempotency_key":      "db_create:12",
				},
			},
			payload:   map[string]any{"status": "success"},
			wantPhase: "completed",
			wantID:    float64(12),
		},
		{
			name: "maps a failure to the failed phase",
			env: client.Envelope{
				Type:    "db_start",
				Payload: map[string]any{"database_instance_id": float64(5)},
			},
			payload:   map[string]any{"status": "failed", "message": "boom"},
			wantPhase: "failed",
			wantID:    float64(5),
		},
		{
			name: "treats an unrecognised status as an acknowledgement",
			env: client.Envelope{
				Type:    "db_stop",
				Payload: map[string]any{"database_instance_id": float64(9)},
			},
			payload:   map[string]any{"status": "accepted"},
			wantPhase: "ack",
			wantID:    float64(9),
		},
		{
			name:      "falls back to instance_id from the scheduler path",
			env:       client.Envelope{Type: "db_snapshot", Payload: map[string]any{}},
			payload:   map[string]any{"status": "completed", "instance_id": 31},
			wantPhase: "completed",
			wantID:    31,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// sendDbReply writes to a live socket, so exercise the payload
			// construction it performs via the same rules rather than the
			// transport. Keep this in step with sendDbReply.
			got := buildDbReplyPayload(tt.env, tt.payload)

			if got["database_instance_id"] != tt.wantID {
				t.Errorf("database_instance_id = %v (%T), want %v (%T)",
					got["database_instance_id"], got["database_instance_id"], tt.wantID, tt.wantID)
			}
			if got["phase"] != tt.wantPhase {
				t.Errorf("phase = %v, want %v", got["phase"], tt.wantPhase)
			}
			if got["action"] != tt.env.Type {
				t.Errorf("action = %v, want %v", got["action"], tt.env.Type)
			}
		})
	}
}

func TestBuildDbReplyPayloadDoesNotOverwriteExplicitFields(t *testing.T) {
	env := client.Envelope{
		Type:    "db_create",
		Payload: map[string]any{"database_instance_id": float64(1)},
	}
	got := buildDbReplyPayload(env, map[string]any{
		"action": "db_custom",
		"phase":  "completed",
		"status": "failed",
	})

	if got["action"] != "db_custom" {
		t.Errorf("action was overwritten: got %v", got["action"])
	}
	if got["phase"] != "completed" {
		t.Errorf("explicit phase was overwritten: got %v", got["phase"])
	}
}

func TestBuildDbReplyPayloadHandlesNilPayload(t *testing.T) {
	env := client.Envelope{Type: "db_stop", Payload: map[string]any{}}
	got := buildDbReplyPayload(env, nil)
	if got == nil {
		t.Fatal("buildDbReplyPayload(nil) returned nil")
	}
	if got["action"] != "db_stop" {
		t.Errorf("action = %v, want db_stop", got["action"])
	}
}

func TestMemoryLimitUnitGuard(t *testing.T) {
	// The orchestrator sends bytes. A value below Docker's 6MB floor can only be
	// a caller that sent megabytes — the bug that made every create fail, since
	// a 512 "MB" request arrived as 512 bytes and Docker rejected it outright.
	tests := []struct {
		name  string
		input int64
		want  int64
	}{
		{"unset", 0, 0},
		{"megabytes mistaken for bytes", 512, 512 * 1024 * 1024},
		{"small megabyte value", 256, 256 * 1024 * 1024},
		{"already bytes", 512 * 1024 * 1024, 512 * 1024 * 1024},
		{"exactly the docker floor", 6 * 1024 * 1024, 6 * 1024 * 1024},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normaliseMemoryLimit(tt.input)
			if got != tt.want {
				t.Errorf("normaliseMemoryLimit(%d) = %d, want %d (%s)",
					tt.input, got, tt.want, fmt.Sprintf("%.0fMB", float64(tt.want)/(1024*1024)))
			}
		})
	}
}
