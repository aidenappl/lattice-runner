package metrics

import (
	"context"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

// SystemMetrics holds collected system metrics.
type SystemMetrics struct {
	CPUPercent     float64 `json:"cpu_percent"`
	MemoryUsedMB   float64 `json:"memory_used_mb"`
	MemoryTotalMB  float64 `json:"memory_total_mb"`
	DiskUsedMB     float64 `json:"disk_used_mb"`
	DiskTotalMB    float64 `json:"disk_total_mb"`
	ContainerCount int     `json:"container_count"`
	NetworkRxBytes int64   `json:"network_rx_bytes"`
	NetworkTxBytes int64   `json:"network_tx_bytes"`
}

// Collect gathers current system metrics.
func Collect(ctx context.Context, docker *dockerclient.Client) SystemMetrics {
	m := SystemMetrics{}

	// Memory from Go runtime (approximate for the host)
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	m.MemoryUsedMB = float64(memStats.Sys) / 1024 / 1024

	// Try to read /proc/meminfo for total memory (Linux only)
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		m.MemoryTotalMB = parseMemInfo(string(data), "MemTotal")
		memAvail := parseMemInfo(string(data), "MemAvailable")
		if m.MemoryTotalMB > 0 && memAvail > 0 {
			m.MemoryUsedMB = m.MemoryTotalMB - memAvail
		}
	}

	// CPU load average (Linux)
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) > 0 {
			if load, err := strconv.ParseFloat(parts[0], 64); err == nil {
				cpuCount := float64(runtime.NumCPU())
				if cpuCount > 0 {
					m.CPUPercent = (load / cpuCount) * 100
				}
			}
		}
	}

	// Disk usage - try reading /proc/mounts or use a simple check
	if data, err := os.ReadFile("/proc/diskstats"); err == nil {
		_ = data // disk stats parsing would go here for production
	}

	// Container count from Docker
	if docker != nil {
		containers, err := docker.ListContainers(ctx, "")
		if err == nil {
			m.ContainerCount = len(containers)
		} else {
			log.Printf("metrics: failed to list containers: %v", err)
		}
	}

	// Network stats (Linux)
	if data, err := os.ReadFile("/proc/net/dev"); err == nil {
		rx, tx := parseNetDev(string(data))
		m.NetworkRxBytes = rx
		m.NetworkTxBytes = tx
	}

	return m
}

func parseMemInfo(data, key string) float64 {
	for _, line := range strings.Split(data, "\n") {
		if strings.HasPrefix(line, key+":") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if val, err := strconv.ParseFloat(fields[1], 64); err == nil {
					return val / 1024 // kB to MB
				}
			}
		}
	}
	return 0
}

func parseNetDev(data string) (rx, tx int64) {
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "eth0:") || strings.HasPrefix(line, "ens") || strings.HasPrefix(line, "enp") {
			fields := strings.Fields(line)
			if len(fields) >= 10 {
				if r, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					rx += r
				}
				if t, err := strconv.ParseInt(fields[9], 10, 64); err == nil {
					tx += t
				}
			}
		}
	}
	return
}
