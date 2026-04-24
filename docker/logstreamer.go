package docker

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

// LogLine represents a single log line from a container.
type LogLine struct {
	ContainerID   string
	ContainerName string
	Stream        string // "stdout" or "stderr"
	Message       string
	RecordedAt    time.Time // Docker-recorded timestamp (nanosecond precision)
}

// CanonicalContainerName strips Lattice deploy suffixes (6-char alphanumeric,
// "-retired-*", "-lattice-updating") so log lines are attributed to the
// canonical container name that matches the DB entry. This prevents log gaps
// when the logstreamer discovers a container before the rolling deploy renames it.
func CanonicalContainerName(name string) string {
	// Strip -retired-<timestamp> or -lattice-updating
	if idx := strings.Index(name, "-retired-"); idx > 0 {
		return name[:idx]
	}
	if strings.HasSuffix(name, "-lattice-updating") {
		return strings.TrimSuffix(name, "-lattice-updating")
	}
	// Strip 6-char alphanumeric deploy suffix (e.g. "openbucket-zixn9i" -> "openbucket")
	if dashIdx := strings.LastIndex(name, "-"); dashIdx > 0 {
		suffix := name[dashIdx+1:]
		if len(suffix) == 6 {
			allValid := true
			for _, c := range suffix {
				if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
					allValid = false
					break
				}
			}
			if allValid {
				return name[:dashIdx]
			}
		}
	}
	return name
}

// LogCallback is called for each log line from a container.
type LogCallback func(line LogLine)

type streamEntry struct {
	cancel context.CancelFunc
	done   chan struct{} // closed when the stream goroutine exits
}

// LogStreamer watches running containers and streams their logs.
type LogStreamer struct {
	docker   *Client
	callback LogCallback
	interval time.Duration

	mu      sync.Mutex
	tracked map[string]*streamEntry // containerID -> entry
}

func NewLogStreamer(docker *Client, callback LogCallback, pollInterval time.Duration) *LogStreamer {
	return &LogStreamer{
		docker:   docker,
		callback: callback,
		interval: pollInterval,
		tracked:  make(map[string]*streamEntry),
	}
}

// Run polls for running containers and starts/stops log streams as needed.
func (ls *LogStreamer) Run(ctx context.Context) {
	ticker := time.NewTicker(ls.interval)
	defer ticker.Stop()

	// Initial scan
	ls.sync(ctx)

	for {
		select {
		case <-ctx.Done():
			ls.stopAll()
			return
		case <-ticker.C:
			ls.sync(ctx)
		}
	}
}

func (ls *LogStreamer) sync(ctx context.Context) {
	containers, err := ls.docker.ListContainers(ctx, "")
	if err != nil {
		log.Printf("logstreamer: failed to list containers: %v", err)
		return
	}

	// Stream logs from running AND restarting containers so restart-loop
	// output is captured. Containers in other states (exited, dead, created)
	// are not streamable — Docker's follow mode requires the container to be
	// alive. Their final logs are still reachable via the one-shot tail in
	// doStream before the stream naturally ends.
	streamable := make(map[string]string) // id -> name
	for _, c := range containers {
		switch c.State {
		case "running", "restarting":
			name := ""
			if len(c.Names) > 0 {
				name = CanonicalContainerName(strings.TrimPrefix(c.Names[0], "/"))
			}
			streamable[c.ID] = name
		}
	}

	ls.mu.Lock()
	defer ls.mu.Unlock()

	// Remove entries for goroutines that exited unexpectedly (e.g. before the
	// reconnect loop was in place) so they are restarted below.
	for id, entry := range ls.tracked {
		select {
		case <-entry.done:
			log.Printf("logstreamer: detected dead stream for %s, will restart", id)
			delete(ls.tracked, id)
		default:
		}
	}

	// Start streaming new (or recovered) containers
	for id, name := range streamable {
		if _, ok := ls.tracked[id]; !ok {
			streamCtx, cancel := context.WithCancel(ctx)
			entry := &streamEntry{cancel: cancel, done: make(chan struct{})}
			ls.tracked[id] = entry
			go ls.stream(streamCtx, id, name, entry.done)
		}
	}

	// Stop streaming removed containers (truly gone, not just restarting)
	for id, entry := range ls.tracked {
		if _, ok := streamable[id]; !ok {
			entry.cancel()
			delete(ls.tracked, id)
		}
	}
}

