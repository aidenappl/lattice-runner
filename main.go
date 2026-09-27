package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	monitor "github.com/aidenappl/go-monitor"
	"github.com/aidenappl/lattice-runner/backup"
	"github.com/aidenappl/lattice-runner/client"
	"github.com/aidenappl/lattice-runner/cmd"
	"github.com/aidenappl/lattice-runner/config"
	"github.com/aidenappl/lattice-runner/deploy"
	dockerclient "github.com/aidenappl/lattice-runner/docker"
	"github.com/aidenappl/lattice-runner/metrics"
	"github.com/aidenappl/lattice-runner/scheduler"
	"github.com/aidenappl/lattice-runner/telemetry"
	"github.com/aidenappl/lattice-runner/web"
	"github.com/docker/docker/api/types"
)

// Set via -ldflags at build time: -ldflags "-X main.Version=v1.0.1"
var Version = "dev"

// handlerSem limits the number of concurrent message handler goroutines.
var handlerSem = make(chan struct{}, 50)

// lastRebootTime and lastRebootMu enforce a cooldown between reboot commands
// to prevent repeated reboots from keeping the server permanently offline.
var (
	lastRebootTime time.Time
	lastRebootMu   sync.Mutex
)

// upgradeOutputTailBytes bounds the script/curl output kept with an upgrade
// failure, in the Monitor event and in the stderr copy.
const upgradeOutputTailBytes = 4096

// outputTail returns at most the last n bytes of b.
func outputTail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

// reportUpgradeFailure records an upgrade failure exactly once in Monitor: one
// error event with fixed text and the output tail in a field. The human-readable
// line goes straight to stderr (journald) rather than through log.Printf, because
// the log tee would turn it into a second error event — a second issue — without
// the output.
func reportUpgradeFailure(event, msg string, err error, out []byte) {
	tail := outputTail(out, upgradeOutputTailBytes)
	fmt.Fprintf(os.Stderr, "%s %s: %v (output %d bytes)\n%s\n", time.Now().Format("2006/01/02 15:04:05"), msg, err, len(out), tail)
	telemetry.Event(monitor.LevelError, event, map[string]any{
		"message":      msg,
		"error":        err.Error(),
		"output":       string(tail),
		"output_bytes": len(out),
	})
}

// wsSend sends a JSON message over the WebSocket and logs any failure. The log
// is attributed to wsSend's caller.
func wsSend(ctx context.Context, ws *client.WSClient, msgType string, payload interface{}) {
	wsSendAt(ctx, 1, ws, msgType, payload)
}

// wsSendAt is wsSend for a helper that sends on behalf of its own caller: a
// failure is attributed skip frames above wsSendAt's caller (skip 0 is the
// function that called wsSendAt), as for telemetry.LogAt.
func wsSendAt(ctx context.Context, skip int, ws *client.WSClient, msgType string, payload interface{}) {
	// A queue-full drop is already logged once, at warn, with its type by
	// SendJSON; only other failures (e.g. marshal errors) are logged here.
	if err := ws.SendJSON(payload); err != nil && !errors.Is(err, client.ErrSendQueueFull) {
		telemetry.LogAt(ctx, skip+1, slog.LevelError, "ws send failed", "component", "runner", "message_type", msgType, "error", err)
	}
}

// wsSendReliable sends a command_id-correlated reply that the orchestrator blocks
// on. Unlike wsSend it waits for queue room (up to the timeout) instead of
// dropping immediately when the queue is momentarily full under a telemetry burst.
func wsSendReliable(ctx context.Context, ws *client.WSClient, msgType string, payload interface{}) {
	wsSendReliableAt(ctx, 1, ws, msgType, payload)
}

// wsSendReliableAt is wsSendReliable with a caller skip, as for wsSendAt.
func wsSendReliableAt(ctx context.Context, skip int, ws *client.WSClient, msgType string, payload interface{}) {
	// The timeout drop is already logged once, at warn, with its type by
	// SendJSONReliable; only other failures are logged here.
	if err := ws.SendJSONReliable(payload); err != nil && !errors.Is(err, client.ErrSendQueueFull) {
		telemetry.LogAt(ctx, skip+1, slog.LevelError, "ws reliable send failed", "component", "runner", "message_type", msgType, "error", err)
	}
}

