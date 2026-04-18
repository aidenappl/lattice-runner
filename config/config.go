package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	OrchestratorURL    string
	WorkerToken        string
	WorkerName         string
	HeartbeatInterval  time.Duration
	ReconnectInterval  time.Duration
	DashboardPort      string
}

func Load() *Config {
	cfg := &Config{
		OrchestratorURL:   getEnvOrPanic("ORCHESTRATOR_URL"),
		WorkerToken:       getEnvOrPanic("WORKER_TOKEN"),
		WorkerName:        getEnv("WORKER_NAME", hostname()),
		HeartbeatInterval: parseDuration("HEARTBEAT_INTERVAL", 15*time.Second),
		ReconnectInterval: parseDuration("RECONNECT_INTERVAL", 5*time.Second),
		DashboardPort:     getEnv("DASHBOARD_PORT", "9100"),
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