// stream is the outer reconnect loop for a single container. It retries the
// Docker log stream whenever it disconnects unexpectedly (e.g. after a
// container restart) as long as the context has not been cancelled.
// done is closed when this goroutine exits so sync() can detect it.
func (ls *LogStreamer) stream(ctx context.Context, containerID, containerName string, done chan struct{}) {
	defer close(done)

	var since time.Time // zero → use Tail:100 on first connect
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		lastSeen := ls.doStream(ctx, containerID, containerName, since)
		if !lastSeen.IsZero() {
			since = lastSeen
			// Reset backoff on successful stream that produced data
			backoff = time.Second
		}

		// Context was cancelled — sync() stopped tracking this container.
		if ctx.Err() != nil {
			return
		}

		// Stream ended for another reason (container restart / flap).
		// Wait with exponential backoff so the container has time to come back up.
		log.Printf("logstreamer: stream ended for %s, reconnecting in %v…", containerName, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// doStream opens the Docker log stream for one container and reads until it
// closes or the context is cancelled. Returns the timestamp of the last line
// received so the caller can avoid replaying historical lines on reconnect.
func (ls *LogStreamer) doStream(ctx context.Context, containerID, containerName string, since time.Time) (lastSeen time.Time) {
	reader, err := ls.docker.StreamContainerLogs(ctx, containerID, since)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("logstreamer: failed to open log stream for %s: %v", containerName, err)
		}
		return
	}
	defer reader.Close()

	// Docker multiplexed stream format: [8-byte header][payload]
	// Header: [stream_type(1)][0][0][0][size(4)]
	// stream_type: 1=stdout, 2=stderr
	bufReader := bufio.NewReader(reader)
	header := make([]byte, 8)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, err := io.ReadFull(bufReader, header)
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				log.Printf("logstreamer: read header error for %s: %v", containerName, err)
			}
			return
		}

		streamType := "stdout"
		if header[0] == 2 {
			streamType = "stderr"
		}

		size := binary.BigEndian.Uint32(header[4:8])
		if size == 0 {
			continue
		}

		const maxLogLineSize = 1 * 1024 * 1024 // 1MB
		if size > maxLogLineSize {
			// Skip oversized log line
			if _, err := io.CopyN(io.Discard, bufReader, int64(size)); err != nil {
				return lastSeen
			}
			continue
		}

		payload := make([]byte, size)
		_, err = io.ReadFull(bufReader, payload)
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				log.Printf("logstreamer: read payload error for %s: %v", containerName, err)
			}
			return
		}

		// Each payload line is prefixed with a Docker RFC3339Nano timestamp and
		// a space (because Timestamps=true in StreamContainerLogs). Parse the
		// timestamp so we can use the exact Docker-recorded time as the `since`
		// value on reconnect, eliminating duplicate log replays.
		rawLine := strings.TrimRight(string(payload), "\n")
		if rawLine == "" {
			continue
		}

		// lineTime is the timestamp of this specific line; lastSeen tracks the
		// running maximum for the reconnect `since` filter.
		var lineTime time.Time
		message := rawLine
		if idx := strings.IndexByte(rawLine, ' '); idx > 0 {
			if t, err := time.Parse(time.RFC3339Nano, rawLine[:idx]); err == nil {
				lineTime = t
				lastSeen = t
				message = rawLine[idx+1:]
			}
		}
		// Fallback to wall clock if the timestamp prefix is absent or unparseable.
		if lineTime.IsZero() {
			lineTime = time.Now()
			if lastSeen.IsZero() {
				lastSeen = lineTime
			}
		}

		if message == "" {
			continue
		}
		ls.callback(LogLine{
			ContainerID:   containerID,
			ContainerName: containerName,
			Stream:        streamType,
			Message:       message,
			RecordedAt:    lineTime,
		})
	}
}

func (ls *LogStreamer) stopAll() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for id, entry := range ls.tracked {
		entry.cancel()
		delete(ls.tracked, id)
	}
}