type deploymentRunState struct {
	DeploymentID   int
	StackName      string
	Attempt        int
	MaxRetries     int
	Status         string
	CurrentStep    string
	LastMessage    string
	LastProgressAt time.Time
	StartedAt      time.Time
	InProgress     bool
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		cmd.RunSetup()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(Version)
		return
	}

	fmt.Printf("Lattice Runner %s\n\n", Version)

	// Telemetry first, and from the environment alone: config.Load panics on a
	// missing variable, and that boot failure is exactly what must be reported.
	// It never blocks — Monitor may be a container on this very worker.
	telemetry.Init(Version, config.LoadMonitor())
	defer telemetry.CrashGuard("main")

	// Load configuration
	cfg := config.Load()
	fmt.Printf("  Worker:       %s\n", cfg.WorkerName)
	fmt.Printf("  Orchestrator: %s\n", cfg.OrchestratorURL)
	fmt.Printf("  Heartbeat:    %v\n", cfg.HeartbeatInterval)
	fmt.Println()

	// Initialize Docker client with retry
	fmt.Print("Connecting to Docker...")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var docker *dockerclient.Client
	for i := 0; i < 30; i++ {
		var err error
		docker, err = dockerclient.NewClient()
		if err == nil {
			if pingErr := docker.Ping(ctx); pingErr == nil {
				break
			} else {
				docker.Close()
				docker = nil
				slog.WarnContext(ctx, "docker connect attempt failed", "component", "runner", "attempt", i+1, "error", pingErr)
			}
		} else {
			slog.WarnContext(ctx, "docker connect attempt failed", "component", "runner", "attempt", i+1, "error", err)
		}
		time.Sleep(2 * time.Second)
	}
	if docker == nil {
		telemetry.Fatal("service.startup.docker_unreachable", "failed to connect to Docker after 30 attempts", nil)
	}
	defer docker.Close()

	dockerVersion, _ := docker.ServerVersion(ctx)
	fmt.Printf(" ✅ Done (Docker %s)\n", dockerVersion)

	// Create WebSocket client
	ws := client.NewWSClient(cfg.OrchestratorURL, cfg.WorkerToken, cfg.ReconnectInterval)

	deploymentStates := make(map[int]*deploymentRunState)
	var deploymentStatesMu sync.RWMutex

	// Create deploy executor
	executor := deploy.NewExecutor(docker, func(deploymentID int, status, message string, payload map[string]any) {
		deploymentStatesMu.Lock()
		st, ok := deploymentStates[deploymentID]
		if !ok {
			st = &deploymentRunState{
				DeploymentID: deploymentID,
				Attempt:      1,
				MaxRetries:   3,
				StartedAt:    time.Now().UTC(),
				InProgress:   true,
			}
			deploymentStates[deploymentID] = st
		}
		st.Status = status
		if step, ok := payload["step"].(string); ok && step != "" {
			st.CurrentStep = step
		}
		st.LastMessage = message
		st.LastProgressAt = time.Now().UTC()
		if status == "deployed" || status == "failed" || status == "rolled_back" {
			st.InProgress = false
		}
		attempt := st.Attempt
		maxRetries := st.MaxRetries
		deploymentStatesMu.Unlock()

		out := make(map[string]any, len(payload)+3)
		for k, v := range payload {
			out[k] = v
		}
		out["attempt"] = attempt
		out["max_retries"] = maxRetries
		out["last_progress_at"] = time.Now().UTC().Format(time.RFC3339)

		wsSend(ctx, ws, "deployment_progress", client.OutgoingMessage{
			Type:    "deployment_progress",
			Payload: out,
		})
	})

	// Active exec sessions: command_id -> cancel func
	type execSession struct {
		execID    string
		conn      types.HijackedResponse
		cancel    context.CancelFunc
		createdAt time.Time
	}
	execSessions := make(map[string]*execSession)
	var execMu sync.Mutex

	// Periodic exec session cleanup — remove orphaned sessions older than 30 minutes
	go func() {
		defer telemetry.Recover("exec-session-cleanup", nil)
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				execMu.Lock()
				for id, s := range execSessions {
					if time.Since(s.createdAt) > 30*time.Minute {
						slog.WarnContext(ctx, "removing orphaned exec session", "component", "runner", "command_id", id, "duration_ms", time.Since(s.createdAt).Milliseconds())
						s.cancel()
						delete(execSessions, id)
					}
				}
				execMu.Unlock()
			}
		}
	}()

	// Create snapshot scheduler
	snapshotScheduler := scheduler.New(func(job scheduler.Job) {
		handleScheduledSnapshot(ctx, ws, docker, job)
	})
	safeGoResilient("snapshot-scheduler", func() { snapshotScheduler.Run(ctx) })

	// Handle incoming messages from orchestrator
	ws.OnMessage(func(env client.Envelope) {
		// Recover from any panic in a message handler so the WS read-pump stays
		// alive rather than crashing the whole process.
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 8192)
				n := runtime.Stack(buf, false)
				log.Printf("[message-handler] PANIC for event %q: %v\n%s", env.Type, r, string(buf[:n]))
				telemetry.ReportPanic("message-handler", r, map[string]any{"event": env.Type, "command_id": env.CommandID})
			}
		}()
		switch env.Type {
		case "connected":
			slog.InfoContext(ctx, "connected to orchestrator", "component", "runner")
			// Send registration info
			wsSend(ctx, ws, "registration", client.OutgoingMessage{
				Type: "registration",
				Payload: map[string]any{
					"name":           cfg.WorkerName,
					"hostname":       hostname(),
					"os":             runtime.GOOS,
					"arch":           runtime.GOARCH,
					"docker_version": dockerVersion,
					"ip_address":     localIP(),
					"runner_version": Version,
				},
			})

		case "deploy":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:deploy", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				spec, err := deploy.ParseDeploymentSpec(env.Payload)
				if err != nil {
					slog.WarnContext(ctx, "invalid deploy spec", "component", "runner", "command_id", env.CommandID, "error", err)
					wsSend(ctx, ws, "deployment_progress", client.OutgoingMessage{
						Type:      "deployment_progress",
						CommandID: env.CommandID,
						Status:    "failed",
						Payload: map[string]any{
							"deployment_id": env.Payload["deployment_id"],
							"status":        "failed",
							"message":       fmt.Sprintf("invalid spec: %v", err),
						},
					})
					return
				}

				attempt := 1
				if v, ok := env.Payload["attempt"].(float64); ok && int(v) > 0 {
					attempt = int(v)
				}
				maxRetries := 3
				if v, ok := env.Payload["max_retries"].(float64); ok && int(v) > 0 {
					maxRetries = int(v)
				}

				// Atomically check-and-set the in-flight guard under a SINGLE lock to
				// avoid a TOCTOU race between the check and the set. In-flight means any
				// non-terminal state — deploying OR the 60s validating window — tracked
				// by InProgress (not just Status=="deploying"). Serialization is by STACK,
				// not just deployment ID: a duplicate of this deployment is rejected, and
				// so is a *different* deployment that targets a stack already deploying.
				deploymentStatesMu.Lock()
				if existing, ok := deploymentStates[spec.DeploymentID]; ok && existing.InProgress {
					deploymentStatesMu.Unlock()
					slog.WarnContext(ctx, "deployment already in progress, ignoring duplicate", "component", "deploy", "deployment_id", spec.DeploymentID, "status", existing.Status)
					return
				}
				if spec.StackName != "" {
					conflict := 0
					for _, st := range deploymentStates {
						if st.InProgress && st.DeploymentID != spec.DeploymentID && st.StackName == spec.StackName {
							conflict = st.DeploymentID
							break
						}
					}
					if conflict != 0 {
						deploymentStatesMu.Unlock()
						slog.WarnContext(ctx, "stack already has an in-flight deployment, rejecting deployment", "component", "deploy", "stack", spec.StackName, "deployment_id", spec.DeploymentID, "conflicting_deployment_id", conflict)
						wsSend(ctx, ws, "deployment_progress", client.OutgoingMessage{
							Type:      "deployment_progress",
							CommandID: env.CommandID,
							Status:    "failed",
							Payload: map[string]any{
								"deployment_id": spec.DeploymentID,
								"status":        "failed",
								"message":       fmt.Sprintf("stack %s already has an in-flight deployment (%d)", spec.StackName, conflict),
							},
						})
						return
					}
				}
				deploymentStates[spec.DeploymentID] = &deploymentRunState{
					DeploymentID:   spec.DeploymentID,
					StackName:      spec.StackName,
					Attempt:        attempt,
					MaxRetries:     maxRetries,
					Status:         "deploying",
					CurrentStep:    "starting",
					LastMessage:    fmt.Sprintf("deploy attempt %d/%d started", attempt, maxRetries),
					LastProgressAt: time.Now().UTC(),
					StartedAt:      time.Now().UTC(),
					InProgress:     true,
				}
				deploymentStatesMu.Unlock()

				wsSend(ctx, ws, "deployment_progress", client.OutgoingMessage{
					Type: "deployment_progress",
					Payload: map[string]any{
						"deployment_id": spec.DeploymentID,
						"status":        "deploying",
						"message":       fmt.Sprintf("deploy attempt %d/%d started", attempt, maxRetries),
						"step":          "attempt_start",
						"attempt":       attempt,
						"max_retries":   maxRetries,
					},
				})

				if err := executor.Execute(ctx, *spec); err != nil {
					slog.ErrorContext(ctx, "deployment failed", "component", "runner", "deployment_id", spec.DeploymentID, "stack", spec.StackName, "error", err)
					deploymentStatesMu.Lock()
					if st, ok := deploymentStates[spec.DeploymentID]; ok {
						st.Status = "failed"
						st.InProgress = false
						st.LastMessage = err.Error()
						st.LastProgressAt = time.Now().UTC()
					}
					deploymentStatesMu.Unlock()
				} else {
					deploymentStatesMu.Lock()
					if st, ok := deploymentStates[spec.DeploymentID]; ok {
						st.Status = "deployed"
						st.InProgress = false
						st.LastProgressAt = time.Now().UTC()
					}
					deploymentStatesMu.Unlock()
				}
			}()

		case "deployment_ping":
			go func() {
				defer telemetry.Recover("handler:deployment_ping", map[string]any{"command_id": env.CommandID})
				depIDFloat, _ := env.Payload["deployment_id"].(float64)
				depID := int(depIDFloat)

				deploymentStatesMu.RLock()
				st, ok := deploymentStates[depID]
				deploymentStatesMu.RUnlock()

				if !ok {
					wsSend(ctx, ws, "deployment_status", client.OutgoingMessage{
						Type: "deployment_status",
						Payload: map[string]any{
							"deployment_id": depID,
							"status":        "idle",
							"in_progress":   false,
							"message":       "no active deployment state for this id",
						},
					})
					return
				}

				wsSend(ctx, ws, "deployment_status", client.OutgoingMessage{
					Type: "deployment_status",
					Payload: map[string]any{
						"deployment_id":    st.DeploymentID,
						"status":           st.Status,
						"in_progress":      st.InProgress,
						"step":             st.CurrentStep,
						"message":          st.LastMessage,
						"attempt":          st.Attempt,
						"max_retries":      st.MaxRetries,
						"last_progress_at": st.LastProgressAt.Format(time.RFC3339),
					},
				})
			}()

		case "stop":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:stop", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					wsSend(ctx, ws, "container_status", client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "stop", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid container name rejected", "component", "stop", "container", containerName)
					wsSend(ctx, ws, "container_status", client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "stop", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "stop", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					slog.WarnContext(ctx, "container not found", "component", "runner", "container", containerName, "action", "stop")
					sendLifecycleLog(ctx, ws, containerName, "stop", "container not found")
					wsSend(ctx, ws, "container_status", client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "stop",
							"status":         "failed",
							"message":        "container not found",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "stop", fmt.Sprintf("stopping container (timeout=30s, id=%s)…", id[:12]))
				if err := docker.StopContainer(ctx, id, 30); err != nil {
					slog.ErrorContext(ctx, "failed to stop container", "component", "runner", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "stop", fmt.Sprintf("failed to stop: %v", err))
					wsSend(ctx, ws, "container_status", client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "stop",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					slog.InfoContext(ctx, "stopped container", "component", "runner", "container", containerName)
					wsSend(ctx, ws, "container_status", client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "stop",
							"status":         "success",
						},
					})
				}
			}()

		case "start":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:start", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					wsSend(ctx, ws, "container_status", client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "start", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid container name rejected", "component", "start", "container", containerName)
					wsSend(ctx, ws, "container_status", client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "start", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "start", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					slog.WarnContext(ctx, "container not found", "component", "runner", "container", containerName, "action", "start")
					sendLifecycleLog(ctx, ws, containerName, "start", "container not found")
					wsSend(ctx, ws, "container_status", client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "start",
							"status":         "failed",
							"message":        "container not found",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "start", fmt.Sprintf("starting container (id=%s)…", id[:12]))
				if err := docker.StartContainer(ctx, id); err != nil {
					slog.ErrorContext(ctx, "failed to start container", "component", "runner", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "start", fmt.Sprintf("failed to start: %v", err))
					wsSend(ctx, ws, "container_status", client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "start",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					slog.InfoContext(ctx, "started container", "component", "runner", "container", containerName)
					wsSend(ctx, ws, "container_status", client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "start",
							"status":         "success",
						},
					})
				}
			}()

		case "kill":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:kill", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "kill", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid container name rejected", "component", "kill", "container", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "kill", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "kill", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					slog.WarnContext(ctx, "container not found", "component", "runner", "container", containerName, "action", "kill")
					sendLifecycleLog(ctx, ws, containerName, "kill", "container not found")
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "kill",
							"status":         "failed",
							"message":        "container not found",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "kill", fmt.Sprintf("sending SIGKILL to container (id=%s)…", id[:12]))
				if err := docker.KillContainer(ctx, id); err != nil {
					slog.ErrorContext(ctx, "failed to kill container", "component", "runner", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "kill", fmt.Sprintf("failed to kill: %v", err))
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "kill",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					slog.InfoContext(ctx, "killed container", "component", "runner", "container", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "kill",
							"status":         "success",
						},
					})
				}
			}()

		case "pause":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:pause", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "pause", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid container name rejected", "component", "pause", "container", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "pause", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "pause", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					slog.WarnContext(ctx, "container not found", "component", "runner", "container", containerName, "action", "pause")
					sendLifecycleLog(ctx, ws, containerName, "pause", "container not found")
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "pause",
							"status":         "failed",
							"message":        "container not found",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "pause", fmt.Sprintf("pausing container (id=%s)…", id[:12]))
				if err := docker.PauseContainer(ctx, id); err != nil {
					slog.ErrorContext(ctx, "failed to pause container", "component", "runner", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "pause", fmt.Sprintf("failed to pause: %v", err))
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "pause",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					slog.InfoContext(ctx, "paused container", "component", "runner", "container", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "pause",
							"status":         "success",
						},
					})
				}
			}()

		case "unpause":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:unpause", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "unpause", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid container name rejected", "component", "unpause", "container", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "unpause", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "unpause", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					slog.WarnContext(ctx, "container not found", "component", "runner", "container", containerName, "action", "unpause")
					sendLifecycleLog(ctx, ws, containerName, "unpause", "container not found")
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "unpause",
							"status":         "failed",
							"message":        "container not found",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "unpause", fmt.Sprintf("resuming container (id=%s)…", id[:12]))
				if err := docker.UnpauseContainer(ctx, id); err != nil {
					slog.ErrorContext(ctx, "failed to unpause container", "component", "runner", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "unpause", fmt.Sprintf("failed to resume: %v", err))
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "unpause",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					slog.InfoContext(ctx, "unpaused container", "component", "runner", "container", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "unpause",
							"status":         "success",
						},
					})
				}
			}()

		case "restart":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:restart", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "restart", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid container name rejected", "component", "restart", "container", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "restart", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "restart", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					slog.WarnContext(ctx, "container not found", "component", "runner", "container", containerName, "action", "restart")
					sendLifecycleLog(ctx, ws, containerName, "restart", "container not found")
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "restart",
							"status":         "failed",
							"message":        "container not found",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "restart", fmt.Sprintf("restarting container (timeout=30s, id=%s)… container will stop then start", id[:12]))
				if err := docker.RestartContainer(ctx, id, 30); err != nil {
					slog.ErrorContext(ctx, "failed to restart container", "component", "runner", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "restart", fmt.Sprintf("failed to restart: %v", err))
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "restart",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					slog.InfoContext(ctx, "restarted container", "component", "runner", "container", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "restart",
							"status":         "success",
						},
					})
				}
			}()

		case "remove":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:remove", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "remove", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid container name rejected", "component", "remove", "container", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "remove", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "remove", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					sendLifecycleWarn(ctx, ws, containerName, "remove", "container not found")
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "remove",
							"status":         "failed",
							"message":        "container not found",
						},
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "remove", fmt.Sprintf("stopping container before removal (timeout=10s, id=%s)…", id[:12]))
				if err := docker.StopContainer(ctx, id, 10); err != nil {
					sendLifecycleWarn(ctx, ws, containerName, "remove", fmt.Sprintf("stop returned: %v (proceeding with force remove)", err))
				} else {
					sendLifecycleLog(ctx, ws, containerName, "remove", "container stopped, removing…")
				}
				if err := docker.RemoveContainer(ctx, id, true); err != nil {
					slog.ErrorContext(ctx, "failed to remove container", "component", "runner", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "remove", fmt.Sprintf("failed to remove: %v", err))
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "remove",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					slog.InfoContext(ctx, "removed container", "component", "runner", "container", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "remove",
							"status":         "success",
						},
					})
				}
			}()

		case "recreate":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:recreate", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "recreate", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid container name rejected", "component", "recreate", "container", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "recreate", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}

				// Pull the latest image before recreating.
				imageRef, _ := env.Payload["image"].(string)
				tag, _ := env.Payload["tag"].(string)
				if imageRef != "" {
					fullRef := imageRef
					if tag != "" {
						fullRef = imageRef + ":" + tag
					}
					var regAuth *dockerclient.RegistryAuth
					if authData, ok := env.Payload["auth"]; ok {
						b, _ := json.Marshal(authData)
						regAuth = &dockerclient.RegistryAuth{}
						_ = json.Unmarshal(b, regAuth)
					}
					authInfo := ""
					if regAuth != nil && regAuth.Username != "" {
						authInfo = fmt.Sprintf(" (registry auth: %s)", regAuth.Username)
					}
					sendLifecycleLog(ctx, ws, containerName, "recreate", fmt.Sprintf("pulling image %s%s…", fullRef, authInfo))
					if err := docker.PullImage(ctx, fullRef, regAuth); err != nil {
						slog.WarnContext(ctx, "image pull failed, proceeding with recreate anyway", "component", "runner", "container", containerName, "image", fullRef, "error", err)
						sendLifecycleLog(ctx, ws, containerName, "recreate", fmt.Sprintf("image pull failed: %v — proceeding with local image", err))
					} else {
						sendLifecycleLog(ctx, ws, containerName, "recreate", fmt.Sprintf("image %s pulled successfully", fullRef))
					}
				} else {
					sendLifecycleLog(ctx, ws, containerName, "recreate", "no image specified, recreating with current image")
				}

				sendLifecycleLog(ctx, ws, containerName, "recreate", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					// Try canonical variants (suffixed names from deploys)
					id, _ = executor.FindCanonicalContainer(ctx, containerName)
				}
				if id == "" {
					slog.WarnContext(ctx, "container not found", "component", "runner", "container", containerName, "action", "recreate")
					sendLifecycleLog(ctx, ws, containerName, "recreate", "container not found — run a stack deploy to create it")
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "recreate",
							"status":         "failed",
							"message":        "container not found — deploy the stack to create it",
						},
					})
					return
				}

				// Build the full image reference for graceful recreate
				newImageRef := ""
				if imageRef != "" {
					newImageRef = imageRef
					if tag != "" {
						newImageRef = imageRef + ":" + tag
					}
				}

				sendLifecycleLog(ctx, ws, containerName, "recreate", fmt.Sprintf("graceful recreate (old id=%s)… starting new container, health checking, then swapping", id[:12]))
				newID, err := docker.GracefulRecreate(ctx, id, newImageRef)
				if err != nil {
					slog.ErrorContext(ctx, "failed to recreate container", "component", "runner", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "recreate", fmt.Sprintf("failed to recreate: %v", err))
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "recreate",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
					return
				}
				slog.InfoContext(ctx, "recreated container", "component", "runner", "container", containerName, "container_id", newID)
				_ = ws.SendJSON(client.OutgoingMessage{
					Type: "container_status",
					Payload: map[string]any{
						"container_name": containerName,
						"action":         "recreate",
						"status":         "success",
					},
				})
			}()

		case "pull_image":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:pull_image", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				imageRef, _ := env.Payload["image"].(string)
				if imageRef == "" {
					return
				}
				var regAuth *dockerclient.RegistryAuth
				if authData, ok := env.Payload["auth"]; ok {
					b, _ := json.Marshal(authData)
					regAuth = &dockerclient.RegistryAuth{}
					_ = json.Unmarshal(b, regAuth)
				}
				authInfo := ""
				if regAuth != nil && regAuth.Username != "" {
					authInfo = fmt.Sprintf(" (registry auth: %s)", regAuth.Username)
				}
				sendLifecycleLog(ctx, ws, imageRef, "pull_image", fmt.Sprintf("pulling image %s%s…", imageRef, authInfo))
				if err := docker.PullImage(ctx, imageRef, regAuth); err != nil {
					slog.ErrorContext(ctx, "failed to pull image", "component", "runner", "image", imageRef, "error", err)
					sendLifecycleLog(ctx, ws, imageRef, "pull_image", fmt.Sprintf("pull failed: %v", err))
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": imageRef,
							"action":         "pull_image",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					slog.InfoContext(ctx, "pulled image", "component", "runner", "image", imageRef)
					sendLifecycleLog(ctx, ws, imageRef, "pull_image", fmt.Sprintf("image %s pulled successfully", imageRef))
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": imageRef,
							"action":         "pull_image",
							"status":         "success",
						},
					})
				}
			}()

		case "reboot_os":
			go func() {
				defer telemetry.Recover("handler:reboot_os", map[string]any{"command_id": env.CommandID})
				// Rate-limit reboots: reject if last reboot was within 5 minutes
				lastRebootMu.Lock()
				if time.Since(lastRebootTime) < 5*time.Minute {
					lastRebootMu.Unlock()
					slog.WarnContext(ctx, "reboot rejected: cooldown period (5 minutes between reboots)", "component", "runner")
					wsSend(ctx, ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "reboot_os",
							"status":  "failed",
							"message": "reboot rejected: minimum 5 minutes between reboot commands",
						},
					})
					return
				}
				lastRebootTime = time.Now()
				lastRebootMu.Unlock()

				slog.InfoContext(ctx, "reboot command received, rebooting system", "component", "runner")
				wsSend(ctx, ws, "worker_action_status", client.OutgoingMessage{
					Type: "worker_action_status",
					Payload: map[string]any{
						"action":  "reboot_os",
						"status":  "accepted",
						"message": "system will reboot momentarily",
					},
				})
				time.Sleep(1 * time.Second)
				out, err := exec.Command("sudo", "reboot").CombinedOutput()
				if err != nil {
					slog.ErrorContext(ctx, "reboot failed", "component", "runner", "error", err, "output", string(out))
				}
			}()

		case "upgrade_runner":
			go func() {
				defer telemetry.Recover("handler:upgrade_runner", map[string]any{"command_id": env.CommandID})
				slog.InfoContext(ctx, "upgrade runner command received", "component", "runner")
				wsSend(ctx, ws, "worker_action_status", client.OutgoingMessage{
					Type: "worker_action_status",
					Payload: map[string]any{
						"action":  "upgrade_runner",
						"status":  "accepted",
						"message": "starting runner upgrade",
					},
				})

				// Extract expected hash from the orchestrator payload for integrity verification
				expectedHash, _ := env.Payload["expected_hash"].(string)

				// Fail closed: without an expected hash we cannot verify the script's
				// integrity, so we refuse to download or execute it. Running an unverified
				// script here is a fleet-wide remote-code-execution risk.
				if expectedHash == "" {
					slog.ErrorContext(ctx, "upgrade aborted: no expected_hash provided by orchestrator, refusing to run unverified script", "component", "runner")
					wsSend(ctx, ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "upgrade_runner",
							"status":  "failed",
							"message": "upgrade aborted: no expected_hash provided; refusing to run an unverified upgrade script",
						},
					})
					return
				}

				// Derive upgrade URL from the orchestrator connection URL
				upgradeBase := cfg.OrchestratorURL
				upgradeBase = strings.Replace(upgradeBase, "ws://", "http://", 1)
				upgradeBase = strings.Replace(upgradeBase, "wss://", "https://", 1)
				upgradeBase = strings.TrimSuffix(upgradeBase, "/ws/worker")
				upgradeBase = strings.TrimSuffix(upgradeBase, "/ws")
				upgradeURL := fmt.Sprintf("%s/install/runner?t=%d", upgradeBase, time.Now().Unix())

				// Use a secure temp directory with unpredictable name
				tmpDir, mkErr := os.MkdirTemp("", "lattice-upgrade-*")
				if mkErr != nil {
					slog.ErrorContext(ctx, "failed to create temp dir", "component", "upgrade", "error", mkErr)
					wsSend(ctx, ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "upgrade_runner",
							"status":  "failed",
							"message": fmt.Sprintf("failed to create temp dir: %v", mkErr),
						},
					})
					return
				}
				defer os.RemoveAll(tmpDir)
				tmpFile := filepath.Join(tmpDir, "upgrade.sh")

				// Download to temp file
				dlCmd := exec.CommandContext(ctx, "curl", "-fsSL", "-o", tmpFile, upgradeURL)
				if dlOut, dlErr := dlCmd.CombinedOutput(); dlErr != nil {
					reportUpgradeFailure("runner.upgrade.download_failed", "upgrade download failed", dlErr, dlOut)
					// Include curl's output (e.g. "curl: (22) ... 404") so the dashboard shows why
					dlMsg := fmt.Sprintf("upgrade download failed: %v", dlErr)
					if dlOutput := string(outputTail(dlOut, 1000)); dlOutput != "" {
						dlMsg = fmt.Sprintf("upgrade download failed: %v\n%s", dlErr, dlOutput)
					}
					wsSend(ctx, ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "upgrade_runner",
							"status":  "failed",
							"message": dlMsg,
						},
					})
					return
				}

				// Verify SHA256 hash of the downloaded script
				scriptBytes, readErr := os.ReadFile(tmpFile)
				if readErr != nil {
					slog.ErrorContext(ctx, "failed to read downloaded script", "component", "upgrade", "error", readErr)
					wsSend(ctx, ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "upgrade_runner",
							"status":  "failed",
							"message": "failed to read downloaded upgrade script",
						},
					})
					return
				}

				actualHash := sha256.Sum256(scriptBytes)
				actualHashHex := hex.EncodeToString(actualHash[:])
				slog.InfoContext(ctx, "upgrade script hash", "component", "runner", "hash", actualHashHex)

				if actualHashHex != expectedHash {
					slog.ErrorContext(ctx, "upgrade aborted: script hash mismatch", "component", "runner", "expected_hash", expectedHash, "hash", actualHashHex)
					wsSend(ctx, ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "upgrade_runner",
							"status":  "failed",
							"message": "upgrade aborted: script integrity check failed (hash mismatch)",
						},
					})
					return
				}

				// Make executable and run
				_ = os.Chmod(tmpFile, 0755)
				out, err := exec.CommandContext(ctx, "bash", tmpFile).CombinedOutput()
				if err != nil {
					reportUpgradeFailure("runner.upgrade.failed", "upgrade failed", err, out)
					// Include truncated script output so the dashboard shows the real error
					scriptOutput := string(out)
					if len(scriptOutput) > 1000 {
						scriptOutput = scriptOutput[len(scriptOutput)-1000:]
					}
					failMsg := fmt.Sprintf("upgrade failed: %v", err)
					if scriptOutput != "" {
						failMsg = fmt.Sprintf("upgrade failed: %v\n%s", err, scriptOutput)
					}
					wsSend(ctx, ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "upgrade_runner",
							"status":  "failed",
							"message": failMsg,
						},
					})
				} else {
					// Fixed text: the script output contains words like "errors" (e.g.
					// github.com/pkg/errors) that the log classifier would read as a failure.
					slog.InfoContext(ctx, "upgrade completed, runner will restart via systemd", "component", "runner", "output_bytes", len(out))
					wsSend(ctx, ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "upgrade_runner",
							"status":  "success",
							"message": "upgrade completed, runner will restart via systemd",
						},
					})
				}
			}()

		case "stop_all":
			go func() {
				defer telemetry.Recover("handler:stop_all", map[string]any{"command_id": env.CommandID})
				slog.InfoContext(ctx, "stop all containers command received", "component", "runner")
				containers, err := docker.ListContainers(ctx, "")
				if err != nil {
					slog.ErrorContext(ctx, "failed to list containers", "component", "runner", "action", "stop_all", "error", err)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "stop_all",
							"status":  "failed",
							"message": fmt.Sprintf("failed to list containers: %v", err),
						},
					})
					return
				}
				running := 0
				for _, c := range containers {
					if c.State == "running" {
						running++
					}
				}
				slog.InfoContext(ctx, "found running containers", "component", "stop_all", "count", running, "total", len(containers))
				stopped := 0
				failed := 0
				for _, c := range containers {
					if c.State == "running" {
						name := ""
						for _, n := range c.Names {
							trimmed := strings.TrimPrefix(n, "/")
							if trimmed != "" {
								name = trimmed
								break
							}
						}
						if name != "" {
							sendLifecycleLog(ctx, ws, name, "stop", fmt.Sprintf("stopping container as part of stop_all (%d/%d)…", stopped+failed+1, running))
						}
						if err := docker.StopContainer(ctx, c.ID, 30); err != nil {
							slog.ErrorContext(ctx, "failed to stop container", "component", "runner", "container", name, "container_id", c.ID[:12], "error", err)
							failed++
						} else {
							stopped++
						}
					}
				}
				// No "failed" in the summary: each failure is already logged at error
				// above, so the summary is info when all succeeded and warn otherwise
				// ("could not be stopped" is a caution token in telemetry.classify).
				if failed == 0 {
					slog.InfoContext(ctx, "stop_all complete, all succeeded", "component", "runner", "stopped", stopped)
				} else {
					slog.WarnContext(ctx, "stop_all complete, some containers could not be stopped", "component", "runner", "stopped", stopped, "failed", failed)
				}
				_ = ws.SendJSON(client.OutgoingMessage{
					Type: "worker_action_status",
					Payload: map[string]any{
						"action":  "stop_all",
						"status":  "success",
						"message": fmt.Sprintf("stopped %d containers, %d failed", stopped, failed),
					},
				})
			}()

		case "start_all":
			go func() {
				defer telemetry.Recover("handler:start_all", map[string]any{"command_id": env.CommandID})
				slog.InfoContext(ctx, "start all containers command received", "component", "runner")
				containers, err := docker.ListContainers(ctx, "")
				if err != nil {
					slog.ErrorContext(ctx, "failed to list containers", "component", "runner", "action", "start_all", "error", err)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "start_all",
							"status":  "failed",
							"message": fmt.Sprintf("failed to list containers: %v", err),
						},
					})
					return
				}
				notRunning := 0
				for _, c := range containers {
					if c.State != "running" {
						notRunning++
					}
				}
				slog.InfoContext(ctx, "found stopped containers", "component", "start_all", "count", notRunning, "total", len(containers))
				started := 0
				failed := 0
				for _, c := range containers {
					if c.State != "running" {
						name := ""
						for _, n := range c.Names {
							trimmed := strings.TrimPrefix(n, "/")
							if trimmed != "" {
								name = trimmed
								break
							}
						}
						if name != "" {
							sendLifecycleLog(ctx, ws, name, "start", fmt.Sprintf("starting container as part of start_all (%d/%d)…", started+failed+1, notRunning))
						}
						if err := docker.StartContainer(ctx, c.ID); err != nil {
							slog.ErrorContext(ctx, "failed to start container", "component", "runner", "container", name, "container_id", c.ID[:12], "error", err)
							failed++
						} else {
							started++
						}
					}
				}
				// See stop_all: info when all succeeded, warn otherwise.
				if failed == 0 {
					slog.InfoContext(ctx, "start_all complete, all succeeded", "component", "runner", "started", started)
				} else {
					slog.WarnContext(ctx, "start_all complete, some containers could not be started", "component", "runner", "started", started, "failed", failed)
				}
				_ = ws.SendJSON(client.OutgoingMessage{
					Type: "worker_action_status",
					Payload: map[string]any{
						"action":  "start_all",
						"status":  "success",
						"message": fmt.Sprintf("started %d containers, %d failed", started, failed),
					},
				})
			}()

		case "list_volumes":
			go func() {
				defer telemetry.Recover("handler:list_volumes", map[string]any{"command_id": env.CommandID})
				volumes, err := docker.ListVolumes(ctx)
				if err != nil {
					slog.ErrorContext(ctx, "failed to list volumes", "component", "runner", "error", err)
					_ = ws.SendJSONReliable(client.OutgoingMessage{
						Type: "list_volumes_response",
						Payload: map[string]any{
							"command_id": env.CommandID,
							"status":     "error",
							"error":      err.Error(),
						},
					})
					return
				}
				volumeList := make([]map[string]any, 0, len(volumes))
				for _, v := range volumes {
					volumeList = append(volumeList, map[string]any{
						"name":       v.Name,
						"driver":     v.Driver,
						"mountpoint": v.Mountpoint,
						"created_at": v.CreatedAt,
						"scope":      v.Scope,
						"labels":     v.Labels,
					})
				}
				_ = ws.SendJSONReliable(client.OutgoingMessage{
					Type: "list_volumes_response",
					Payload: map[string]any{
						"command_id": env.CommandID,
						"status":     "success",
						"volumes":    volumeList,
					},
				})
			}()

		case "create_volume":
			go func() {
				defer telemetry.Recover("handler:create_volume", map[string]any{"command_id": env.CommandID})
				name, _ := env.Payload["name"].(string)
				driver, _ := env.Payload["driver"].(string)
				if name == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"command_id": env.CommandID,
							"action":     "create_volume",
							"status":     "failed",
							"message":    "volume name is required",
						},
					})
					return
				}
				if driver == "" {
					driver = "local"
				}
				if err := docker.CreateVolume(ctx, name, driver); err != nil {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"command_id": env.CommandID,
							"action":     "create_volume",
							"status":     "failed",
							"message":    fmt.Sprintf("failed to create volume: %v", err),
						},
					})
					return
				}
				_ = ws.SendJSON(client.OutgoingMessage{
					Type: "worker_action_status",
					Payload: map[string]any{
						"command_id": env.CommandID,
						"action":     "create_volume",
						"status":     "success",
						"message":    fmt.Sprintf("volume %s created", name),
					},
				})
			}()

		case "remove_volume":
			go func() {
				defer telemetry.Recover("handler:remove_volume", map[string]any{"command_id": env.CommandID})
				name, _ := env.Payload["name"].(string)
				if name == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"command_id": env.CommandID,
							"action":     "remove_volume",
							"status":     "failed",
							"message":    "volume name is required",
						},
					})
					return
				}
				force, _ := env.Payload["force"].(bool)
				if err := docker.RemoveVolume(ctx, name, force); err != nil {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"command_id": env.CommandID,
							"action":     "remove_volume",
							"status":     "failed",
							"message":    fmt.Sprintf("failed to remove volume: %v", err),
						},
					})
					return
				}
				_ = ws.SendJSON(client.OutgoingMessage{
					Type: "worker_action_status",
					Payload: map[string]any{
						"command_id": env.CommandID,
						"action":     "remove_volume",
						"status":     "success",
						"message":    fmt.Sprintf("volume %s removed", name),
					},
				})
			}()

		case "list_networks":
			go func() {
				defer telemetry.Recover("handler:list_networks", map[string]any{"command_id": env.CommandID})
				networks, err := docker.ListNetworks(ctx)
				if err != nil {
					slog.ErrorContext(ctx, "failed to list networks", "component", "runner", "error", err)
					_ = ws.SendJSONReliable(client.OutgoingMessage{
						Type: "list_networks_response",
						Payload: map[string]any{
							"command_id": env.CommandID,
							"status":     "error",
							"error":      err.Error(),
						},
					})
					return
				}
				networkList := make([]map[string]any, 0, len(networks))
				for _, n := range networks {
					containers := make(map[string]string, len(n.Containers))
					for id, ep := range n.Containers {
						containers[id] = ep.Name
					}
					networkList = append(networkList, map[string]any{
						"id":         n.ID,
						"name":       n.Name,
						"driver":     n.Driver,
						"scope":      n.Scope,
						"internal":   n.Internal,
						"containers": containers,
						"created":    n.Created,
					})
				}
				_ = ws.SendJSONReliable(client.OutgoingMessage{
					Type: "list_networks_response",
					Payload: map[string]any{
						"command_id": env.CommandID,
						"status":     "success",
						"networks":   networkList,
					},
				})
			}()

		case "create_network":
			go func() {
				defer telemetry.Recover("handler:create_network", map[string]any{"command_id": env.CommandID})
				name, _ := env.Payload["name"].(string)
				driver, _ := env.Payload["driver"].(string)
				if name == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"command_id": env.CommandID,
							"action":     "create_network",
							"status":     "failed",
							"message":    "network name is required",
						},
					})
					return
				}
				if driver == "" {
					driver = "bridge"
				}
				if err := docker.CreateNetwork(ctx, name, driver); err != nil {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"command_id": env.CommandID,
							"action":     "create_network",
							"status":     "failed",
							"message":    fmt.Sprintf("failed to create network: %v", err),
						},
					})
					return
				}
				_ = ws.SendJSON(client.OutgoingMessage{
					Type: "worker_action_status",
					Payload: map[string]any{
						"command_id": env.CommandID,
						"action":     "create_network",
						"status":     "success",
						"message":    fmt.Sprintf("network %s created", name),
					},
				})
			}()

		case "remove_network":
			go func() {
				defer telemetry.Recover("handler:remove_network", map[string]any{"command_id": env.CommandID})
				name, _ := env.Payload["name"].(string)
				if name == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"command_id": env.CommandID,
							"action":     "remove_network",
							"status":     "failed",
							"message":    "network name is required",
						},
					})
					return
				}
				if err := docker.RemoveNetwork(ctx, name); err != nil {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"command_id": env.CommandID,
							"action":     "remove_network",
							"status":     "failed",
							"message":    fmt.Sprintf("failed to remove network: %v", err),
						},
					})
					return
				}
				_ = ws.SendJSON(client.OutgoingMessage{
					Type: "worker_action_status",
					Payload: map[string]any{
						"command_id": env.CommandID,
						"action":     "remove_network",
						"status":     "success",
						"message":    fmt.Sprintf("network %s removed", name),
					},
				})
			}()

		case "force_remove":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:force_remove", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "force_remove", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid container name rejected", "component", "force_remove", "container", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "force_remove", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "force_remove", "status": "error", "message": "container not found",
						},
					})
					return
				}
				slog.InfoContext(ctx, "stopping and removing container", "component", "force_remove", "container", containerName, "container_id", id[:12])
				_ = docker.StopContainer(ctx, id, 5)
				if err := docker.RemoveContainer(ctx, id, true); err != nil {
					slog.ErrorContext(ctx, "failed to remove container", "component", "force_remove", "container", containerName, "error", err)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "force_remove", "status": "failed", "message": fmt.Sprintf("failed to remove: %v", err),
						},
					})
					return
				}
				slog.InfoContext(ctx, "removed container", "component", "force_remove", "container", containerName)
				_ = ws.SendJSON(client.OutgoingMessage{
					Type: "worker_action_status",
					Payload: map[string]any{
						"action":         "force_remove",
						"status":         "success",
						"message":        fmt.Sprintf("container %s removed", containerName),
						"container_name": containerName,
					},
				})
			}()

		case "exec_start":
			go func() {
				defer telemetry.Recover("handler:exec_start", map[string]any{"command_id": env.CommandID})
				containerName, _ := env.Payload["container_name"].(string)
				commandID := env.CommandID
				if containerName == "" || commandID == "" {
					return
				}
				if !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid container name", "component", "exec_start", "container", containerName)
					return
				}
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					_ = ws.SendJSONReliable(client.OutgoingMessage{
						Type: "exec_output",
						Payload: map[string]any{
							"command_id": commandID,
							"error":      "container not found",
						},
					})
					return
				}
				cmd := []string{"/bin/sh"}
				if cmdPayload, ok := env.Payload["cmd"].(string); ok && cmdPayload != "" {
					cmd = []string{cmdPayload}
				}

				execID, err := docker.ContainerExecCreate(ctx, id, cmd)
				if err != nil {
					_ = ws.SendJSONReliable(client.OutgoingMessage{
						Type: "exec_output",
						Payload: map[string]any{
							"command_id": commandID,
							"error":      fmt.Sprintf("exec create failed: %v", err),
						},
					})
					return
				}

				conn, err := docker.ContainerExecAttach(ctx, execID)
				if err != nil {
					_ = ws.SendJSONReliable(client.OutgoingMessage{
						Type: "exec_output",
						Payload: map[string]any{
							"command_id": commandID,
							"error":      fmt.Sprintf("exec attach failed: %v", err),
						},
					})
					return
				}

				execCtx, execCancel := context.WithCancel(ctx)
				session := &execSession{execID: execID, conn: conn, cancel: execCancel, createdAt: time.Now()}
				execMu.Lock()
				execSessions[commandID] = session
				execMu.Unlock()

				// Read output from exec and forward to orchestrator
				go func() {
					defer telemetry.Recover("handler:exec_start:reader", map[string]any{"command_id": env.CommandID})
					defer func() {
						conn.Close()
						execMu.Lock()
						delete(execSessions, commandID)
						execMu.Unlock()
						_ = ws.SendJSONReliable(client.OutgoingMessage{
							Type: "exec_output",
							Payload: map[string]any{
								"command_id": commandID,
								"closed":     true,
							},
						})
					}()
					buf := make([]byte, 4096)
					for {
						select {
						case <-execCtx.Done():
							return
						default:
						}
						n, err := conn.Reader.Read(buf)
						if n > 0 {
							_ = ws.SendJSONReliable(client.OutgoingMessage{
								Type: "exec_output",
								Payload: map[string]any{
									"command_id": commandID,
									"data":       base64.StdEncoding.EncodeToString(buf[:n]),
								},
							})
						}
						if err != nil {
							if err != io.EOF {
								slog.ErrorContext(ctx, "exec read error", "component", "runner", "command_id", commandID, "error", err)
							}
							return
						}
					}
				}()
			}()

		case "exec_input":
			go func() {
				defer telemetry.Recover("handler:exec_input", map[string]any{"command_id": env.CommandID})
				commandID := env.CommandID
				dataB64, _ := env.Payload["data"].(string)
				if commandID == "" || dataB64 == "" {
					return
				}
				execMu.Lock()
				session, ok := execSessions[commandID]
				execMu.Unlock()
				if !ok {
					return
				}
				data, err := base64.StdEncoding.DecodeString(dataB64)
				if err != nil {
					return
				}
				_, _ = session.conn.Conn.Write(data)
			}()

		case "exec_resize":
			go func() {
				defer telemetry.Recover("handler:exec_resize", map[string]any{"command_id": env.CommandID})
				commandID := env.CommandID
				heightF, _ := env.Payload["height"].(float64)
				widthF, _ := env.Payload["width"].(float64)
				if commandID == "" {
					return
				}
				execMu.Lock()
				session, ok := execSessions[commandID]
				execMu.Unlock()
				if !ok {
					return
				}
				_ = docker.ContainerExecResize(ctx, session.execID, uint(heightF), uint(widthF))
			}()

		case "exec_close":
			go func() {
				defer telemetry.Recover("handler:exec_close", map[string]any{"command_id": env.CommandID})
				commandID := env.CommandID
				if commandID == "" {
					return
				}
				execMu.Lock()
				session, ok := execSessions[commandID]
				execMu.Unlock()
				if !ok {
					return
				}
				session.cancel()
			}()

		case "db_sync_request":
			// The orchestrator wants an immediate report of observed database
			// state — sent on its reconcile tick and whenever this worker
			// reconnects, so anything that changed while we were unreachable is
			// corrected straight away.
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:db_sync_request", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				sendDatabaseSync(ctx, ws, docker)
			}()

		case "db_create":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:db_create", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" || !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid or empty container name", "component", "db_create", "container", containerName)
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"action": "db_create", "status": "error", "message": "invalid or missing container_name",
					})
					return
				}

				engine, _ := env.Payload["engine"].(string)
				engineVersion, _ := env.Payload["engine_version"].(string)
				portF, _ := env.Payload["port"].(float64)
				rootPassword, _ := env.Payload["root_password"].(string)
				databaseName, _ := env.Payload["database_name"].(string)
				username, _ := env.Payload["username"].(string)
				password, _ := env.Payload["password"].(string)
				volumeName, _ := env.Payload["volume_name"].(string)
				cpuLimitF, _ := env.Payload["cpu_limit"].(float64)
				memoryLimitF, _ := env.Payload["memory_limit"].(float64)
				adoptVolume, _ := env.Payload["adopt_existing_volume"].(bool)

				if volumeName == "" {
					volumeName = containerName + "-data"
				}

				port := int(portF)

				// Acknowledge receipt before doing anything slow. Pulling a
				// database image can take minutes; without this the
				// orchestrator cannot distinguish "the worker never got the
				// command" from "the worker is working on it".
				sendDbReply(ctx, ws, env, "db_status", map[string]any{
					"container_name": containerName,
					"action":         "db_create",
					"status":         "accepted",
					"phase":          "ack",
				})

				// Bind the host port for real before pulling several hundred
				// megabytes of image. Probing is not enough on its own — the
				// port can be taken between the check and the container start —
				// but failing here turns a late, opaque Docker bind error into
				// an immediate, specific one.
				if port > 0 {
					if err := probeHostPort(port); err != nil {
						msg := fmt.Sprintf("host port %d is not available: %v", port, err)
						slog.ErrorContext(ctx, "host port is not available", "component", "db_create", "container", containerName, "port", port, "error", err)
						sendLifecycleLog(ctx, ws, containerName, "db_create", msg)
						sendDbReply(ctx, ws, env, "db_status", map[string]any{
							"container_name": containerName,
							"action":         "db_create",
							"status":         "failed",
							"message":        msg,
						})
						return
					}
				}

				memoryLimit := normaliseMemoryLimit(int64(memoryLimitF))
				if memoryLimit != int64(memoryLimitF) {
					slog.WarnContext(ctx, "memory_limit is below Docker's minimum, interpreted as megabytes", "component", "db_create", "container", containerName, "memory_limit", memoryLimitF, "memory_limit_bytes", memoryLimit)
				}

				spec := dockerclient.DatabaseSpec{
					ContainerName: containerName,
					VolumeName:    volumeName,
					Engine:        engine,
					EngineVersion: engineVersion,
					Port:          port,
					RootPassword:  rootPassword,
					DatabaseName:  databaseName,
					Username:      username,
					Password:      password,
					CPULimit:      cpuLimitF,
					MemoryLimit:   memoryLimit,
					AdoptVolume:   adoptVolume,
					// Create-time durability flags. Zero means the 7-day default.
					BinlogRetentionSeconds: func() int {
						if v, ok := env.Payload["binlog_retention_seconds"].(float64); ok {
							return int(v)
						}
						return 0
					}(),
				}

				sendLifecycleLog(ctx, ws, containerName, "db_create", fmt.Sprintf("creating %s:%s database container…", engine, engineVersion))
				containerID, err := docker.CreateDatabaseContainer(ctx, spec)
				if err != nil {
					slog.ErrorContext(ctx, "failed to create database container", "component", "db_create", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_create", fmt.Sprintf("failed to create: %v", err))
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_create",
						"status":         "failed",
						"message":        err.Error(),
					})
					return
				}

				slog.InfoContext(ctx, "created database container", "component", "db_create", "container", containerName, "container_id", containerID[:12])
				sendLifecycleLog(ctx, ws, containerName, "db_create", fmt.Sprintf("database container created and started (id=%s)", containerID[:12]))
				sendDbReply(ctx, ws, env, "db_status", map[string]any{
					"container_name": containerName,
					"action":         "db_create",
					"status":         "success",
					"container_id":   containerID,
				})
			}()

		case "db_start":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:db_start", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" || !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid or empty container name", "component", "db_start", "container", containerName)
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"action": "db_start", "status": "error", "message": "invalid or missing container_name",
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "db_start", "looking up database container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				// Distinguish a Docker API failure from a genuinely absent
				// container: reporting "not found" for both sends the reader
				// hunting for a deleted container when the daemon merely
				// errored.
				if err != nil {
					slog.ErrorContext(ctx, "failed to look up container", "component", "db_start", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_start", fmt.Sprintf("failed to look up container: %v", err))
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_start",
						"status":         "failed",
						"message":        fmt.Sprintf("failed to look up container: %v", err),
					})
					return
				}
				if id == "" {
					slog.WarnContext(ctx, "container not found", "component", "db_start", "container", containerName)
					sendLifecycleLog(ctx, ws, containerName, "db_start", "container not found")
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_start",
						"status":         "failed",
						"message":        "container not found",
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "db_start", fmt.Sprintf("starting database container (id=%s)…", id[:12]))
				if err := docker.StartContainer(ctx, id); err != nil {
					slog.ErrorContext(ctx, "failed to start database container", "component", "db_start", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_start", fmt.Sprintf("failed to start: %v", err))
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_start",
						"status":         "failed",
						"message":        err.Error(),
					})
				} else {
					slog.InfoContext(ctx, "started database container", "component", "db_start", "container", containerName)
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_start",
						"status":         "success",
					})
				}
			}()

		case "db_stop":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:db_stop", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" || !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid or empty container name", "component", "db_stop", "container", containerName)
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"action": "db_stop", "status": "error", "message": "invalid or missing container_name",
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "db_stop", "looking up database container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				// Distinguish a Docker API failure from a genuinely absent
				// container: reporting "not found" for both sends the reader
				// hunting for a deleted container when the daemon merely
				// errored.
				if err != nil {
					slog.ErrorContext(ctx, "failed to look up container", "component", "db_stop", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_stop", fmt.Sprintf("failed to look up container: %v", err))
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_stop",
						"status":         "failed",
						"message":        fmt.Sprintf("failed to look up container: %v", err),
					})
					return
				}
				if id == "" {
					slog.WarnContext(ctx, "container not found", "component", "db_stop", "container", containerName)
					sendLifecycleLog(ctx, ws, containerName, "db_stop", "container not found")
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_stop",
						"status":         "failed",
						"message":        "container not found",
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "db_stop", fmt.Sprintf("stopping database container (timeout=30s, id=%s)…", id[:12]))
				if err := docker.StopContainer(ctx, id, 30); err != nil {
					slog.ErrorContext(ctx, "failed to stop database container", "component", "db_stop", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_stop", fmt.Sprintf("failed to stop: %v", err))
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_stop",
						"status":         "failed",
						"message":        err.Error(),
					})
				} else {
					slog.InfoContext(ctx, "stopped database container", "component", "db_stop", "container", containerName)
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_stop",
						"status":         "success",
					})
				}
			}()

		case "db_restart":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:db_restart", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" || !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid or empty container name", "component", "db_restart", "container", containerName)
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"action": "db_restart", "status": "error", "message": "invalid or missing container_name",
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "db_restart", "looking up database container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				// Distinguish a Docker API failure from a genuinely absent
				// container: reporting "not found" for both sends the reader
				// hunting for a deleted container when the daemon merely
				// errored.
				if err != nil {
					slog.ErrorContext(ctx, "failed to look up container", "component", "db_restart", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_restart", fmt.Sprintf("failed to look up container: %v", err))
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_restart",
						"status":         "failed",
						"message":        fmt.Sprintf("failed to look up container: %v", err),
					})
					return
				}
				if id == "" {
					slog.WarnContext(ctx, "container not found", "component", "db_restart", "container", containerName)
					sendLifecycleLog(ctx, ws, containerName, "db_restart", "container not found")
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_restart",
						"status":         "failed",
						"message":        "container not found",
					})
					return
				}
				sendLifecycleLog(ctx, ws, containerName, "db_restart", fmt.Sprintf("restarting database container (timeout=30s, id=%s)…", id[:12]))
				if err := docker.RestartContainer(ctx, id, 30); err != nil {
					slog.ErrorContext(ctx, "failed to restart database container", "component", "db_restart", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_restart", fmt.Sprintf("failed to restart: %v", err))
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_restart",
						"status":         "failed",
						"message":        err.Error(),
					})
				} else {
					slog.InfoContext(ctx, "restarted database container", "component", "db_restart", "container", containerName)
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_restart",
						"status":         "success",
					})
				}
			}()

		case "db_remove":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:db_remove", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" || !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid or empty container name", "component", "db_remove", "container", containerName)
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"action": "db_remove", "status": "error", "message": "invalid or missing container_name",
					})
					return
				}
				// removeVolume distinguishes the two callers of db_remove: the
				// `remove` action tears the container down and keeps the data,
				// a delete purges both.
				removeVolume, _ := env.Payload["remove_volume"].(bool)
				volumeName, _ := env.Payload["volume_name"].(string)
				if removeVolume && (volumeName == "" || !validContainerName(volumeName)) {
					slog.WarnContext(ctx, "volume purge requested with invalid volume name", "component", "db_remove", "container", containerName, "volume", volumeName)
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_remove",
						"status":         "failed",
						"message":        "volume purge requested but volume_name is missing or invalid",
					})
					return
				}

				sendLifecycleLog(ctx, ws, containerName, "db_remove", "looking up database container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				// Distinguish a Docker API failure from a genuinely absent
				// container: reporting "not found" for both sends the reader
				// hunting for a deleted container when the daemon merely
				// errored.
				if err != nil {
					slog.ErrorContext(ctx, "failed to look up container", "component", "db_remove", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_remove", fmt.Sprintf("failed to look up container: %v", err))
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_remove",
						"status":         "failed",
						"message":        fmt.Sprintf("failed to look up container: %v", err),
					})
					return
				}

				// An absent container is the *goal* of a remove, not a failure.
				// Reporting it as one used to poison the instance into `error`
				// on any second remove — and made a delete unable to clean up
				// the volume left behind by a first one.
				if id == "" {
					slog.WarnContext(ctx, "container already absent", "component", "db_remove", "container", containerName)
					sendLifecycleLog(ctx, ws, containerName, "db_remove", "container already absent, nothing to remove")
				} else {
					sendLifecycleLog(ctx, ws, containerName, "db_remove", fmt.Sprintf("stopping database container before removal (timeout=10s, id=%s)…", id[:12]))
					if err := docker.StopContainer(ctx, id, 10); err != nil {
						sendLifecycleWarn(ctx, ws, containerName, "db_remove", fmt.Sprintf("stop returned: %v (proceeding with remove)", err))
					} else {
						sendLifecycleLog(ctx, ws, containerName, "db_remove", "container stopped, removing…")
					}
					if err := docker.RemoveContainer(ctx, id, true); err != nil {
						slog.ErrorContext(ctx, "failed to remove database container", "component", "db_remove", "container", containerName, "error", err)
						sendLifecycleLog(ctx, ws, containerName, "db_remove", fmt.Sprintf("failed to remove: %v", err))
						sendDbReply(ctx, ws, env, "db_status", map[string]any{
							"container_name": containerName,
							"action":         "db_remove",
							"status":         "failed",
							"message":        err.Error(),
						})
						return
					}
					slog.InfoContext(ctx, "removed database container", "component", "db_remove", "container", containerName)
				}

				if !removeVolume {
					sendLifecycleLog(ctx, ws, containerName, "db_remove", "database container removed (volume preserved)")
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_remove",
						"status":         "success",
						"volume_removed": false,
					})
					return
				}

				// force=true makes the daemon treat an already-absent volume as
				// success, so a retried purge converges instead of erroring on
				// the volume it removed last time.
				sendLifecycleLog(ctx, ws, containerName, "db_remove", fmt.Sprintf("removing data volume %s…", volumeName))
				if err := docker.RemoveVolume(ctx, volumeName, true); err != nil {
					slog.ErrorContext(ctx, "failed to remove data volume", "component", "db_remove", "container", containerName, "volume", volumeName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_remove", fmt.Sprintf("failed to remove data volume: %v", err))
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_remove",
						"status":         "failed",
						"message":        fmt.Sprintf("container removed but data volume %s could not be deleted: %v", volumeName, err),
					})
					return
				}

				slog.InfoContext(ctx, "removed database container and data volume", "component", "db_remove", "container", containerName, "volume", volumeName)
				sendLifecycleLog(ctx, ws, containerName, "db_remove", "database container and data volume removed")
				sendDbReply(ctx, ws, env, "db_status", map[string]any{
					"container_name": containerName,
					"action":         "db_remove",
					"status":         "success",
					"volume_removed": true,
				})
			}()

		case "db_snapshot":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:db_snapshot", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				engine, _ := env.Payload["engine"].(string)
				databaseName, _ := env.Payload["database_name"].(string)
				username, _ := env.Payload["username"].(string)
				password, _ := env.Payload["password"].(string)
				snapshotID := payloadString(env.Payload, "snapshot_id")
				remotePath := payloadString(env.Payload, "remote_path", "filename")
				destType, destConfig := backupDestinationFrom(env.Payload)

				if containerName == "" || !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid or empty container name", "component", "db_snapshot", "container", containerName)
					sendDbReply(ctx, ws, env, "db_snapshot_status", map[string]any{
						"snapshot_id":    snapshotID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  "invalid or missing container_name",
					})
					return
				}
				if engine == "" || databaseName == "" {
					sendDbReply(ctx, ws, env, "db_snapshot_status", map[string]any{
						"snapshot_id":    snapshotID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  "missing required fields (engine, database_name)",
					})
					return
				}

				// Send uploading status
				sendDbReply(ctx, ws, env, "db_snapshot_status", map[string]any{
					"snapshot_id":    snapshotID,
					"container_name": containerName,
					"status":         "uploading",
				})

				sendLifecycleLog(ctx, ws, containerName, "db_snapshot", "looking up database container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				// Distinguish a Docker API failure from a genuinely absent
				// container: reporting "not found" for both sends the reader
				// hunting for a deleted container when the daemon merely
				// errored.
				if err != nil {
					slog.ErrorContext(ctx, "failed to look up container", "component", "db_snapshot", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_snapshot", fmt.Sprintf("failed to look up container: %v", err))
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_snapshot",
						"status":         "failed",
						"message":        fmt.Sprintf("failed to look up container: %v", err),
					})
					return
				}
				if id == "" {
					slog.WarnContext(ctx, "container not found", "component", "db_snapshot", "container", containerName)
					sendLifecycleLog(ctx, ws, containerName, "db_snapshot", "container not found")
					sendDbReply(ctx, ws, env, "db_snapshot_status", map[string]any{
						"snapshot_id":    snapshotID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  "container not found",
					})
					return
				}

				// Build the destination first: a bad destination should fail
				// before the database is asked to produce a dump.
				dest, err := backup.NewDestination(destType, destConfig)
				if err != nil {
					slog.ErrorContext(ctx, "failed to create backup destination", "component", "db_snapshot", "container", containerName, "error", err)
					sendDbReply(ctx, ws, env, "db_snapshot_status", map[string]any{
						"snapshot_id":    snapshotID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  fmt.Sprintf("failed to create backup destination: %v", err),
					})
					return
				}

				sendLifecycleLog(ctx, ws, containerName, "db_snapshot", fmt.Sprintf("streaming %s dump to backup destination…", engine))
				size, err := streamSnapshot(ctx,
					func(c context.Context) (io.ReadCloser, error) {
						return docker.ExecDatabaseDump(c, id, engine, databaseName, username, password)
					}, dest, remotePath)
				if err != nil {
					slog.ErrorContext(ctx, "snapshot failed", "component", "db_snapshot", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_snapshot", fmt.Sprintf("snapshot failed: %v", err))
					sendDbReply(ctx, ws, env, "db_snapshot_status", map[string]any{
						"snapshot_id":    snapshotID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  err.Error(),
					})
					return
				}

				slog.InfoContext(ctx, "snapshot completed", "component", "db_snapshot", "container", containerName, "size_bytes", size)
				sendLifecycleLog(ctx, ws, containerName, "db_snapshot", fmt.Sprintf("snapshot completed (size=%d bytes)", size))
				sendDbReply(ctx, ws, env, "db_snapshot_status", map[string]any{
					"snapshot_id":    snapshotID,
					"container_name": containerName,
					"status":         "completed",
					"size_bytes":     size,
				})
			}()

		case "db_restore":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:db_restore", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				engine, _ := env.Payload["engine"].(string)
				databaseName, _ := env.Payload["database_name"].(string)
				username, _ := env.Payload["username"].(string)
				password, _ := env.Payload["password"].(string)
				// The orchestrator identifies a restore by the snapshot it is
				// restoring from; `restore_id` was never sent.
				restoreID := payloadString(env.Payload, "restore_id", "snapshot_id")
				remotePath := payloadString(env.Payload, "remote_path", "filename")
				destType, destConfig := backupDestinationFrom(env.Payload)

				if containerName == "" || !validContainerName(containerName) {
					slog.WarnContext(ctx, "invalid or empty container name", "component", "db_restore", "container", containerName)
					sendDbReply(ctx, ws, env, "db_restore_status", map[string]any{
						"restore_id":     restoreID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  "invalid or missing container_name",
					})
					return
				}
				if engine == "" || databaseName == "" {
					sendDbReply(ctx, ws, env, "db_restore_status", map[string]any{
						"restore_id":     restoreID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  "missing required fields (engine, database_name)",
					})
					return
				}

				// Send downloading status
				sendDbReply(ctx, ws, env, "db_restore_status", map[string]any{
					"restore_id":     restoreID,
					"container_name": containerName,
					"status":         "downloading",
				})

				sendLifecycleLog(ctx, ws, containerName, "db_restore", "downloading snapshot from backup destination…")
				dest, err := backup.NewDestination(destType, destConfig)
				if err != nil {
					slog.ErrorContext(ctx, "failed to create backup destination", "component", "db_restore", "container", containerName, "error", err)
					sendDbReply(ctx, ws, env, "db_restore_status", map[string]any{
						"restore_id":     restoreID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  fmt.Sprintf("failed to create backup destination: %v", err),
					})
					return
				}

				tmpDir, err := os.MkdirTemp("", "lattice-restore-*")
				if err != nil {
					slog.ErrorContext(ctx, "failed to create temp dir", "component", "db_restore", "container", containerName, "error", err)
					sendDbReply(ctx, ws, env, "db_restore_status", map[string]any{
						"restore_id":     restoreID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  fmt.Sprintf("failed to create temp dir: %v", err),
					})
					return
				}
				defer os.RemoveAll(tmpDir)

				tmpFile := filepath.Join(tmpDir, "restore.sql")
				if err := dest.Download(ctx, remotePath, tmpFile); err != nil {
					slog.ErrorContext(ctx, "snapshot download failed", "component", "db_restore", "container", containerName, "path", remotePath, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_restore", fmt.Sprintf("download failed: %v", err))
					sendDbReply(ctx, ws, env, "db_restore_status", map[string]any{
						"restore_id":     restoreID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  fmt.Sprintf("download failed: %v", err),
					})
					return
				}

				sendLifecycleLog(ctx, ws, containerName, "db_restore", "looking up database container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				// Distinguish a Docker API failure from a genuinely absent
				// container: reporting "not found" for both sends the reader
				// hunting for a deleted container when the daemon merely
				// errored.
				if err != nil {
					slog.ErrorContext(ctx, "failed to look up container", "component", "db_restore", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_restore", fmt.Sprintf("failed to look up container: %v", err))
					sendDbReply(ctx, ws, env, "db_status", map[string]any{
						"container_name": containerName,
						"action":         "db_restore",
						"status":         "failed",
						"message":        fmt.Sprintf("failed to look up container: %v", err),
					})
					return
				}
				if id == "" {
					slog.WarnContext(ctx, "container not found", "component", "db_restore", "container", containerName)
					sendLifecycleLog(ctx, ws, containerName, "db_restore", "container not found")
					sendDbReply(ctx, ws, env, "db_restore_status", map[string]any{
						"restore_id":     restoreID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  "container not found",
					})
					return
				}

				sendLifecycleLog(ctx, ws, containerName, "db_restore", fmt.Sprintf("restoring %s database…", engine))
				restoreFile, err := os.Open(tmpFile)
				if err != nil {
					slog.ErrorContext(ctx, "failed to open temp file", "component", "db_restore", "container", containerName, "error", err)
					sendDbReply(ctx, ws, env, "db_restore_status", map[string]any{
						"restore_id":     restoreID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  fmt.Sprintf("failed to open restore file: %v", err),
					})
					return
				}
				defer restoreFile.Close()

				// Snapshots are gzipped; older ones are not. Decompress by
				// content rather than by filename — the .sql.gz suffix was a lie
				// for months before compression actually existed.
				restoreReader, err := maybeGunzip(restoreFile)
				if err != nil {
					slog.ErrorContext(ctx, "failed to read snapshot", "component", "db_restore", "container", containerName, "error", err)
					sendDbReply(ctx, ws, env, "db_restore_status", map[string]any{
						"restore_id":     restoreID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  err.Error(),
					})
					return
				}

				if err := docker.ExecDatabaseRestore(ctx, id, engine, databaseName, username, password, restoreReader); err != nil {
					slog.ErrorContext(ctx, "restore failed", "component", "db_restore", "container", containerName, "error", err)
					sendLifecycleLog(ctx, ws, containerName, "db_restore", fmt.Sprintf("restore failed: %v", err))
					sendDbReply(ctx, ws, env, "db_restore_status", map[string]any{
						"restore_id":     restoreID,
						"container_name": containerName,
						"status":         "failed",
						"error_message":  fmt.Sprintf("restore failed: %v", err),
					})
					return
				}

				slog.InfoContext(ctx, "restore completed", "component", "db_restore", "container", containerName)
				sendLifecycleLog(ctx, ws, containerName, "db_restore", "database restore completed")
				sendDbReply(ctx, ws, env, "db_restore_status", map[string]any{
					"restore_id":     restoreID,
					"container_name": containerName,
					"status":         "completed",
				})
			}()

		case "db_update_schedule":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:db_update_schedule", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				instanceIDFloat, _ := env.Payload["instance_id"].(float64)
				instanceID := int(instanceIDFloat)
				enabled, _ := env.Payload["enabled"].(bool)

				if !enabled {
					snapshotScheduler.RemoveSchedule(instanceID)
					slog.InfoContext(ctx, "removed schedule", "component", "db_update_schedule", "instance_id", instanceID)
					sendDbReply(ctx, ws, env, "db_schedule_status", map[string]any{
						"instance_id": instanceID,
						"status":      "removed",
					})
					return
				}

				containerName, _ := env.Payload["container_name"].(string)
				engine, _ := env.Payload["engine"].(string)
				databaseName, _ := env.Payload["database_name"].(string)
				username, _ := env.Payload["username"].(string)
				password, _ := env.Payload["password"].(string)
				cron, _ := env.Payload["cron"].(string)
				retentionF, _ := env.Payload["retention_count"].(float64)

				// The orchestrator sends this as `backup_destination`, matching
				// every other db_* command; this side read `backup_dest`, so
				// every scheduled job was registered with a nil destination and
				// silently did nothing when it fired. Accept both.
				backupDest, _ := env.Payload["backup_destination"].(map[string]any)
				if backupDest == nil {
					backupDest, _ = env.Payload["backup_dest"].(map[string]any)
				}

				snapshotScheduler.UpdateSchedule(scheduler.Job{
					InstanceID:     instanceID,
					ContainerName:  containerName,
					Engine:         engine,
					DatabaseName:   databaseName,
					Username:       username,
					Password:       password,
					Cron:           cron,
					RetentionCount: int(retentionF),
					BackupDest:     backupDest,
				})

				slog.InfoContext(ctx, "updated schedule", "component", "db_update_schedule", "instance_id", instanceID, "cron", cron)
				sendDbReply(ctx, ws, env, "db_schedule_status", map[string]any{
					"instance_id": instanceID,
					"status":      "updated",
					"cron":        cron,
				})
			}()

		case "backup_dest_test":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:backup_dest_test", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				destType, _ := env.Payload["dest_type"].(string)
				destConfig, _ := env.Payload["dest_config"].(map[string]any)

				dest, err := backup.NewDestination(destType, destConfig)
				if err != nil {
					slog.WarnContext(ctx, "failed to create destination", "component", "backup_dest_test", "error", err)
					wsSendReliable(ctx, ws, "backup_dest_test_result", client.OutgoingMessage{
						Type: "backup_dest_test_result",
						Payload: map[string]any{
							"command_id": env.CommandID,
							"status":     "failed",
							"message":    fmt.Sprintf("failed to create destination: %v", err),
						},
					})
					return
				}

				if err := dest.Test(ctx); err != nil {
					slog.WarnContext(ctx, "destination test failed", "component", "backup_dest_test", "destination_type", destType, "error", err)
					wsSendReliable(ctx, ws, "backup_dest_test_result", client.OutgoingMessage{
						Type: "backup_dest_test_result",
						Payload: map[string]any{
							"command_id": env.CommandID,
							"status":     "failed",
							"message":    fmt.Sprintf("connection test failed: %v", err),
						},
					})
					return
				}

				slog.InfoContext(ctx, "destination test passed", "component", "backup_dest_test", "destination_type", destType)
				wsSendReliable(ctx, ws, "backup_dest_test_result", client.OutgoingMessage{
					Type: "backup_dest_test_result",
					Payload: map[string]any{
						"command_id": env.CommandID,
						"status":     "success",
						"message":    "connection test passed",
					},
				})
			}()

		case "db_mirror_snapshot":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:db_mirror_snapshot", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()

				filename := payloadString(env.Payload, "filename", "remote_path")
				sourceRaw, _ := env.Payload["source_destination"].(map[string]any)
				targetRaw, _ := env.Payload["target_destination"].(map[string]any)

				// fail logs msg (fixed text, attributed to the caller of fail)
				// and replies with reason, the text the control plane shows.
				fail := func(level slog.Level, msg, reason string, err error) {
					args := []any{"component", "db_mirror_snapshot", "path", filename}
					if err != nil {
						args = append(args, "error", err)
					}
					telemetry.LogAt(ctx, 1, level, msg, args...)
					sendDbReply(ctx, ws, env, "db_mirror_status", map[string]any{
						"filename":      filename,
						"status":        "failed",
						"error_message": reason,
					})
				}

				if filename == "" || sourceRaw == nil || targetRaw == nil {
					fail(slog.LevelWarn, "missing filename, source_destination or target_destination", "missing filename, source_destination or target_destination", nil)
					return
				}

				sourceType, _ := sourceRaw["type"].(string)
				sourceConfig, _ := sourceRaw["config"].(map[string]any)
				targetType, _ := targetRaw["type"].(string)
				targetConfig, _ := targetRaw["config"].(map[string]any)

				source, err := backup.NewDestination(sourceType, sourceConfig)
				if err != nil {
					fail(slog.LevelError, "source destination unusable", fmt.Sprintf("source destination unusable: %v", err), err)
					return
				}
				target, err := backup.NewDestination(targetType, targetConfig)
				if err != nil {
					fail(slog.LevelError, "target destination unusable", fmt.Sprintf("target destination unusable: %v", err), err)
					return
				}

				// Copy via a temp file rather than streaming between the two.
				// A mirror is a background copy of an artifact that already
				// exists safely; staging keeps a slow target from holding a
				// read open on the source for the whole transfer, and no
				// database is waiting on this.
				tmpDir, err := os.MkdirTemp("", "lattice-mirror-*")
				if err != nil {
					fail(slog.LevelError, "failed to create temp dir", fmt.Sprintf("failed to create temp dir: %v", err), err)
					return
				}
				defer os.RemoveAll(tmpDir)
				local := filepath.Join(tmpDir, "snapshot.bin")

				if err := source.Download(ctx, filename, local); err != nil {
					fail(slog.LevelError, "failed to read snapshot from the primary destination", fmt.Sprintf("failed to read %s from the primary destination: %v", filename, err), err)
					return
				}

				size, err := target.Upload(ctx, local, filename)
				if err != nil {
					fail(slog.LevelError, "failed to write snapshot to the mirror", fmt.Sprintf("failed to write %s to the mirror: %v", filename, err), err)
					return
				}

				slog.InfoContext(ctx, "mirrored snapshot", "component", "db_mirror_snapshot", "path", filename, "size_bytes", size)
				sendDbReply(ctx, ws, env, "db_mirror_status", map[string]any{
					"filename":   filename,
					"status":     "completed",
					"size_bytes": size,
				})
			}()

		case "db_delete_snapshot_file":
			handlerSem <- struct{}{}
			go func() {
				defer telemetry.Recover("handler:db_delete_snapshot_file", map[string]any{"command_id": env.CommandID})
				defer func() { <-handlerSem }()
				destType, destConfig := backupDestinationFrom(env.Payload)
				remotePath := payloadString(env.Payload, "remote_path", "filename")
				snapshotID := payloadString(env.Payload, "snapshot_id")

				// This is the check that made the remote-delete path a no-op:
				// the orchestrator sends the object name as `filename`, so
				// `remote_path` was always empty and every snapshot file was
				// left on the destination forever — the exact leak the July
				// pass believed it had fixed by starting to send the command.
				if remotePath == "" {
					sendDbReply(ctx, ws, env, "db_delete_snapshot_result", map[string]any{
						"snapshot_id": snapshotID,
						"status":      "failed",
						"message":     "missing remote_path/filename",
					})
					return
				}

				dest, err := backup.NewDestination(destType, destConfig)
				if err != nil {
					slog.ErrorContext(ctx, "failed to create destination", "component", "db_delete_snapshot_file", "path", remotePath, "error", err)
					sendDbReply(ctx, ws, env, "db_delete_snapshot_result", map[string]any{
						"snapshot_id": snapshotID,
						"status":      "failed",
						"message":     fmt.Sprintf("failed to create destination: %v", err),
					})
					return
				}

				if err := dest.Delete(ctx, remotePath); err != nil {
					slog.ErrorContext(ctx, "delete failed", "component", "db_delete_snapshot_file", "path", remotePath, "error", err)
					sendDbReply(ctx, ws, env, "db_delete_snapshot_result", map[string]any{
						"snapshot_id": snapshotID,
						"status":      "failed",
						"message":     fmt.Sprintf("delete failed: %v", err),
					})
					return
				}

				slog.InfoContext(ctx, "deleted snapshot file", "component", "db_delete_snapshot_file", "path", remotePath)
				sendDbReply(ctx, ws, env, "db_delete_snapshot_result", map[string]any{
					"snapshot_id": snapshotID,
					"status":      "success",
				})
			}()
		}
	})

	// Start WebSocket connection in background
	safeGo(ws, "ws-connect", func() { ws.Connect(ctx) })

	// Stream container logs to orchestrator
	logStreamer := dockerclient.NewLogStreamer(docker, func(line dockerclient.LogLine) {
		_ = ws.SendJSON(client.OutgoingMessage{
			Type: "container_logs",
			Payload: map[string]any{
				"container_name": line.ContainerName,
				"stream":         line.Stream,
				"message":        line.Message,
				"recorded_at":    line.RecordedAt.UTC().Format(time.RFC3339Nano),
			},
		})
	}, 10*time.Second)
	safeGoResilient("log-streamer", func() { logStreamer.Run(ctx) })

	// Database observer — periodically reports the true state of managed
	// database containers so the orchestrator can reconcile against it rather
	// than relying solely on command replies arriving intact.
	safeGoResilient("db-observer", func() { startDatabaseObserver(ctx, ws, docker) })

	// Network health monitor — detects DNS failures, bridge-only containers,
	// restart loops, and attempts auto-repair. Reports via lifecycle_log.
	netMonitor := dockerclient.NewNetMonitor(docker, func(diag dockerclient.NetworkDiagnostic) {
		msg := fmt.Sprintf("[network] %s: %s", diag.Issue, diag.Detail)
		if diag.Repaired {
			msg += fmt.Sprintf(" | auto-repaired: %s", diag.RepairDetail)
		}
		_ = ws.SendJSON(client.OutgoingMessage{
			Type: "lifecycle_log",
			Payload: map[string]any{
				"container_name": diag.ContainerName,
				"event":          "network_diagnostic",
				"message":        msg,
			},
		})
	}, 30*time.Second)
	safeGoResilient("net-monitor", func() {
		netMonitor.Run(ctx, func(event dockerclient.RestartLoopEvent) {
			_ = ws.SendJSON(client.OutgoingMessage{
				Type: "lifecycle_log",
				Payload: map[string]any{
					"container_name": event.ContainerName,
					"event":          "restart_loop",
					"message":        event.Message,
				},
			})
		})
	})

	// Heartbeat ticker — also pushes live container states each tick
	safeGoResilient("heartbeat", func() {
		ticker := time.NewTicker(cfg.HeartbeatInterval)
		defer ticker.Stop()

		heartbeatCount := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				heartbeatCount++
				m := metrics.Collect(ctx, docker)
				runnerMetrics := metrics.CollectRunnerMetrics()
				payload := map[string]any{
					"cpu_percent":             m.CPUPercent,
					"cpu_cores":               m.CPUCores,
					"load_avg_1":              m.LoadAvg1,
					"load_avg_5":              m.LoadAvg5,
					"load_avg_15":             m.LoadAvg15,
					"memory_used_mb":          m.MemoryUsedMB,
					"memory_total_mb":         m.MemoryTotalMB,
					"memory_free_mb":          m.MemoryFreeMB,
					"swap_used_mb":            m.SwapUsedMB,
					"swap_total_mb":           m.SwapTotalMB,
					"disk_used_mb":            m.DiskUsedMB,
					"disk_total_mb":           m.DiskTotalMB,
					"container_count":         m.ContainerCount,
					"container_running_count": m.ContainerRunningCount,
					"network_rx_bytes":        m.NetworkRxBytes,
					"network_tx_bytes":        m.NetworkTxBytes,
					"uptime_seconds":          m.UptimeSeconds,
					"process_count":           m.ProcessCount,
					"runner_version":          Version,
					"runner_goroutines":       runnerMetrics["runner_goroutines"],
					"runner_heap_mb":          runnerMetrics["runner_heap_mb"],
					"runner_sys_mb":           runnerMetrics["runner_sys_mb"],
				}

				// Collect per-container resource stats every 3rd heartbeat (expensive)
				if heartbeatCount%3 == 0 {
					if containerStats, err := docker.ContainerStats(ctx); err == nil && len(containerStats) > 0 {
						payload["container_stats"] = containerStats
					}
				}

				wsSend(ctx, ws, "heartbeat", client.OutgoingMessage{
					Type:    "heartbeat",
					Payload: payload,
				})

				// Clean up old deployment states to prevent memory leak
				deploymentStatesMu.Lock()
				for id, st := range deploymentStates {
					if st.Status != "deploying" && time.Since(st.LastProgressAt) > 15*time.Minute {
						delete(deploymentStates, id)
					}
				}
				deploymentStatesMu.Unlock()

				// Push live container state snapshot so the orchestrator stays in sync
				// even when containers are stopped/started outside of Lattice.
				if containers, err := docker.ListContainers(ctx, ""); err == nil {
					for _, c := range containers {
						name := ""
						for _, n := range c.Names {
							trimmed := strings.TrimPrefix(n, "/")
							if trimmed != "" {
								name = dockerclient.CanonicalContainerName(trimmed)
								break
							}
						}
						if name == "" {
							continue
						}

						// Map Docker state to Lattice status.
						var latticeStatus string
						switch c.State {
						case "running":
							latticeStatus = "running"
						case "paused":
							latticeStatus = "paused"
						case "exited", "dead":
							latticeStatus = "stopped"
						case "created":
							latticeStatus = "pending"
						case "restarting":
							latticeStatus = "restarting"
						default:
							latticeStatus = "error"
						}

						statePayload := map[string]any{
							"container_name": name,
							"state":          c.State,
							"status":         latticeStatus,
						}

						// Report health status if available.
						if c.Status != "" {
							healthStatus := ""
							switch {
							case strings.Contains(c.Status, "(healthy)"):
								healthStatus = "healthy"
							case strings.Contains(c.Status, "(unhealthy)"):
								healthStatus = "unhealthy"
							case strings.Contains(c.Status, "(health: starting)"):
								healthStatus = "starting"
							}
							if healthStatus != "" {
								statePayload["health_status"] = healthStatus
								_ = ws.SendJSON(client.OutgoingMessage{
									Type: "container_health_status",
									Payload: map[string]any{
										"container_name": name,
										"health_status":  healthStatus,
									},
								})
							}
						}

						_ = ws.SendJSON(client.OutgoingMessage{
							Type:    "container_sync",
							Payload: statePayload,
						})
					}
				}
			}
		}
	})

	// Start local dashboard
	dashboard := &web.Server{
		Docker:     docker,
		Version:    Version,
		WorkerName: cfg.WorkerName,
		StartedAt:  time.Now(),
		Port:       cfg.DashboardPort,
		LatticeURL: cfg.LatticeURL,
	}
	go dashboard.Start()

	fmt.Println()
	fmt.Println("Lattice Runner ready")

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.InfoContext(ctx, "shutting down gracefully", "component", "runner")

	// Wait for in-flight deployments to finish (up to 60s)
	shutdownDeadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(shutdownDeadline) {
		deploymentStatesMu.RLock()
		hasActive := false
		for _, st := range deploymentStates {
			if st.InProgress {
				hasActive = true
				break
			}
		}
		deploymentStatesMu.RUnlock()
		if !hasActive {
			break
		}
		slog.InfoContext(ctx, "waiting for in-flight deployment to complete", "component", "runner")
		time.Sleep(2 * time.Second)
	}

	wsSend(ctx, ws, "worker_shutdown", client.OutgoingMessage{
		Type: "worker_shutdown",
		Payload: map[string]any{
			"reason":  "graceful",
			"message": "runner shutting down gracefully",
		},
	})
	// Drain BEFORE cancel: the write pump exits on context cancellation, so
	// cancelling first would strand every queued message (including worker_shutdown).
	// Give in-flight handlers a moment to enqueue their final status, flush the
	// queue while the pump is still alive, then stop goroutines and close.
	time.Sleep(2 * time.Second) // let in-flight work enqueue final messages
	ws.Drain(5 * time.Second)   // flush remaining messages while the write pump lives
	cancel()                    // signal all goroutines to stop
	ws.Close()
	slog.InfoContext(ctx, "runner stopped", "component", "runner")
	telemetry.Shutdown("signal")
}

