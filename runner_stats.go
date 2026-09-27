package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	monitor "github.com/aidenappl/go-monitor"
	dockerclient "github.com/aidenappl/lattice-runner/docker"
	"github.com/aidenappl/lattice-runner/telemetry"
)

// RUNNER_STATS_INTERVAL is how often the runner.stats event is emitted.
const RUNNER_STATS_INTERVAL = 60 * time.Second

// handlerSemWaits counts handler acquisitions that found the pool full and had
// to block, since the last runner.stats.
var handlerSemWaits atomic.Int64

// acquireHandler takes a slot in the handler pool, blocking while all of them
// are in use. It is called on the WebSocket read loop, so a full pool stalls
// every inbound command: each time that happens is counted, and logged as a
// warning that the handler rate-limits.
func acquireHandler(ctx context.Context, msgType string) {
	select {
	case handlerSem <- struct{}{}:
		return
	default:
	}
	handlerSemWaits.Add(1)
	slog.WarnContext(ctx, "handler pool saturated", "component", "runner",
		"message_type", msgType, "pool_size", cap(handlerSem))
	handlerSem <- struct{}{}
}

// releaseHandler gives back a slot taken by acquireHandler.
func releaseHandler() {
	<-handlerSem
}

// sendQueue is the part of the WebSocket client runner.stats reads.
type sendQueue interface {
	QueueStats() (length, capacity int)
	TakeDrops() map[string]int64
}

// runnerStats is one runner.stats sample. Counters named per interval
// (DroppedByType, SemWaits, SDKDropped) cover the time since the previous
// sample; the rest are point-in-time or lifetime values.
type runnerStats struct {
	QueueLen         int
	QueueCap         int
	DroppedByType    map[string]int64
	InflightHandlers int
	SemWaits         int64
	Goroutines       int
	HeapMB           float64
	SDK              monitor.ShipperStats
	SDKDropped       int64
}

// runnerStatsCollector takes runner.stats samples. It remembers the SDK drop
// total between samples so each one reports only the new drops.
type runnerStatsCollector struct {
	ws           sendQueue
	sdkStats     func() (monitor.ShipperStats, int64)
	lastSDKDrops int64
}

// newRunnerStatsCollector starts counting SDK drops from the current total, so
// a collector made when the stats loop (re)starts — after a panic restart, say
// — does not report every earlier drop as new in its first sample.
func newRunnerStatsCollector(ws sendQueue, sdkStats func() (monitor.ShipperStats, int64)) *runnerStatsCollector {
	c := &runnerStatsCollector{ws: ws, sdkStats: sdkStats}
	_, c.lastSDKDrops = c.sdk()
	return c
}

// sdk reads the SDK's shipper stats and its drop total.
func (c *runnerStatsCollector) sdk() (monitor.ShipperStats, int64) {
	get := c.sdkStats
	if get == nil {
		get = telemetry.SDKStats
	}
	sdk, total := get()
	if sdk.Dropped > total {
		total = sdk.Dropped
	}
	return sdk, total
}

func (c *runnerStatsCollector) sample() runnerStats {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	qlen, qcap := c.ws.QueueStats()
	sdk, sdkDropTotal := c.sdk()
	sdkDropped := sdkDropTotal - c.lastSDKDrops
	if sdkDropped < 0 {
		// A re-Init starts the shipper's counters again.
		sdkDropped = sdkDropTotal
	}
	c.lastSDKDrops = sdkDropTotal
	return runnerStats{
		QueueLen:         qlen,
		QueueCap:         qcap,
		DroppedByType:    c.ws.TakeDrops(),
		InflightHandlers: len(handlerSem),
		SemWaits:         handlerSemWaits.Swap(0),
		Goroutines:       runtime.NumGoroutine(),
		HeapMB:           float64(mem.HeapAlloc) / (1024 * 1024),
		SDK:              sdk,
		SDKDropped:       sdkDropped,
	}
}

// logRunnerStats emits one runner.stats event.
func logRunnerStats(ctx context.Context, s runnerStats) {
	var dropped int64
	for _, n := range s.DroppedByType {
		dropped += n
	}
	slog.InfoContext(ctx, "runner stats", "component", "runner", "event", "runner.stats",
		"queue_len", s.QueueLen,
		"queue_cap", s.QueueCap,
		"dropped", dropped,
		"dropped_by_type", s.DroppedByType,
		"inflight_handlers", s.InflightHandlers,
		"sem_waits", s.SemWaits,
		"goroutines", s.Goroutines,
		"heap_mb", s.HeapMB,
		"sdk_enqueued", s.SDK.Enqueued,
		"sdk_flushed", s.SDK.Flushed,
		"sdk_dropped", s.SDKDropped,
		"sdk_dropped_total", s.SDK.Dropped,
		"sdk_quarantined", s.SDK.Quarantined,
		"sdk_spooled", s.SDK.Spooled,
		"sdk_pending", s.SDK.Pending,
		"sdk_pending_bytes", s.SDK.PendingBytes,
	)
}

// runRunnerStats emits runner.stats every interval until ctx ends.
func runRunnerStats(ctx context.Context, ws sendQueue, interval time.Duration) {
	c := newRunnerStatsCollector(ws, telemetry.SDKStats)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logRunnerStats(cycleContext(ctx), c.sample())
		}
	}
}

// healthTracker remembers each container's last reported health so only
// changes are logged. The heartbeat still sends every container's health to
// the orchestrator each cycle; this only decides what reaches the logs.
type healthTracker struct {
	mu   sync.Mutex
	last map[string]string
}

func newHealthTracker() *healthTracker {
	return &healthTracker{last: map[string]string{}}
}

// observe records a container's health and logs container.health_changed if
// it differs from the last value seen. The first sighting of a container is
// logged only when it is unhealthy, so a runner restart does not re-announce
// every healthy container but does surface one that is already failing.
func (t *healthTracker) observe(ctx context.Context, container, health string) {
	t.mu.Lock()
	prev, seen := t.last[container]
	t.last[container] = health
	t.mu.Unlock()

	if seen && prev == health {
		return
	}
	if !seen && health != "unhealthy" {
		return
	}
	level := slog.LevelInfo
	if health == "unhealthy" {
		level = slog.LevelWarn
	}
	slog.Log(ctx, level, "container health changed", "component", "heartbeat", "event", "container.health_changed",
		"container", container, "from", prev, "to", health)
}

// retain forgets containers not in present (removed, or no longer reporting
// health), so the map does not grow without bound.
func (t *healthTracker) retain(present map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for name := range t.last {
		if !present[name] {
			delete(t.last, name)
		}
	}
}

// parseRegistryAuth reads a command's "auth" payload as registry credentials.
// Credentials that do not parse are logged, naming the registry, and the pull
// goes ahead with whatever was read, as it always has — most likely anonymous.
func parseRegistryAuth(ctx context.Context, payload map[string]any, imageRef string) *dockerclient.RegistryAuth {
	authData, ok := payload["auth"]
	if !ok {
		return nil
	}
	regAuth := &dockerclient.RegistryAuth{}
	b, err := json.Marshal(authData)
	if err == nil {
		err = json.Unmarshal(b, regAuth)
	}
	if err != nil {
		slog.WarnContext(ctx, "registry auth could not be parsed", "component", "runner",
			"registry", registryHost(imageRef), "image", imageRef, "error", err)
	}
	return regAuth
}

// registryHost is the registry an image reference pulls from, by Docker's
// rule: the first path component when it looks like a host, else Docker Hub.
func registryHost(imageRef string) string {
	first, _, found := strings.Cut(imageRef, "/")
	if found && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return "docker.io"
}
