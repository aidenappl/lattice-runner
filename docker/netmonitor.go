package docker

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// NetworkDiagnostic describes a network issue detected on a container.
type NetworkDiagnostic struct {
	ContainerName string
	ContainerID   string
	Issue         string // "bridge_only", "dns_failure", "missing_network"
	Detail        string // human-readable description
	Repaired      bool   // true if auto-repair succeeded
	RepairDetail  string // what repair action was taken
}

// NetMonitorCallback is called for each diagnostic event.
type NetMonitorCallback func(diag NetworkDiagnostic)

// NetMonitor periodically checks container network health and attempts repairs.
type NetMonitor struct {
	docker   *Client
	callback NetMonitorCallback
	interval time.Duration

	mu            sync.Mutex
	restartCounts map[string]*restartTracker // containerID -> tracker
}

type restartTracker struct {
	lastCount int
	firstSeen time.Time
	reported  bool
}

// RestartLoopEvent is emitted when a container is detected in a restart loop.
type RestartLoopEvent struct {
	ContainerName string
	ContainerID   string
	RestartCount  int
	ExitCode      int
	Message       string
}

// RestartLoopCallback is called when a restart loop is detected.
type RestartLoopCallback func(event RestartLoopEvent)

func NewNetMonitor(docker *Client, callback NetMonitorCallback, interval time.Duration) *NetMonitor {
	return &NetMonitor{
		docker:        docker,
		callback:      callback,
		interval:      interval,
		restartCounts: make(map[string]*restartTracker),
	}
}

// Run starts the periodic network health check loop.
func (nm *NetMonitor) Run(ctx context.Context, restartCb RestartLoopCallback) {
	ticker := time.NewTicker(nm.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			nm.check(ctx, restartCb)
		}
	}
}

func (nm *NetMonitor) check(ctx context.Context, restartCb RestartLoopCallback) {
	containers, err := nm.docker.ListContainers(ctx, "")
	if err != nil {
		log.Printf("netmonitor: failed to list containers: %v", err)
		return
	}

	for _, c := range containers {
		if c.State != "running" && c.State != "restarting" {
			continue
		}

		name := ""
		if len(c.Names) > 0 {
			name = CanonicalContainerName(strings.TrimPrefix(c.Names[0], "/"))
		}
		if name == "" {
			continue
		}

		// Check for restart loops via container inspect
		nm.checkRestartLoop(ctx, c.ID, name, restartCb)

		// Network checks only make sense for running containers
		if c.State != "running" {
			continue
		}

		nm.checkContainerNetwork(ctx, c.ID, name)
	}
}

func (nm *NetMonitor) checkRestartLoop(ctx context.Context, containerID, name string, cb RestartLoopCallback) {
	info, err := nm.docker.InspectContainer(ctx, containerID)
	if err != nil {
		return
	}

	restartCount := info.RestartCount
	exitCode := 0
	if info.State != nil {
		exitCode = info.State.ExitCode
	}

	nm.mu.Lock()
	tracker, exists := nm.restartCounts[containerID]
	if !exists {
		nm.restartCounts[containerID] = &restartTracker{
			lastCount: restartCount,
			firstSeen: time.Now(),
		}
		nm.mu.Unlock()
		return
	}

	// Detect rapid restarts: count increased since last check
	if restartCount > tracker.lastCount && restartCount >= 3 && !tracker.reported {
		tracker.reported = true
		tracker.lastCount = restartCount
		nm.mu.Unlock()

		msg := fmt.Sprintf("Container %s has restarted %d times (exit code: %d). Possible crash loop detected.", name, restartCount, exitCode)
		cb(RestartLoopEvent{
			ContainerName: name,
			ContainerID:   containerID,
			RestartCount:  restartCount,
			ExitCode:      exitCode,
			Message:       msg,
		})
		return
	}

	// Reset reported flag if container stabilizes (no new restarts for 5 minutes)
	if restartCount == tracker.lastCount && tracker.reported && time.Since(tracker.firstSeen) > 5*time.Minute {
		tracker.reported = false
		tracker.firstSeen = time.Now()
	}

	tracker.lastCount = restartCount
	nm.mu.Unlock()
}