// sendLifecycleLog sends a verbose lifecycle log entry to the orchestrator so
// it gets persisted in the lifecycle_logs table and broadcast to the admin UI.
// sendDbReply emits a database-subsystem reply to the orchestrator, echoing the
// correlation fields from the command that triggered it.
//
// Every db_* reply must go through here. Replies used to be built as bare
// payload literals that omitted database_instance_id entirely, so the
// orchestrator could not match a reply to the row it was supposed to update —
// which meant no managed database could ever leave "pending", whether the
// operation succeeded or failed.
//
// A send failure is attributed to sendDbReply's caller.
func sendDbReply(ctx context.Context, ws *client.WSClient, env client.Envelope, msgType string, payload map[string]any) {
	wsSendReliableAt(ctx, 1, ws, msgType, client.OutgoingMessage{
		Type:    msgType,
		Payload: buildDbReplyPayload(env, payload),
	})
}

// payloadString reads the first of several candidate keys that yields a
// non-empty string, tolerating a JSON number where a string is expected.
//
// This exists because the orchestrator and the runner disagreed on the name of
// every field in the snapshot command family: the API sends `filename`,
// `backup_destination.{type,config}` and a numeric `snapshot_id`, while this
// side read `remote_path`, `dest_type`/`dest_config` and asserted
// `snapshot_id.(string)`. Each mismatch failed silently — an empty destination
// type, an empty object key, and a reply whose snapshot id the orchestrator
// could not match to a row. Accepting both spellings here (rather than only
// fixing the API) means a runner upgraded ahead of the control plane repairs
// the whole family on its own.
func payloadString(p map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := p[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return strconv.FormatInt(int64(v), 10)
		case int:
			return strconv.Itoa(v)
		}
	}
	return ""
}

