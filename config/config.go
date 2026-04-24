package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	OrchestratorURL   string
	WorkerToken       string
	WorkerName        string
	HeartbeatInterval time.Duration
	ReconnectInterval time.Duration
	DashboardPort     string
	LatticeURL        string
}

func Load() *Config {
	orchestratorURL := getEnvOrPanic("ORCHESTRATOR_URL")

	// Enforce TLS for WebSocket connections unless explicitly opted out.
	// Unencrypted connections expose the worker token and all messages (including
	// deployment specs with registry passwords and env vars) in plaintext.
	allowInsecure := strings.EqualFold(getEnv("ALLOW_INSECURE", "false"), "true")
	if !allowInsecure && strings.HasPrefix(orchestratorURL, "ws://") {
		panic("ORCHESTRATOR_URL uses unencrypted ws:// — use wss:// for production. Set ALLOW_INSECURE=true to override for local development.")
	}

	cfg := &Config{
		OrchestratorURL:   orchestratorURL,
		WorkerToken:       getEnvOrPanic("WORKER_TOKEN"),
		WorkerName:        getEnv("WORKER_NAME", hostname()),
		HeartbeatInterval: parseDuration("HEARTBEAT_INTERVAL", 10*time.Second),
		ReconnectInterval: parseDuration("RECONNECT_INTERVAL", 5*time.Second),
		DashboardPort:     getEnv("DASHBOARD_PORT", "9100"),
		LatticeURL:        getEnv("LATTICE_URL", ""),
	}
	return cfg
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func getEnvOrPanic(key string) string {
	v, ok := os.LookupEnv(key)
	if !ok {
		panic(fmt.Sprintf("missing required environment variable: '%s'", key))
	}
	return v
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func parseDuration(key string, fallback time.Duration) time.Duration {
	v := getEnv(key, "")
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}