func (nm *NetMonitor) checkContainerNetwork(ctx context.Context, containerID, name string) {
	networks, err := nm.docker.ContainerNetworks(ctx, containerID)
	if err != nil {
		return
	}

	// Detect containers only on the default bridge network (no custom networks).
	// This is the root cause of DNS resolution failures — containers on the
	// default bridge use the host's DNS resolver instead of Docker's internal DNS.
	hasCustomNetwork := false
	onBridge := false
	for _, net := range networks {
		if net == "bridge" {
			onBridge = true
		} else if net != "host" && net != "none" {
			hasCustomNetwork = true
		}
	}

	if onBridge && !hasCustomNetwork {
		diag := NetworkDiagnostic{
			ContainerName: name,
			ContainerID:   containerID,
			Issue:         "bridge_only",
			Detail:        fmt.Sprintf("Container %s is only connected to the default bridge network. DNS resolution to other containers will fail. Networks: %v", name, networks),
		}

		// Attempt auto-repair: find a custom network this container should be on.
		// Look for networks that other lattice-managed containers are using.
		repaired := nm.attemptNetworkRepair(ctx, containerID, name)
		if repaired != "" {
			diag.Repaired = true
			diag.RepairDetail = fmt.Sprintf("Connected container to network %q", repaired)
		}

		nm.callback(diag)
	}

	// Test DNS resolution inside the container
	nm.checkDNS(ctx, containerID, name)
}

func (nm *NetMonitor) attemptNetworkRepair(ctx context.Context, containerID, name string) string {
	// Find custom networks that other lattice-managed containers use
	allContainers, err := nm.docker.ListContainers(ctx, "")
	if err != nil {
		return ""
	}

	// Build a set of custom networks used by managed containers
	candidateNetworks := make(map[string]int) // network -> usage count
	for _, c := range allContainers {
		if c.ID == containerID {
			continue
		}
		// Only look at lattice-managed containers
		if c.Labels["managed-by"] != "lattice" {
			continue
		}
		for netName := range c.NetworkSettings.Networks {
			if netName != "bridge" && netName != "host" && netName != "none" {
				candidateNetworks[netName]++
			}
		}
	}

	// Connect to the most commonly used network
	var bestNet string
	var bestCount int
	for net, count := range candidateNetworks {
		if count > bestCount {
			bestNet = net
			bestCount = count
		}
	}

	if bestNet == "" {
		return ""
	}

	if err := nm.docker.ConnectNetwork(ctx, bestNet, containerID); err != nil {
		log.Printf("netmonitor: failed to connect %s to network %s: %v", name, bestNet, err)
		return ""
	}

	log.Printf("netmonitor: connected %s to network %s (was bridge-only)", name, bestNet)
	return bestNet
}

func (nm *NetMonitor) checkDNS(ctx context.Context, containerID, name string) {
	// Quick DNS check: try to read /etc/resolv.conf to see if Docker DNS is configured
	output, err := nm.docker.ExecInContainer(ctx, containerID, []string{"cat", "/etc/resolv.conf"})
	if err != nil {
		// Container might not have cat — skip DNS check
		return
	}

	// Docker's internal DNS is at 127.0.0.11
	// If we see a different nameserver (like 10.x.x.x), DNS is using host resolver
	if !strings.Contains(output, "127.0.0.11") {
		nm.callback(NetworkDiagnostic{
			ContainerName: name,
			ContainerID:   containerID,
			Issue:         "dns_failure",
			Detail:        fmt.Sprintf("Container %s is not using Docker's internal DNS (127.0.0.11). Container-to-container DNS resolution will fail. resolv.conf shows: %s", name, strings.TrimSpace(output)),
		})
	}
}