// payloadNested reads a nested map by parent key, e.g. backup_destination.config.
func payloadNested(p map[string]any, parent, child string) any {
	m, ok := p[parent].(map[string]any)
	if !ok {
		return nil
	}
	return m[child]
}

// backupDestinationFrom resolves the backup destination from either shape: the
// flat `dest_type`/`dest_config` pair this side originally read, or the nested
// `backup_destination` object the orchestrator actually sends.
func backupDestinationFrom(p map[string]any) (string, map[string]any) {
	destType := payloadString(p, "dest_type")
	destConfig, _ := p["dest_config"].(map[string]any)

	if destType == "" {
		if s, ok := payloadNested(p, "backup_destination", "type").(string); ok {
			destType = s
		}
	}
	if destConfig == nil {
		if m, ok := payloadNested(p, "backup_destination", "config").(map[string]any); ok {
			destConfig = m
		}
	}
	return destType, destConfig
}

// buildDbReplyPayload attaches correlation and phase to a database reply.
//
// Kept separate from the send so the contract can be tested without a socket —
// this is the exact logic whose absence meant no managed database could ever
// leave "pending".
func buildDbReplyPayload(env client.Envelope, payload map[string]any) map[string]any {
	if payload == nil {
		payload = map[string]any{}
	}

	// Echo correlation back verbatim. Pass through exactly what the
	// orchestrator sent so a reply is never ambiguous.
	for _, key := range []string{"database_instance_id", "request_id", "idempotency_key"} {
		if v, ok := env.Payload[key]; ok {
			if _, already := payload[key]; !already {
				payload[key] = v
			}
		}
	}

	// Commands from an older orchestrator carry no instance id, but the
	// scheduler path supplies one via instance_id. Prefer the explicit field.
	if _, ok := payload["database_instance_id"]; !ok {
		if v, ok := payload["instance_id"]; ok {
			payload["database_instance_id"] = v
		}
	}

	if _, ok := payload["action"]; !ok && env.Type != "" {
		payload["action"] = env.Type
	}

	// Classify the outcome so the orchestrator does not have to infer lifecycle
	// state from an action-outcome string.
	if _, ok := payload["phase"]; !ok {
		switch payload["status"] {
		case "success", "completed":
			payload["phase"] = "completed"
		case "failed", "error":
			payload["phase"] = "failed"
		default:
			payload["phase"] = "ack"
		}
	}

	return payload
}

