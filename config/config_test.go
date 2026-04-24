package config

import (
	"os"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	os.Unsetenv("ORCHESTRATOR_URL")
	os.Unsetenv("WORKER_TOKEN")
	os.Unsetenv("WORKER_NAME")
	os.Unsetenv("HEARTBEAT_INTERVAL")
	os.Unsetenv("RECONNECT_INTERVAL")
	os.Unsetenv("DASHBOARD_PORT")
	os.Unsetenv("LATTICE_URL")
	os.Unsetenv("ALLOW_INSECURE")
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	os.Setenv("ORCHESTRATOR_URL", "wss://lattice.example.com/ws/worker")
	os.Setenv("WORKER_TOKEN", "test-token-123")
}

func TestLoadValidConfig(t *testing.T) {
	clearEnv(t)
	defer clearEnv(t)
	setRequiredEnv(t)

	cfg := Load()
	if cfg.OrchestratorURL != "wss://lattice.example.com/ws/worker" {
		t.Errorf("OrchestratorURL = %q, want wss://...", cfg.OrchestratorURL)
	}
	if cfg.WorkerToken != "test-token-123" {
		t.Errorf("WorkerToken = %q, want test-token-123", cfg.WorkerToken)
	}
	if cfg.HeartbeatInterval != 10*time.Second {
		t.Errorf("HeartbeatInterval = %v, want 10s", cfg.HeartbeatInterval)
	}
	if cfg.ReconnectInterval != 5*time.Second {
		t.Errorf("ReconnectInterval = %v, want 5s", cfg.ReconnectInterval)
	}
	if cfg.DashboardPort != "9100" {
		t.Errorf("DashboardPort = %q, want 9100", cfg.DashboardPort)
	}
}

func TestLoadCustomValues(t *testing.T) {
	clearEnv(t)
	defer clearEnv(t)
	setRequiredEnv(t)
	os.Setenv("WORKER_NAME", "my-worker")
	os.Setenv("HEARTBEAT_INTERVAL", "30s")
	os.Setenv("DASHBOARD_PORT", "9200")
	os.Setenv("LATTICE_URL", "https://lattice.example.com")

	cfg := Load()
	if cfg.WorkerName != "my-worker" {
		t.Errorf("WorkerName = %q, want my-worker", cfg.WorkerName)
	}
	if cfg.HeartbeatInterval != 30*time.Second {
		t.Errorf("HeartbeatInterval = %v, want 30s", cfg.HeartbeatInterval)
	}
	if cfg.DashboardPort != "9200" {
		t.Errorf("DashboardPort = %q, want 9200", cfg.DashboardPort)
	}
	if cfg.LatticeURL != "https://lattice.example.com" {
		t.Errorf("LatticeURL = %q, want https://lattice.example.com", cfg.LatticeURL)
	}
}

func TestLoadMissingOrchestratorURLPanics(t *testing.T) {
	clearEnv(t)
	defer clearEnv(t)
	os.Setenv("WORKER_TOKEN", "test-token")

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for missing ORCHESTRATOR_URL")
		}
	}()
	Load()
}

func TestLoadMissingWorkerTokenPanics(t *testing.T) {
	clearEnv(t)
	defer clearEnv(t)
	os.Setenv("ORCHESTRATOR_URL", "wss://lattice.example.com/ws/worker")

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for missing WORKER_TOKEN")
		}
	}()
	Load()
}

func TestLoadInsecureWSPanics(t *testing.T) {
	clearEnv(t)
	defer clearEnv(t)
	os.Setenv("ORCHESTRATOR_URL", "ws://lattice.example.com/ws/worker")
	os.Setenv("WORKER_TOKEN", "test-token")

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for ws:// without ALLOW_INSECURE")
		}
	}()
	Load()
}

func TestLoadInsecureWSAllowed(t *testing.T) {
	clearEnv(t)
	defer clearEnv(t)
	os.Setenv("ORCHESTRATOR_URL", "ws://localhost/ws/worker")
	os.Setenv("WORKER_TOKEN", "test-token")
	os.Setenv("ALLOW_INSECURE", "true")

	cfg := Load()
	if cfg.OrchestratorURL != "ws://localhost/ws/worker" {
		t.Errorf("OrchestratorURL = %q, want ws://localhost/ws/worker", cfg.OrchestratorURL)
	}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		name     string
		envKey   string
		envVal   string
		fallback time.Duration
		want     time.Duration
	}{
		{"valid", "TEST_DUR", "30s", 10 * time.Second, 30 * time.Second},
		{"empty-uses-fallback", "TEST_DUR", "", 10 * time.Second, 10 * time.Second},
		{"invalid-uses-fallback", "TEST_DUR", "not-a-duration", 10 * time.Second, 10 * time.Second},
		{"minutes", "TEST_DUR", "2m", 10 * time.Second, 2 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Unsetenv(tt.envKey)
			if tt.envVal != "" {
				os.Setenv(tt.envKey, tt.envVal)
				defer os.Unsetenv(tt.envKey)
			}
			got := parseDuration(tt.envKey, tt.fallback)
			if got != tt.want {
				t.Errorf("parseDuration(%q, %v) = %v, want %v", tt.envKey, tt.fallback, got, tt.want)
			}
		})
	}
}
