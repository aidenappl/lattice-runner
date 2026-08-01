package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/aidenappl/lattice-runner/client"
	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

// dbSyncInterval is how often the runner proactively reports the database
// containers it can see, independently of the orchestrator asking. Combined
// with the orchestrator's own periodic request and its request on reconnect,
// this means a database's true state is never more than this stale.
const dbSyncInterval = 60 * time.Second

// probeHostPort reports whether a TCP port can actually be bound on this host.
//
// This is a genuine bind rather than a scan of /proc or `ss` output: checking
// whether something is listening and then binding later leaves a race window,
// and a bind is the same operation Docker will perform, so it fails for exactly
// the same reasons. The listener is closed immediately — the window between
// this and the container binding is why the orchestrator also keeps its own
// allocation ledger.
func probeHostPort(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}
	return ln.Close()
}

// dockerHealthToLattice maps Docker's health vocabulary onto the platform's.
//
// Docker reports "starting" for the whole of a container's start_period, which
// for a cold database is a legitimate state rather than a failure — a database
// running InnoDB recovery or first-time initdb is neither healthy nor broken.
func dockerHealthToLattice(state *dockerclient.ContainerHealth) string {
	if state == nil || state.Status == "" {
		return "none"
	}
	switch state.Status {
	case "healthy":
		return "healthy"
	case "unhealthy":
		return "unhealthy"
	case "starting":
		return "starting"
	}
	return "none"
}

// fatalInitSignatures are log fragments that mean a database container will
// never come up on its own, whatever the restart policy tries. Almost every
// failed managed-database provision lands on one of these: the volume has the
// wrong ownership, or a data directory was mounted where the engine expected to
// initialise a fresh one.
var fatalInitSignatures = []struct {
	needle string
	hint   string
}{
	{"has wrong ownership", "the data volume has wrong ownership — the engine cannot write to its data directory"},
	{"Permission denied", "permission denied writing to the data volume"},
	{"wrong permissions", "the data directory has wrong permissions"},
	{"[ERROR] Aborting", "the engine aborted during startup — see the container logs for the preceding error"},
	{"initdb: error:", "Postgres could not initialise its data directory"},
	{"exists but is not empty", "the Postgres data directory exists but is not empty — mount a subdirectory, not the volume root"},
	{"Unable to lock", "another process is holding the data directory lock"},
	{"InnoDB: Unable to lock", "InnoDB cannot lock its data files — a previous instance may still be running"},
	{"Access denied for user", "the engine rejected the configured credentials"},
}

// detectFatalInitError tails a container's logs looking for a known-fatal
// startup failure and returns a human-readable hint, or "" if none matched.
func detectFatalInitError(ctx context.Context, docker *dockerclient.Client, containerID string) string {
	reader, err := docker.ContainerLogs(ctx, containerID, "80")
	if err != nil {
		return ""
	}
	defer reader.Close()

	// Cap the read: a flapping container can produce a lot of output, and the
	// signature we're looking for is always near the end of a failed startup.
	buf := make([]byte, 32*1024)
	n, _ := io.ReadFull(reader, buf)
	if n <= 0 {
		return ""
	}
	text := string(buf[:n])

	for _, sig := range fatalInitSignatures {
		if strings.Contains(text, sig.needle) {
			return sig.hint
		}
	}
	return ""
}

// startDatabaseObserver reports observed database container state to the
// orchestrator on a fixed interval.
//
// The orchestrator treats this as the observed half of its reconcile loop: it
// diffs this report against what it believes and corrects the difference. That
// is what makes a lost or mishandled db_status message self-correcting instead
// of permanently stranding an instance in a transitional state.
func startDatabaseObserver(ctx context.Context, ws *client.WSClient, docker *dockerclient.Client) {
	ticker := time.NewTicker(dbSyncInterval)
	defer ticker.Stop()

	// Report immediately on start rather than waiting a full interval — a
	// runner that just restarted is exactly when the orchestrator's view is
	// most likely to be stale.
	sendDatabaseSync(ctx, ws, docker)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendDatabaseSync(ctx, ws, docker)
		}
	}
}

// volumeSizeCache holds the last measured size of each data volume.
//
// Measuring walks the volume, which is O(files), so it happens on its own slow
// cadence and every db_sync in between reports the cached figure. The
// alternative — `docker system df -v` — holds the daemon's container lock while
// it computes and would wedge `docker ps` for the whole host.
var (
	volumeSizeMu    sync.Mutex
	volumeSizeCache = map[string]volumeSizeEntry{}
)

type volumeSizeEntry struct {
	bytes      int64
	measuredAt time.Time
}

// dbVolumeSizeTTL is how stale a volume measurement may get. A database's
// footprint changes over hours, not seconds, and the walk is expensive.
const dbVolumeSizeTTL = time.Hour

// cachedVolumeSize returns the last measured size, refreshing it if stale.
// Returns false when no measurement has ever succeeded.
func cachedVolumeSize(ctx context.Context, docker *dockerclient.Client, containerID string) (int64, bool) {
	volumeName, err := docker.DatabaseVolumeName(ctx, containerID)
	if err != nil || volumeName == "" {
		return 0, false
	}

	volumeSizeMu.Lock()
	entry, ok := volumeSizeCache[volumeName]
	volumeSizeMu.Unlock()

	if ok && time.Since(entry.measuredAt) < dbVolumeSizeTTL {
		return entry.bytes, true
	}

	size, err := docker.VolumeSize(ctx, volumeName)
	if err != nil {
		log.Printf("db_sync: failed to measure volume %s: %v", volumeName, err)
		// Keep serving the stale figure rather than reporting nothing: an old
		// size is far more useful than a blank where a growth trend should be.
		return entry.bytes, ok
	}

	volumeSizeMu.Lock()
	volumeSizeCache[volumeName] = volumeSizeEntry{bytes: size, measuredAt: time.Now()}
	volumeSizeMu.Unlock()
	return size, true
}

// sendDatabaseSync enumerates every Lattice-managed database container on this
// host and reports its state, health, restart count and data volume size.
func sendDatabaseSync(ctx context.Context, ws *client.WSClient, docker *dockerclient.Client) {
	observed, err := docker.ListDatabaseContainers(ctx)
	if err != nil {
		log.Printf("db_sync: failed to list database containers: %v", err)
		return
	}

	entries := make([]map[string]any, 0, len(observed))
	for _, c := range observed {
		entry := map[string]any{
			"container_name": c.Name,
			"container_id":   c.ID,
			"state":          c.State,
			"health":         dockerHealthToLattice(c.Health),
			"restart_count":  c.RestartCount,
		}

		// A restart count alone says a container is unhappy but not why. When
		// one is clearly flapping, look for the handful of fatal signatures that
		// account for nearly every failed database init, so the control plane
		// can report the actual cause instead of "it keeps restarting".
		if c.RestartCount >= 3 || c.State == "restarting" {
			if hint := detectFatalInitError(ctx, docker, c.ID); hint != "" {
				entry["fatal_hint"] = hint
			}
		}

		// What the database actually costs on disk. Nothing in the platform has
		// ever tracked this: the only disk figure collected anywhere is the
		// worker's root filesystem, so a database filling its host was invisible
		// until it took every other container down with it.
		if size, ok := cachedVolumeSize(ctx, docker, c.ID); ok {
			entry["volume_size_bytes"] = size
		}

		entries = append(entries, entry)
	}

	wsSendReliable(ws, "db_sync", client.OutgoingMessage{
		Type: "db_sync",
		Payload: map[string]any{
			"containers": entries,
		},
	})
}