// normaliseMemoryLimit corrects a memory limit that was sent in megabytes where
// bytes were expected.
//
// Docker rejects any limit below 6MB, so a value in that range is never a
// legitimate request — it is always a caller that skipped the MB→bytes
// conversion. That mismatch made every database create fail: a 512MB request
// arrived as 512 bytes and Docker refused the container outright.
func normaliseMemoryLimit(limit int64) int64 {
	const dockerMinimumBytes = 6 * 1024 * 1024
	if limit > 0 && limit < dockerMinimumBytes {
		return limit * 1024 * 1024
	}
	return limit
}

// scheduledEnv synthesises an envelope for replies produced by the cron
// scheduler rather than by an incoming command, so scheduled snapshots are
// correlated to their instance the same way manual ones are.
func scheduledEnv(instanceID int) client.Envelope {
	return client.Envelope{
		Type: "db_snapshot",
		Payload: map[string]any{
			"database_instance_id": instanceID,
		},
	}
}

// sendLifecycleLog sends a lifecycle entry to the orchestrator and logs it at
// info, attributed to its caller. Failures the entry describes are logged by
// the caller at their own level; logging them here too would make every
// failure two issues.
func sendLifecycleLog(ctx context.Context, ws *client.WSClient, containerName, event, message string) {
	lifecycleLog(ctx, slog.LevelInfo, ws, containerName, event, message)
}

