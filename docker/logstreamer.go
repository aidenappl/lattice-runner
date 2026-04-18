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
}

// LogCallback is called for each log line from a container.
type LogCallback func(line LogLine)

// LogStreamer watches running containers and streams their logs.
type LogStreamer struct {
	docker   *Client
	callback LogCallback
	interval time.Duration

	mu      sync.Mutex
	tracked map[string]context.CancelFunc // containerID -> cancel
}

func NewLogStreamer(docker *Client, callback LogCallback, pollInterval time.Duration) *LogStreamer {
	return &LogStreamer{
		docker:   docker,
		callback: callback,
		interval: pollInterval,
		tracked:  make(map[string]context.CancelFunc),
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

	running := make(map[string]string) // id -> name
	for _, c := range containers {
		if c.State == "running" {
			name := ""
			if len(c.Names) > 0 {
				name = strings.TrimPrefix(c.Names[0], "/")
			}
			running[c.ID] = name
		}
	}

	ls.mu.Lock()
	defer ls.mu.Unlock()

	// Start streaming new containers
	for id, name := range running {
		if _, ok := ls.tracked[id]; !ok {
			streamCtx, cancel := context.WithCancel(ctx)
			ls.tracked[id] = cancel
			go ls.stream(streamCtx, id, name)
		}
	}

	// Stop streaming removed/stopped containers
	for id, cancel := range ls.tracked {
		if _, ok := running[id]; !ok {
			cancel()
			delete(ls.tracked, id)
		}
	}
}

func (ls *LogStreamer) stream(ctx context.Context, containerID, containerName string) {
	reader, err := ls.docker.StreamContainerLogs(ctx, containerID)
	if err != nil {
		log.Printf("logstreamer: failed to stream logs for %s: %v", containerName, err)
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

		payload := make([]byte, size)
		_, err = io.ReadFull(bufReader, payload)
		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				log.Printf("logstreamer: read payload error for %s: %v", containerName, err)
			}
			return
		}

		message := strings.TrimRight(string(payload), "\n")
		if message == "" {
			continue
		}

		ls.callback(LogLine{
			ContainerID:   containerID,
			ContainerName: containerName,
			Stream:        streamType,
			Message:       message,
		})
	}
}

func (ls *LogStreamer) stopAll() {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for id, cancel := range ls.tracked {
		cancel()
		delete(ls.tracked, id)
	}
}
