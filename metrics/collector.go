package metrics

import (
	"context"
	"log"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

// SystemMetrics holds collected system metrics.
type SystemMetrics struct {
	// CPU
	CPUPercent float64 `json:"cpu_percent"`
	CPUCores   int     `json:"cpu_cores"`
	LoadAvg1   float64 `json:"load_avg_1"`
	LoadAvg5   float64 `json:"load_avg_5"`
	LoadAvg15  float64 `json:"load_avg_15"`

	// Memory
	MemoryUsedMB  float64 `json:"memory_used_mb"`
	MemoryTotalMB float64 `json:"memory_total_mb"`
	MemoryFreeMB  float64 `json:"memory_free_mb"`
	SwapUsedMB    float64 `json:"swap_used_mb"`
	SwapTotalMB   float64 `json:"swap_total_mb"`

	// Disk
	DiskUsedMB  float64 `json:"disk_used_mb"`
	DiskTotalMB float64 `json:"disk_total_mb"`

	// Containers
	ContainerCount        int `json:"container_count"`
	ContainerRunningCount int `json:"container_running_count"`

	// Network (cumulative bytes since boot)
	NetworkRxBytes int64 `json:"network_rx_bytes"`
	NetworkTxBytes int64 `json:"network_tx_bytes"`

	// System
	UptimeSeconds float64 `json:"uptime_seconds"`
	ProcessCount  int     `json:"process_count"`
}

// cpuSample stores a snapshot of /proc/stat for delta calculation.
type cpuSample struct {
	idle  uint64
	total uint64
}

var (
	prevCPU   cpuSample
	prevCPUMu sync.Mutex
)

// Collect gathers current system metrics.
func Collect(ctx context.Context, docker *dockerclient.Client) SystemMetrics {
	m := SystemMetrics{
		CPUCores: runtime.NumCPU(),
	}

	// CPU usage from /proc/stat (delta between two samples)
	m.CPUPercent = measureCPU()

	// Load averages
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) >= 3 {
			m.LoadAvg1, _ = strconv.ParseFloat(parts[0], 64)
			m.LoadAvg5, _ = strconv.ParseFloat(parts[1], 64)
			m.LoadAvg15, _ = strconv.ParseFloat(parts[2], 64)
		}
		// Process count from field 4: "running/total"
		if len(parts) >= 4 {
			if slash := strings.Index(parts[3], "/"); slash > 0 {
				m.ProcessCount, _ = strconv.Atoi(parts[3][slash+1:])
			}
		}
	}

	// Memory from /proc/meminfo
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		s := string(data)
		memTotal := parseMemInfoKB(s, "MemTotal")
		memAvail := parseMemInfoKB(s, "MemAvailable")
		memFree := parseMemInfoKB(s, "MemFree")
		swapTotal := parseMemInfoKB(s, "SwapTotal")
		swapFree := parseMemInfoKB(s, "SwapFree")

		m.MemoryTotalMB = round1(float64(memTotal) / 1024)
		m.MemoryFreeMB = round1(float64(memFree) / 1024)
		if memAvail > 0 {
			m.MemoryUsedMB = round1(float64(memTotal-memAvail) / 1024)
		} else {
			m.MemoryUsedMB = round1(float64(memTotal-memFree) / 1024)
		}
		m.SwapTotalMB = round1(float64(swapTotal) / 1024)
		m.SwapUsedMB = round1(float64(swapTotal-swapFree) / 1024)
	}

	// Disk usage via statfs on root filesystem
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err == nil {
		totalBytes := stat.Blocks * uint64(stat.Bsize)
		freeBytes := stat.Bavail * uint64(stat.Bsize)
		m.DiskTotalMB = round1(float64(totalBytes) / 1024 / 1024)
		m.DiskUsedMB = round1(float64(totalBytes-freeBytes) / 1024 / 1024)
	}

	// Container stats from Docker
	if docker != nil {
		containers, err := docker.ListContainers(ctx, "")
		if err == nil {
			m.ContainerCount = len(containers)
			for _, c := range containers {
				if c.State == "running" {
					m.ContainerRunningCount++
				}
			}
		} else {
			log.Printf("metrics: failed to list containers: %v", err)
		}
	}

	// Network stats — sum all physical interfaces
	if data, err := os.ReadFile("/proc/net/dev"); err == nil {
		m.NetworkRxBytes, m.NetworkTxBytes = parseNetDev(string(data))
	}

	// Uptime
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) >= 1 {
			m.UptimeSeconds, _ = strconv.ParseFloat(parts[0], 64)
		}
	}

	return m
}

// measureCPU reads /proc/stat, compares to previous sample, and returns
// actual CPU utilization as a percentage (0-100).
func measureCPU() float64 {
	current := readCPUSample()
	if current.total == 0 {
		return 0
	}

	prevCPUMu.Lock()
	prev := prevCPU
	prevCPU = current
	prevCPUMu.Unlock()

	if prev.total == 0 {
		// First sample — no delta yet, return 0
		return 0
	}

	totalDelta := current.total - prev.total
	idleDelta := current.idle - prev.idle

	if totalDelta == 0 {
		return 0
	}

	return round1(float64(totalDelta-idleDelta) / float64(totalDelta) * 100)
}

func readCPUSample() cpuSample {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}
	}

	// First line: cpu  user nice system idle iowait irq softirq steal ...
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)
			if len(fields) < 5 {
				return cpuSample{}
			}

			var total, idle uint64
			for i := 1; i < len(fields); i++ {
				v, _ := strconv.ParseUint(fields[i], 10, 64)
				total += v
				if i == 4 { // idle is field index 4
					idle = v
				}
				if i == 5 { // iowait is also idle time
					idle += v
				}
			}

			return cpuSample{idle: idle, total: total}
		}
	}
	return cpuSample{}
}

// parseMemInfoKB extracts a value in kB from /proc/meminfo.
func parseMemInfoKB(data, key string) uint64 {
	prefix := key + ":"
	for _, line := range strings.Split(data, "\n") {
		if strings.HasPrefix(line, prefix) {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, _ := strconv.ParseUint(fields[1], 10, 64)
				return v
			}
		}
	}
	return 0
}

// parseNetDev sums RX/TX bytes across all physical interfaces, excluding
// virtual/loopback (lo, docker, br-, veth, virbr).
func parseNetDev(data string) (rx, tx int64) {
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:colonIdx])

		// Skip virtual interfaces
		if iface == "lo" ||
			strings.HasPrefix(iface, "docker") ||
			strings.HasPrefix(iface, "br-") ||
			strings.HasPrefix(iface, "veth") ||
			strings.HasPrefix(iface, "virbr") {
			continue
		}

		fields := strings.Fields(line[colonIdx+1:])
		if len(fields) < 10 {
			continue
		}

		if r, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
			rx += r
		}
		if t, err := strconv.ParseInt(fields[8], 10, 64); err == nil {
			tx += t
		}
	}
	return
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}