// sendLifecycleWarn is sendLifecycleLog for a handled failure (a fallback, a
// missing container) that the caller does not log itself.
func sendLifecycleWarn(ctx context.Context, ws *client.WSClient, containerName, event, message string) {
	lifecycleLog(ctx, slog.LevelWarn, ws, containerName, event, message)
}

// lifecycleCallerSkip is how far above lifecycleLog its logs are attributed:
// lifecycleLog is called only by sendLifecycleLog and sendLifecycleWarn, so
// skip 2 is their caller.
const lifecycleCallerSkip = 2

// lifecycleLog logs the entry and sends it, attributing both the entry and any
// send failure to the caller of sendLifecycleLog/sendLifecycleWarn.
func lifecycleLog(ctx context.Context, level slog.Level, ws *client.WSClient, containerName, event, message string) {
	telemetry.LogAt(ctx, lifecycleCallerSkip, level, "lifecycle event",
		"component", "lifecycle", "container", containerName, "action", event, "detail", message)
	wsSendAt(ctx, lifecycleCallerSkip, ws, "lifecycle_log", client.OutgoingMessage{
		Type: "lifecycle_log",
		Payload: map[string]any{
			"container_name": containerName,
			"event":          event,
			"message":        message,
		},
	})
}

// handleScheduledSnapshot executes a database snapshot triggered by the scheduler.
// It follows the same logic as the db_snapshot message handler.
//
// Two things differ from a manual snapshot, both deliberate. The filename is
// computed before anything can fail, so a pre-flight failure still names the
// artifact it would have produced and the control plane can record a failed
// snapshot row against it. And the reply carries no snapshot_id: there is no row
// yet, because the runner's own cron decided to run. The orchestrator keys on
// (database_instance_id, filename) instead — previously this sent a synthetic
// string id like "scheduled-5-1738…", which the orchestrator parsed to 0 and
// dropped, so no scheduled snapshot has ever been recorded.
func handleScheduledSnapshot(ctx context.Context, ws *client.WSClient, docker *dockerclient.Client, job scheduler.Job) {
	containerName := job.ContainerName
	filename := fmt.Sprintf("%s_%s_%s.sql", containerName, job.DatabaseName, time.Now().UTC().Format("20060102T150405Z"))

	// A scheduled snapshot that cannot run must say so where an operator will
	// see it. These three checks used to return after a bare log.Printf on the
	// worker: no lifecycle log, no event, no failed row, nothing in Monitor —
	// which is how a schedule that never once produced a backup went unnoticed.
	failPreflight := func(reason string) {
		telemetry.LogAt(ctx, 1, slog.LevelError, "scheduled snapshot failed preflight",
			"component", "runner", "instance_id", job.InstanceID, "container", containerName, "reason", reason)
		sendLifecycleLog(ctx, ws, containerName, "db_snapshot", "scheduled snapshot could not start: "+reason)
		sendDbReply(ctx, ws, scheduledEnv(job.InstanceID), "db_snapshot_status", map[string]any{
			"filename":       filename,
			"container_name": containerName,
			"instance_id":    job.InstanceID,
			"scheduled":      true,
			"status":         "failed",
			"error_message":  reason,
		})
	}

	if job.BackupDest == nil {
		failPreflight("no backup destination configured for this schedule")
		return
	}
	destType, ok := job.BackupDest["type"].(string)
	if !ok || destType == "" {
		failPreflight("backup destination has no type")
		return
	}
	destConfig, _ := job.BackupDest["config"].(map[string]any)
	if destConfig == nil {
		failPreflight("backup destination has no config")
		return
	}

	// WithoutCancel: a snapshot already under way runs to completion (bounded by
	// the timeout) rather than being cut off by runner shutdown.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
	defer cancel()

	sendLifecycleLog(ctx, ws, containerName, "db_snapshot", fmt.Sprintf("scheduled snapshot triggered (instance=%d)", job.InstanceID))

	sendDbReply(ctx, ws, scheduledEnv(job.InstanceID), "db_snapshot_status", map[string]any{
		"filename":       filename,
		"container_name": containerName,
		"instance_id":    job.InstanceID,
		"scheduled":      true,
		"status":         "uploading",
	})

	id, err := docker.FindContainerByName(ctx, containerName)
	if err != nil || id == "" {
		slog.ErrorContext(ctx, "scheduled snapshot failed: container not found", "component", "runner", "container", containerName, "instance_id", job.InstanceID)
		sendLifecycleLog(ctx, ws, containerName, "db_snapshot", "scheduled snapshot failed: container not found")
		sendDbReply(ctx, ws, scheduledEnv(job.InstanceID), "db_snapshot_status", map[string]any{
			"filename":       filename,
			"container_name": containerName,
			"instance_id":    job.InstanceID,
			"scheduled":      true,
			"status":         "failed",
			"error_message":  "container not found",
		})
		return
	}

	dest, err := backup.NewDestination(destType, destConfig)
	if err != nil {
		slog.ErrorContext(ctx, "scheduled snapshot failed to create backup destination", "component", "runner", "container", containerName, "instance_id", job.InstanceID, "error", err)
		sendLifecycleLog(ctx, ws, containerName, "db_snapshot", fmt.Sprintf("scheduled snapshot failed: %v", err))
		sendDbReply(ctx, ws, scheduledEnv(job.InstanceID), "db_snapshot_status", map[string]any{
			"filename":       filename,
			"container_name": containerName,
			"instance_id":    job.InstanceID,
			"scheduled":      true,
			"status":         "failed",
			"error_message":  fmt.Sprintf("failed to create backup destination: %v", err),
		})
		return
	}

	// The object key is the filename the control plane recorded, so a row and
	// its artifact can never point at different objects.
	remotePath := filename

	sendLifecycleLog(ctx, ws, containerName, "db_snapshot", fmt.Sprintf("streaming %s dump to backup destination (scheduled)…", job.Engine))
	size, err := streamSnapshot(ctx,
		func(c context.Context) (io.ReadCloser, error) {
			return docker.ExecDatabaseDump(c, id, job.Engine, job.DatabaseName, job.Username, job.Password)
		}, dest, remotePath)
	if err != nil {
		slog.ErrorContext(ctx, "scheduled snapshot failed", "component", "runner", "container", containerName, "instance_id", job.InstanceID, "error", err)
		sendLifecycleLog(ctx, ws, containerName, "db_snapshot", fmt.Sprintf("scheduled snapshot failed: %v", err))
		sendDbReply(ctx, ws, scheduledEnv(job.InstanceID), "db_snapshot_status", map[string]any{
			"filename":       filename,
			"container_name": containerName,
			"instance_id":    job.InstanceID,
			"scheduled":      true,
			"status":         "failed",
			"error_message":  err.Error(),
		})
		return
	}

	slog.InfoContext(ctx, "scheduled snapshot completed", "component", "runner", "container", containerName, "instance_id", job.InstanceID, "size_bytes", size)
	sendLifecycleLog(ctx, ws, containerName, "db_snapshot", fmt.Sprintf("scheduled snapshot completed (size=%d bytes)", size))
	sendDbReply(ctx, ws, scheduledEnv(job.InstanceID), "db_snapshot_status", map[string]any{
		"filename":       filename,
		"container_name": containerName,
		"instance_id":    job.InstanceID,
		"scheduled":      true,
		"status":         "completed",
		"size_bytes":     size,
		"remote_path":    remotePath,
	})
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func localIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return ""
}

