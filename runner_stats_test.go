package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	monitor "github.com/aidenappl/go-monitor"
)

func TestHealthTrackerLogsOnlyTransitions(t *testing.T) {
	type obs struct{ container, health string }
	type logged struct {
		level    slog.Level
		from, to string
	}
	tests := []struct {
		name string
		seq  []obs
		want []logged
	}{
		{
			name: "first healthy sighting is silent, repeats are silent",
			seq:  []obs{{"api", "healthy"}, {"api", "healthy"}, {"api", "healthy"}},
			want: nil,
		},
		{
			name: "first sighting unhealthy warns",
			seq:  []obs{{"api", "unhealthy"}, {"api", "unhealthy"}},
			want: []logged{{slog.LevelWarn, "", "unhealthy"}},
		},
		{
			name: "each change logs once",
			seq: []obs{
				{"api", "starting"}, {"api", "starting"},
				{"api", "healthy"}, {"api", "healthy"},
				{"api", "unhealthy"}, {"api", "unhealthy"},
				{"api", "healthy"},
			},
			want: []logged{
				{slog.LevelInfo, "starting", "healthy"},
				{slog.LevelWarn, "healthy", "unhealthy"},
				{slog.LevelInfo, "unhealthy", "healthy"},
			},
		},
		{
			name: "containers are tracked separately",
			seq:  []obs{{"api", "healthy"}, {"web", "starting"}, {"web", "healthy"}, {"api", "healthy"}},
			want: []logged{{slog.LevelInfo, "starting", "healthy"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := capture(t)
			tr := newHealthTracker()
			for _, o := range tt.seq {
				tr.observe(context.Background(), o.container, o.health)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.records) != len(tt.want) {
				t.Fatalf("logged %d records, want %d", len(h.records), len(tt.want))
			}
			for i, r := range h.records {
				a := attrs(r)
				if r.Level != tt.want[i].level {
					t.Errorf("record %d level = %v, want %v", i, r.Level, tt.want[i].level)
				}
				if a["event"] != "container.health_changed" {
					t.Errorf("record %d event = %q", i, a["event"])
				}
				if a["from"] != tt.want[i].from || a["to"] != tt.want[i].to {
					t.Errorf("record %d from/to = %q/%q, want %q/%q", i, a["from"], a["to"], tt.want[i].from, tt.want[i].to)
				}
			}
		})
	}
}

func TestHealthTrackerRetainForgetsGoneContainers(t *testing.T) {
	h := capture(t)
	tr := newHealthTracker()
	ctx := context.Background()
	tr.observe(ctx, "api", "healthy")
	tr.retain(map[string]bool{})
	// Seen afresh: a healthy first sighting is silent.
	tr.observe(ctx, "api", "healthy")
	if len(h.records) != 0 {
		t.Fatalf("logged %d records, want 0", len(h.records))
	}
	if len(tr.last) != 1 {
		t.Fatalf("tracker holds %d containers, want 1", len(tr.last))
	}
}

type fakeQueue struct {
	length, capacity int
	drops            map[string]int64
}

func (f *fakeQueue) QueueStats() (int, int) { return f.length, f.capacity }
func (f *fakeQueue) TakeDrops() map[string]int64 {
	d := f.drops
	f.drops = map[string]int64{}
	return d
}

func TestRunnerStatsEventFields(t *testing.T) {
	h := capture(t)
	handlerSemWaits.Store(3)
	t.Cleanup(func() { handlerSemWaits.Store(0) })

	q := &fakeQueue{length: 7, capacity: 256, drops: map[string]int64{"container_sync": 4, "container_logs": 2}}
	c := &runnerStatsCollector{ws: q}
	s := c.sample()
	logRunnerStats(context.Background(), s)

	r := onlyRecord(t, h)
	if r.Level != slog.LevelInfo {
		t.Errorf("level = %v, want INFO", r.Level)
	}
	a := attrs(r)
	for _, k := range []string{
		"event", "component", "queue_len", "queue_cap", "dropped", "dropped_by_type", "inflight_handlers",
		"sem_waits", "goroutines", "heap_mb", "sdk_enqueued", "sdk_flushed", "sdk_dropped", "sdk_dropped_total",
		"sdk_pending", "sdk_pending_bytes",
	} {
		if _, ok := a[k]; !ok {
			t.Errorf("runner.stats has no %s", k)
		}
	}
	want := map[string]string{"event": "runner.stats", "queue_len": "7", "queue_cap": "256", "dropped": "6", "sem_waits": "3"}
	for k, v := range want {
		if a[k] != v {
			t.Errorf("%s = %q, want %q", k, a[k], v)
		}
	}
	if s.DroppedByType["container_sync"] != 4 || s.DroppedByType["container_logs"] != 2 {
		t.Errorf("dropped_by_type = %v", s.DroppedByType)
	}

	// Interval counters reset between samples.
	s2 := c.sample()
	if len(s2.DroppedByType) != 0 || s2.SemWaits != 0 {
		t.Errorf("second sample carried over drops %v / sem_waits %d", s2.DroppedByType, s2.SemWaits)
	}
}

func TestAcquireHandlerCountsBlockedAcquisitions(t *testing.T) {
	h := capture(t)
	handlerSemWaits.Store(0)
	t.Cleanup(func() { handlerSemWaits.Store(0) })

	for i := 0; i < cap(handlerSem); i++ {
		acquireHandler(context.Background(), "stop")
	}
	if n := handlerSemWaits.Load(); n != 0 {
		t.Fatalf("sem_waits = %d with room in the pool, want 0", n)
	}

	done := make(chan struct{})
	go func() {
		acquireHandler(context.Background(), "deploy")
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for handlerSemWaits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	releaseHandler()
	<-done
	for i := 0; i < cap(handlerSem); i++ {
		releaseHandler()
	}

	if n := handlerSemWaits.Load(); n != 1 {
		t.Errorf("sem_waits = %d, want 1", n)
	}
	r := onlyRecord(t, h)
	if r.Level != slog.LevelWarn || r.Message != "handler pool saturated" {
		t.Errorf("logged %v %q, want a warn \"handler pool saturated\"", r.Level, r.Message)
	}
}

func TestRegistryHost(t *testing.T) {
	tests := []struct{ in, want string }{
		{"nginx:latest", "docker.io"},
		{"library/nginx", "docker.io"},
		{"registry.appleby.cloud/lattice-api:latest", "registry.appleby.cloud"},
		{"localhost:5000/app", "localhost:5000"},
		{"localhost/app", "localhost"},
		{"ghcr.io/org/app:v1", "ghcr.io"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := registryHost(tt.in); got != tt.want {
				t.Errorf("registryHost(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A collector made when the stats loop (re)starts counts SDK drops from the
// total at that moment, not from zero.
func TestRunnerStatsCollectorStartsFromCurrentSDKDrops(t *testing.T) {
	var total int64 = 40
	stats := func() (monitor.ShipperStats, int64) { return monitor.ShipperStats{Dropped: total}, total }
	c := newRunnerStatsCollector(&fakeQueue{}, stats)

	if s := c.sample(); s.SDKDropped != 0 {
		t.Errorf("first sample after (re)start sdk_dropped = %d, want 0", s.SDKDropped)
	}
	total = 45
	if s := c.sample(); s.SDKDropped != 5 {
		t.Errorf("sdk_dropped = %d, want 5", s.SDKDropped)
	}
	total = 2 // shipper counters restarted by a re-Init
	if s := c.sample(); s.SDKDropped != 2 {
		t.Errorf("sdk_dropped after counter reset = %d, want 2", s.SDKDropped)
	}
}