// safeGo runs fn in a new goroutine with panic recovery. If fn panics, the
// full stack trace is logged and a worker_crash event is sent to the
// orchestrator before the process exits.
// safeGo runs a truly-unrecoverable background goroutine (the WS connect loop).
// A panic here means the worker cannot function, so it is reported as a
// worker_crash and the process exits(2) so systemd restarts a cleanly-reported
// crash. Do NOT use this for degradable telemetry loops — use safeGoResilient.
func safeGo(ws *client.WSClient, name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 8192)
				n := runtime.Stack(buf, false)
				stackStr := string(buf[:n])
				log.Printf("[%s] PANIC: %v\n%s", name, r, stackStr)
				// Before worker_crash: the orchestrator may be what is unreachable,
				// and the spool keeps this report through the exit either way.
				telemetry.ReportCrash(name, r)
				_ = ws.SendJSON(client.OutgoingMessage{
					Type: "worker_crash",
					Payload: map[string]any{
						"goroutine": name,
						"panic":     fmt.Sprintf("%v", r),
						"stack":     stackStr,
					},
				})
				ws.Drain(2 * time.Second)
				ws.Close()
				os.Exit(2)
			}
		}()
		fn()
	}()
}

// safeGoResilient runs a long-lived background loop that must survive panics
// WITHOUT taking the whole worker down. Used for the telemetry/diagnostic loops
// (log-streamer, net-monitor, heartbeat): a panic in one of those degrades a
// single subsystem, it is not grounds to kill a worker that is otherwise
// happily running containers. On panic it logs the stack, waits a short backoff,
// and restarts the loop. It does NOT emit worker_crash — that message means the
// worker is exiting, and here it is not. If fn returns normally (e.g. ctx was
// cancelled on shutdown) the goroutine stops.
func safeGoResilient(name string, fn func()) {
	go func() {
		for {
			returnedNormally := func() (ok bool) {
				defer func() {
					if r := recover(); r != nil {
						buf := make([]byte, 8192)
						n := runtime.Stack(buf, false)
						log.Printf("[%s] PANIC (recovered, restarting loop after backoff): %v\n%s", name, r, string(buf[:n]))
						telemetry.ReportPanic(name, r, map[string]any{"restarting": true})
						ok = false
					}
				}()
				fn()
				return true
			}()
			if returnedNormally {
				return
			}
			time.Sleep(2 * time.Second) // backoff to avoid a tight panic-restart loop
		}
	}()
}
