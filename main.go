package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aidenappl/lattice-runner/backup"
	"github.com/aidenappl/lattice-runner/client"
	"github.com/aidenappl/lattice-runner/cmd"
	"github.com/aidenappl/lattice-runner/config"
	"github.com/aidenappl/lattice-runner/deploy"
	dockerclient "github.com/aidenappl/lattice-runner/docker"
	"github.com/aidenappl/lattice-runner/metrics"
	"github.com/aidenappl/lattice-runner/scheduler"
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

// wsSend sends a JSON message over the WebSocket and logs any failure.
func wsSend(ws *client.WSClient, msgType string, payload interface{}) {
	if err := ws.SendJSON(payload); err != nil {
		log.Printf("ws send [%s] failed: %v", msgType, err)
	}
}

type deploymentRunState struct {
	DeploymentID   int
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
				log.Printf("docker connect attempt %d/30 failed: %v", i+1, pingErr)
			}
		} else {
			log.Printf("docker connect attempt %d/30 failed: %v", i+1, err)
		}
		time.Sleep(2 * time.Second)
	}
	if docker == nil {
		log.Fatal("failed to connect to Docker after 30 attempts")
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

		wsSend(ws, "deployment_progress", client.OutgoingMessage{
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
						log.Printf("exec session cleanup: removing orphaned session %s (age=%v)", id, time.Since(s.createdAt))
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
		handleScheduledSnapshot(ws, docker, job)
	})
	go snapshotScheduler.Run(ctx)

	// Handle incoming messages from orchestrator
	ws.OnMessage(func(env client.Envelope) {
		// Recover from any panic in a message handler so the WS read-pump stays
		// alive rather than crashing the whole process.
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 8192)
				n := runtime.Stack(buf, false)
				log.Printf("[message-handler] PANIC for event %q: %v\n%s", env.Type, r, string(buf[:n]))
			}
		}()
		switch env.Type {
		case "connected":
			log.Println("connected to orchestrator")
			// Send registration info
			wsSend(ws, "registration", client.OutgoingMessage{
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
				defer func() { <-handlerSem }()
				spec, err := deploy.ParseDeploymentSpec(env.Payload)
				if err != nil {
					log.Printf("invalid deploy spec: %v", err)
					wsSend(ws, "deployment_progress", client.OutgoingMessage{
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

				// Check if deployment is already in progress
				deploymentStatesMu.Lock()
				if existing, ok := deploymentStates[spec.DeploymentID]; ok && existing.Status == "deploying" {
					deploymentStatesMu.Unlock()
					log.Printf("deploy: deployment %d already in progress, ignoring duplicate", spec.DeploymentID)
					return
				}
				deploymentStatesMu.Unlock()

				attempt := 1
				if v, ok := env.Payload["attempt"].(float64); ok && int(v) > 0 {
					attempt = int(v)
				}
				maxRetries := 3
				if v, ok := env.Payload["max_retries"].(float64); ok && int(v) > 0 {
					maxRetries = int(v)
				}

				deploymentStatesMu.Lock()
				deploymentStates[spec.DeploymentID] = &deploymentRunState{
					DeploymentID:   spec.DeploymentID,
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

				wsSend(ws, "deployment_progress", client.OutgoingMessage{
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
					log.Printf("deployment failed: %v", err)
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
				depIDFloat, _ := env.Payload["deployment_id"].(float64)
				depID := int(depIDFloat)

				deploymentStatesMu.RLock()
				st, ok := deploymentStates[depID]
				deploymentStatesMu.RUnlock()

				if !ok {
					wsSend(ws, "deployment_status", client.OutgoingMessage{
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

				wsSend(ws, "deployment_status", client.OutgoingMessage{
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
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					wsSend(ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "stop", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					log.Printf("stop: invalid container name rejected: %q", containerName)
					wsSend(ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "stop", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "stop", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "stop", "container not found")
					wsSend(ws, "container_status", client.OutgoingMessage{
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
				sendLifecycleLog(ws, containerName, "stop", fmt.Sprintf("stopping container (timeout=30s, id=%s)…", id[:12]))
				if err := docker.StopContainer(ctx, id, 30); err != nil {
					log.Printf("failed to stop %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "stop", fmt.Sprintf("failed to stop: %v", err))
					wsSend(ws, "container_status", client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "stop",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					log.Printf("stopped container %s", containerName)
					wsSend(ws, "container_status", client.OutgoingMessage{
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
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					wsSend(ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "start", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					log.Printf("start: invalid container name rejected: %q", containerName)
					wsSend(ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "start", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "start", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "start", "container not found")
					wsSend(ws, "container_status", client.OutgoingMessage{
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
				sendLifecycleLog(ws, containerName, "start", fmt.Sprintf("starting container (id=%s)…", id[:12]))
				if err := docker.StartContainer(ctx, id); err != nil {
					log.Printf("failed to start %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "start", fmt.Sprintf("failed to start: %v", err))
					wsSend(ws, "container_status", client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "start",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					log.Printf("started container %s", containerName)
					wsSend(ws, "container_status", client.OutgoingMessage{
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
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "kill", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					log.Printf("kill: invalid container name rejected: %q", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "kill", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "kill", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "kill", "container not found")
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
				sendLifecycleLog(ws, containerName, "kill", fmt.Sprintf("sending SIGKILL to container (id=%s)…", id[:12]))
				if err := docker.KillContainer(ctx, id); err != nil {
					log.Printf("failed to kill %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "kill", fmt.Sprintf("failed to kill: %v", err))
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
					log.Printf("killed container %s", containerName)
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
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "pause", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					log.Printf("pause: invalid container name rejected: %q", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "pause", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "pause", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "pause", "container not found")
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
				sendLifecycleLog(ws, containerName, "pause", fmt.Sprintf("pausing container (id=%s)…", id[:12]))
				if err := docker.PauseContainer(ctx, id); err != nil {
					log.Printf("failed to pause %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "pause", fmt.Sprintf("failed to pause: %v", err))
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
					log.Printf("paused container %s", containerName)
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
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "unpause", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					log.Printf("unpause: invalid container name rejected: %q", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "unpause", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "unpause", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "unpause", "container not found")
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
				sendLifecycleLog(ws, containerName, "unpause", fmt.Sprintf("resuming container (id=%s)…", id[:12]))
				if err := docker.UnpauseContainer(ctx, id); err != nil {
					log.Printf("failed to unpause %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "unpause", fmt.Sprintf("failed to resume: %v", err))
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
					log.Printf("unpaused container %s", containerName)
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
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "restart", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					log.Printf("restart: invalid container name rejected: %q", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "restart", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "restart", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "restart", "container not found")
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
				sendLifecycleLog(ws, containerName, "restart", fmt.Sprintf("restarting container (timeout=30s, id=%s)… container will stop then start", id[:12]))
				if err := docker.RestartContainer(ctx, id, 30); err != nil {
					log.Printf("failed to restart %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "restart", fmt.Sprintf("failed to restart: %v", err))
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
					log.Printf("restarted container %s", containerName)
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
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "remove", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					log.Printf("remove: invalid container name rejected: %q", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "remove", "status": "error", "message": "invalid container_name",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "remove", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					sendLifecycleLog(ws, containerName, "remove", "container not found")
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
				sendLifecycleLog(ws, containerName, "remove", fmt.Sprintf("stopping container before removal (timeout=10s, id=%s)…", id[:12]))
				if err := docker.StopContainer(ctx, id, 10); err != nil {
					sendLifecycleLog(ws, containerName, "remove", fmt.Sprintf("stop returned: %v (proceeding with force remove)", err))
				} else {
					sendLifecycleLog(ws, containerName, "remove", "container stopped, removing…")
				}
				if err := docker.RemoveContainer(ctx, id, true); err != nil {
					log.Printf("failed to remove %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "remove", fmt.Sprintf("failed to remove: %v", err))
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
					log.Printf("removed container %s", containerName)
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
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "recreate", "status": "error", "message": "missing container_name",
						},
					})
					return
				}
				if !validContainerName(containerName) {
					log.Printf("recreate: invalid container name rejected: %q", containerName)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "recreate", "status": "error", "message": "invalid container_name",
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
					sendLifecycleLog(ws, containerName, "recreate", fmt.Sprintf("pulling image %s%s…", fullRef, authInfo))
					if err := docker.PullImage(ctx, fullRef, regAuth); err != nil {
						log.Printf("pull failed for %s: %v — proceeding with recreate anyway", fullRef, err)
						sendLifecycleLog(ws, containerName, "recreate", fmt.Sprintf("image pull failed: %v — proceeding with local image", err))
					} else {
						sendLifecycleLog(ws, containerName, "recreate", fmt.Sprintf("image %s pulled successfully", fullRef))
					}
				} else {
					sendLifecycleLog(ws, containerName, "recreate", "no image specified, recreating with current image")
				}

				sendLifecycleLog(ws, containerName, "recreate", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					// Try canonical variants (suffixed names from deploys)
					id, _ = executor.FindCanonicalContainer(ctx, containerName)
				}
				if id == "" {
					log.Printf("container %s not found for recreate", containerName)
					sendLifecycleLog(ws, containerName, "recreate", "container not found — run a stack deploy to create it")
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

				sendLifecycleLog(ws, containerName, "recreate", fmt.Sprintf("graceful recreate (old id=%s)… starting new container, health checking, then swapping", id[:12]))
				newID, err := docker.GracefulRecreate(ctx, id, newImageRef)
				if err != nil {
					log.Printf("failed to recreate %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "recreate", fmt.Sprintf("failed to recreate: %v", err))
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
				log.Printf("recreated container %s -> %s", containerName, newID)
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
				sendLifecycleLog(ws, imageRef, "pull_image", fmt.Sprintf("pulling image %s%s…", imageRef, authInfo))
				if err := docker.PullImage(ctx, imageRef, regAuth); err != nil {
					log.Printf("failed to pull %s: %v", imageRef, err)
					sendLifecycleLog(ws, imageRef, "pull_image", fmt.Sprintf("pull failed: %v", err))
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
					log.Printf("pulled image %s", imageRef)
					sendLifecycleLog(ws, imageRef, "pull_image", fmt.Sprintf("image %s pulled successfully", imageRef))
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
				// Rate-limit reboots: reject if last reboot was within 5 minutes
				lastRebootMu.Lock()
				if time.Since(lastRebootTime) < 5*time.Minute {
					lastRebootMu.Unlock()
					log.Println("reboot rejected: cooldown period (5 minutes between reboots)")
					wsSend(ws, "worker_action_status", client.OutgoingMessage{
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

				log.Println("reboot command received, rebooting system...")
				wsSend(ws, "worker_action_status", client.OutgoingMessage{
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
					log.Printf("reboot failed: %v — %s", err, string(out))
				}
			}()

		case "upgrade_runner":
			go func() {
				log.Println("upgrade runner command received")
				wsSend(ws, "worker_action_status", client.OutgoingMessage{
					Type: "worker_action_status",
					Payload: map[string]any{
						"action":  "upgrade_runner",
						"status":  "accepted",
						"message": "starting runner upgrade",
					},
				})

				// Extract expected hash from the orchestrator payload for integrity verification
				expectedHash, _ := env.Payload["expected_hash"].(string)

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
					log.Printf("upgrade: failed to create temp dir: %v", mkErr)
					wsSend(ws, "worker_action_status", client.OutgoingMessage{
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
					log.Printf("upgrade download failed: %v — %s", dlErr, string(dlOut))
					wsSend(ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "upgrade_runner",
							"status":  "failed",
							"message": fmt.Sprintf("upgrade download failed: %v", dlErr),
						},
					})
					return
				}

				// Verify SHA256 hash of the downloaded script
				scriptBytes, readErr := os.ReadFile(tmpFile)
				if readErr != nil {
					log.Printf("upgrade: failed to read downloaded script: %v", readErr)
					wsSend(ws, "worker_action_status", client.OutgoingMessage{
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
				log.Printf("upgrade script hash: %s", actualHashHex)

				if expectedHash != "" && actualHashHex != expectedHash {
					log.Printf("upgrade ABORTED: hash mismatch — expected %s, got %s", expectedHash, actualHashHex)
					wsSend(ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "upgrade_runner",
							"status":  "failed",
							"message": "upgrade aborted: script integrity check failed (hash mismatch)",
						},
					})
					return
				}
				if expectedHash == "" {
					log.Println("upgrade WARNING: no expected_hash provided by orchestrator, skipping verification")
				}

				// Make executable and run
				_ = os.Chmod(tmpFile, 0755)
				out, err := exec.CommandContext(ctx, "bash", tmpFile).CombinedOutput()
				if err != nil {
					log.Printf("upgrade failed: %v — %s", err, string(out))
					// Include truncated script output so the dashboard shows the real error
					scriptOutput := string(out)
					if len(scriptOutput) > 1000 {
						scriptOutput = scriptOutput[len(scriptOutput)-1000:]
					}
					failMsg := fmt.Sprintf("upgrade failed: %v", err)
					if scriptOutput != "" {
						failMsg = fmt.Sprintf("upgrade failed: %v\n%s", err, scriptOutput)
					}
					wsSend(ws, "worker_action_status", client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "upgrade_runner",
							"status":  "failed",
							"message": failMsg,
						},
					})
				} else {
					log.Printf("upgrade completed: %s", string(out))
					wsSend(ws, "worker_action_status", client.OutgoingMessage{
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
				log.Println("stop all containers command received")
				containers, err := docker.ListContainers(ctx, "")
				if err != nil {
					log.Printf("failed to list containers: %v", err)
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
				log.Printf("stop_all: found %d running containers out of %d total", running, len(containers))
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
							sendLifecycleLog(ws, name, "stop", fmt.Sprintf("stopping container as part of stop_all (%d/%d)…", stopped+failed+1, running))
						}
						if err := docker.StopContainer(ctx, c.ID, 30); err != nil {
							log.Printf("failed to stop %s: %v", c.ID[:12], err)
							failed++
						} else {
							stopped++
						}
					}
				}
				log.Printf("stop_all complete: stopped=%d failed=%d", stopped, failed)
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
				log.Println("start all containers command received")
				containers, err := docker.ListContainers(ctx, "")
				if err != nil {
					log.Printf("failed to list containers: %v", err)
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
				log.Printf("start_all: found %d stopped containers out of %d total", notRunning, len(containers))
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
							sendLifecycleLog(ws, name, "start", fmt.Sprintf("starting container as part of start_all (%d/%d)…", started+failed+1, notRunning))
						}
						if err := docker.StartContainer(ctx, c.ID); err != nil {
							log.Printf("failed to start %s: %v", c.ID[:12], err)
							failed++
						} else {
							started++
						}
					}
				}
				log.Printf("start_all complete: started=%d failed=%d", started, failed)
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
				volumes, err := docker.ListVolumes(ctx)
				if err != nil {
					log.Printf("failed to list volumes: %v", err)
					_ = ws.SendJSON(client.OutgoingMessage{
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
				_ = ws.SendJSON(client.OutgoingMessage{
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
				networks, err := docker.ListNetworks(ctx)
				if err != nil {
					log.Printf("failed to list networks: %v", err)
					_ = ws.SendJSON(client.OutgoingMessage{
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
				_ = ws.SendJSON(client.OutgoingMessage{
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
					log.Printf("force_remove: invalid container name rejected: %q", containerName)
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
				log.Printf("force_remove: stopping and removing %s (id=%s)", containerName, id[:12])
				_ = docker.StopContainer(ctx, id, 5)
				if err := docker.RemoveContainer(ctx, id, true); err != nil {
					log.Printf("force_remove: failed to remove %s: %v", containerName, err)
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action": "force_remove", "status": "failed", "message": fmt.Sprintf("failed to remove: %v", err),
						},
					})
					return
				}
				log.Printf("force_remove: removed %s", containerName)
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
				containerName, _ := env.Payload["container_name"].(string)
				commandID := env.CommandID
				if containerName == "" || commandID == "" {
					return
				}
				if !validContainerName(containerName) {
					log.Printf("exec_start: invalid container name: %s", containerName)
					return
				}
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					_ = ws.SendJSON(client.OutgoingMessage{
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
					_ = ws.SendJSON(client.OutgoingMessage{
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
					_ = ws.SendJSON(client.OutgoingMessage{
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
					defer func() {
						conn.Close()
						execMu.Lock()
						delete(execSessions, commandID)
						execMu.Unlock()
						_ = ws.SendJSON(client.OutgoingMessage{
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
							_ = ws.SendJSON(client.OutgoingMessage{
								Type: "exec_output",
								Payload: map[string]any{
									"command_id": commandID,
									"data":       base64.StdEncoding.EncodeToString(buf[:n]),
								},
							})
						}
						if err != nil {
							if err != io.EOF {
								log.Printf("exec read error for %s: %v", commandID, err)
							}
							return
						}
					}
				}()
			}()

		case "exec_input":
			go func() {
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

		case "db_create":
			handlerSem <- struct{}{}
			go func() {
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" || !validContainerName(containerName) {
					log.Printf("db_create: invalid or empty container name: %q", containerName)
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"action": "db_create", "status": "error", "message": "invalid or missing container_name",
						},
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

				if volumeName == "" {
					volumeName = containerName + "-data"
				}

				spec := dockerclient.DatabaseSpec{
					ContainerName: containerName,
					VolumeName:    volumeName,
					Engine:        engine,
					EngineVersion: engineVersion,
					Port:          int(portF),
					RootPassword:  rootPassword,
					DatabaseName:  databaseName,
					Username:      username,
					Password:      password,
					CPULimit:      cpuLimitF,
					MemoryLimit:   int64(memoryLimitF),
				}

				sendLifecycleLog(ws, containerName, "db_create", fmt.Sprintf("creating %s:%s database container…", engine, engineVersion))
				containerID, err := docker.CreateDatabaseContainer(ctx, spec)
				if err != nil {
					log.Printf("db_create: failed to create %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "db_create", fmt.Sprintf("failed to create: %v", err))
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "db_create",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
					return
				}

				log.Printf("db_create: created database container %s (id=%s)", containerName, containerID[:12])
				sendLifecycleLog(ws, containerName, "db_create", fmt.Sprintf("database container created and started (id=%s)", containerID[:12]))
				wsSend(ws, "db_status", client.OutgoingMessage{
					Type: "db_status",
					Payload: map[string]any{
						"container_name": containerName,
						"action":         "db_create",
						"status":         "success",
						"container_id":   containerID,
					},
				})
			}()

		case "db_start":
			handlerSem <- struct{}{}
			go func() {
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" || !validContainerName(containerName) {
					log.Printf("db_start: invalid or empty container name: %q", containerName)
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"action": "db_start", "status": "error", "message": "invalid or missing container_name",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "db_start", "looking up database container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("db_start: container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "db_start", "container not found")
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "db_start",
							"status":         "failed",
							"message":        "container not found",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "db_start", fmt.Sprintf("starting database container (id=%s)…", id[:12]))
				if err := docker.StartContainer(ctx, id); err != nil {
					log.Printf("db_start: failed to start %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "db_start", fmt.Sprintf("failed to start: %v", err))
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "db_start",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					log.Printf("db_start: started database container %s", containerName)
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "db_start",
							"status":         "success",
						},
					})
				}
			}()

		case "db_stop":
			handlerSem <- struct{}{}
			go func() {
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" || !validContainerName(containerName) {
					log.Printf("db_stop: invalid or empty container name: %q", containerName)
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"action": "db_stop", "status": "error", "message": "invalid or missing container_name",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "db_stop", "looking up database container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("db_stop: container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "db_stop", "container not found")
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "db_stop",
							"status":         "failed",
							"message":        "container not found",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "db_stop", fmt.Sprintf("stopping database container (timeout=30s, id=%s)…", id[:12]))
				if err := docker.StopContainer(ctx, id, 30); err != nil {
					log.Printf("db_stop: failed to stop %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "db_stop", fmt.Sprintf("failed to stop: %v", err))
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "db_stop",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					log.Printf("db_stop: stopped database container %s", containerName)
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "db_stop",
							"status":         "success",
						},
					})
				}
			}()

		case "db_restart":
			handlerSem <- struct{}{}
			go func() {
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" || !validContainerName(containerName) {
					log.Printf("db_restart: invalid or empty container name: %q", containerName)
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"action": "db_restart", "status": "error", "message": "invalid or missing container_name",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "db_restart", "looking up database container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("db_restart: container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "db_restart", "container not found")
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "db_restart",
							"status":         "failed",
							"message":        "container not found",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "db_restart", fmt.Sprintf("restarting database container (timeout=30s, id=%s)…", id[:12]))
				if err := docker.RestartContainer(ctx, id, 30); err != nil {
					log.Printf("db_restart: failed to restart %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "db_restart", fmt.Sprintf("failed to restart: %v", err))
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "db_restart",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					log.Printf("db_restart: restarted database container %s", containerName)
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "db_restart",
							"status":         "success",
						},
					})
				}
			}()

		case "db_remove":
			handlerSem <- struct{}{}
			go func() {
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" || !validContainerName(containerName) {
					log.Printf("db_remove: invalid or empty container name: %q", containerName)
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"action": "db_remove", "status": "error", "message": "invalid or missing container_name",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "db_remove", "looking up database container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("db_remove: container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "db_remove", "container not found")
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "db_remove",
							"status":         "failed",
							"message":        "container not found",
						},
					})
					return
				}
				sendLifecycleLog(ws, containerName, "db_remove", fmt.Sprintf("stopping database container before removal (timeout=10s, id=%s)…", id[:12]))
				if err := docker.StopContainer(ctx, id, 10); err != nil {
					sendLifecycleLog(ws, containerName, "db_remove", fmt.Sprintf("stop returned: %v (proceeding with remove)", err))
				} else {
					sendLifecycleLog(ws, containerName, "db_remove", "container stopped, removing…")
				}
				if err := docker.RemoveContainer(ctx, id, true); err != nil {
					log.Printf("db_remove: failed to remove %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "db_remove", fmt.Sprintf("failed to remove: %v", err))
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "db_remove",
							"status":         "failed",
							"message":        err.Error(),
						},
					})
				} else {
					log.Printf("db_remove: removed database container %s (volume preserved)", containerName)
					sendLifecycleLog(ws, containerName, "db_remove", "database container removed (volume preserved)")
					wsSend(ws, "db_status", client.OutgoingMessage{
						Type: "db_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "db_remove",
							"status":         "success",
						},
					})
				}
			}()

		case "db_snapshot":
			handlerSem <- struct{}{}
			go func() {
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				engine, _ := env.Payload["engine"].(string)
				databaseName, _ := env.Payload["database_name"].(string)
				username, _ := env.Payload["username"].(string)
				password, _ := env.Payload["password"].(string)
				snapshotID, _ := env.Payload["snapshot_id"].(string)
				remotePath, _ := env.Payload["remote_path"].(string)
				destType, _ := env.Payload["dest_type"].(string)
				destConfig, _ := env.Payload["dest_config"].(map[string]any)

				if containerName == "" || !validContainerName(containerName) {
					log.Printf("db_snapshot: invalid or empty container name: %q", containerName)
					wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
						Type: "db_snapshot_status",
						Payload: map[string]any{
							"snapshot_id":    snapshotID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  "invalid or missing container_name",
						},
					})
					return
				}
				if engine == "" || databaseName == "" {
					wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
						Type: "db_snapshot_status",
						Payload: map[string]any{
							"snapshot_id":    snapshotID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  "missing required fields (engine, database_name)",
						},
					})
					return
				}

				// Send uploading status
				wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
					Type: "db_snapshot_status",
					Payload: map[string]any{
						"snapshot_id":    snapshotID,
						"container_name": containerName,
						"status":         "uploading",
					},
				})

				sendLifecycleLog(ws, containerName, "db_snapshot", "looking up database container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("db_snapshot: container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "db_snapshot", "container not found")
					wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
						Type: "db_snapshot_status",
						Payload: map[string]any{
							"snapshot_id":    snapshotID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  "container not found",
						},
					})
					return
				}

				sendLifecycleLog(ws, containerName, "db_snapshot", fmt.Sprintf("executing %s dump…", engine))
				dumpReader, err := docker.ExecDatabaseDump(ctx, id, engine, databaseName, username, password)
				if err != nil {
					log.Printf("db_snapshot: dump failed for %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "db_snapshot", fmt.Sprintf("dump failed: %v", err))
					wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
						Type: "db_snapshot_status",
						Payload: map[string]any{
							"snapshot_id":    snapshotID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  fmt.Sprintf("dump failed: %v", err),
						},
					})
					return
				}

				// Write dump to temp file
				tmpDir, err := os.MkdirTemp("", "lattice-snapshot-*")
				if err != nil {
					log.Printf("db_snapshot: failed to create temp dir: %v", err)
					wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
						Type: "db_snapshot_status",
						Payload: map[string]any{
							"snapshot_id":    snapshotID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  fmt.Sprintf("failed to create temp dir: %v", err),
						},
					})
					return
				}
				defer os.RemoveAll(tmpDir)

				tmpFile := filepath.Join(tmpDir, "dump.sql")
				f, err := os.Create(tmpFile)
				if err != nil {
					log.Printf("db_snapshot: failed to create temp file: %v", err)
					wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
						Type: "db_snapshot_status",
						Payload: map[string]any{
							"snapshot_id":    snapshotID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  fmt.Sprintf("failed to create temp file: %v", err),
						},
					})
					return
				}
				if _, err := io.Copy(f, dumpReader); err != nil {
					f.Close()
					log.Printf("db_snapshot: failed to write dump: %v", err)
					wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
						Type: "db_snapshot_status",
						Payload: map[string]any{
							"snapshot_id":    snapshotID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  fmt.Sprintf("failed to write dump: %v", err),
						},
					})
					return
				}
				f.Close()

				// Create backup destination and upload
				sendLifecycleLog(ws, containerName, "db_snapshot", "uploading snapshot to backup destination…")
				dest, err := backup.NewDestination(destType, destConfig)
				if err != nil {
					log.Printf("db_snapshot: failed to create backup destination: %v", err)
					wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
						Type: "db_snapshot_status",
						Payload: map[string]any{
							"snapshot_id":    snapshotID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  fmt.Sprintf("failed to create backup destination: %v", err),
						},
					})
					return
				}

				size, err := dest.Upload(ctx, tmpFile, remotePath)
				if err != nil {
					log.Printf("db_snapshot: upload failed for %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "db_snapshot", fmt.Sprintf("upload failed: %v", err))
					wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
						Type: "db_snapshot_status",
						Payload: map[string]any{
							"snapshot_id":    snapshotID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  fmt.Sprintf("upload failed: %v", err),
						},
					})
					return
				}

				log.Printf("db_snapshot: snapshot completed for %s (size=%d bytes)", containerName, size)
				sendLifecycleLog(ws, containerName, "db_snapshot", fmt.Sprintf("snapshot completed (size=%d bytes)", size))
				wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
					Type: "db_snapshot_status",
					Payload: map[string]any{
						"snapshot_id":    snapshotID,
						"container_name": containerName,
						"status":         "completed",
						"size_bytes":     size,
					},
				})
			}()

		case "db_restore":
			handlerSem <- struct{}{}
			go func() {
				defer func() { <-handlerSem }()
				containerName, _ := env.Payload["container_name"].(string)
				engine, _ := env.Payload["engine"].(string)
				databaseName, _ := env.Payload["database_name"].(string)
				username, _ := env.Payload["username"].(string)
				password, _ := env.Payload["password"].(string)
				restoreID, _ := env.Payload["restore_id"].(string)
				remotePath, _ := env.Payload["remote_path"].(string)
				destType, _ := env.Payload["dest_type"].(string)
				destConfig, _ := env.Payload["dest_config"].(map[string]any)

				if containerName == "" || !validContainerName(containerName) {
					log.Printf("db_restore: invalid or empty container name: %q", containerName)
					wsSend(ws, "db_restore_status", client.OutgoingMessage{
						Type: "db_restore_status",
						Payload: map[string]any{
							"restore_id":     restoreID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  "invalid or missing container_name",
						},
					})
					return
				}
				if engine == "" || databaseName == "" {
					wsSend(ws, "db_restore_status", client.OutgoingMessage{
						Type: "db_restore_status",
						Payload: map[string]any{
							"restore_id":     restoreID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  "missing required fields (engine, database_name)",
						},
					})
					return
				}

				// Send downloading status
				wsSend(ws, "db_restore_status", client.OutgoingMessage{
					Type: "db_restore_status",
					Payload: map[string]any{
						"restore_id":     restoreID,
						"container_name": containerName,
						"status":         "downloading",
					},
				})

				sendLifecycleLog(ws, containerName, "db_restore", "downloading snapshot from backup destination…")
				dest, err := backup.NewDestination(destType, destConfig)
				if err != nil {
					log.Printf("db_restore: failed to create backup destination: %v", err)
					wsSend(ws, "db_restore_status", client.OutgoingMessage{
						Type: "db_restore_status",
						Payload: map[string]any{
							"restore_id":     restoreID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  fmt.Sprintf("failed to create backup destination: %v", err),
						},
					})
					return
				}

				tmpDir, err := os.MkdirTemp("", "lattice-restore-*")
				if err != nil {
					log.Printf("db_restore: failed to create temp dir: %v", err)
					wsSend(ws, "db_restore_status", client.OutgoingMessage{
						Type: "db_restore_status",
						Payload: map[string]any{
							"restore_id":     restoreID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  fmt.Sprintf("failed to create temp dir: %v", err),
						},
					})
					return
				}
				defer os.RemoveAll(tmpDir)

				tmpFile := filepath.Join(tmpDir, "restore.sql")
				if err := dest.Download(ctx, remotePath, tmpFile); err != nil {
					log.Printf("db_restore: download failed: %v", err)
					sendLifecycleLog(ws, containerName, "db_restore", fmt.Sprintf("download failed: %v", err))
					wsSend(ws, "db_restore_status", client.OutgoingMessage{
						Type: "db_restore_status",
						Payload: map[string]any{
							"restore_id":     restoreID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  fmt.Sprintf("download failed: %v", err),
						},
					})
					return
				}

				sendLifecycleLog(ws, containerName, "db_restore", "looking up database container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("db_restore: container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "db_restore", "container not found")
					wsSend(ws, "db_restore_status", client.OutgoingMessage{
						Type: "db_restore_status",
						Payload: map[string]any{
							"restore_id":     restoreID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  "container not found",
						},
					})
					return
				}

				sendLifecycleLog(ws, containerName, "db_restore", fmt.Sprintf("restoring %s database…", engine))
				restoreFile, err := os.Open(tmpFile)
				if err != nil {
					log.Printf("db_restore: failed to open temp file: %v", err)
					wsSend(ws, "db_restore_status", client.OutgoingMessage{
						Type: "db_restore_status",
						Payload: map[string]any{
							"restore_id":     restoreID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  fmt.Sprintf("failed to open restore file: %v", err),
						},
					})
					return
				}
				defer restoreFile.Close()

				if err := docker.ExecDatabaseRestore(ctx, id, engine, databaseName, username, password, restoreFile); err != nil {
					log.Printf("db_restore: restore failed for %s: %v", containerName, err)
					sendLifecycleLog(ws, containerName, "db_restore", fmt.Sprintf("restore failed: %v", err))
					wsSend(ws, "db_restore_status", client.OutgoingMessage{
						Type: "db_restore_status",
						Payload: map[string]any{
							"restore_id":     restoreID,
							"container_name": containerName,
							"status":         "failed",
							"error_message":  fmt.Sprintf("restore failed: %v", err),
						},
					})
					return
				}

				log.Printf("db_restore: restore completed for %s", containerName)
				sendLifecycleLog(ws, containerName, "db_restore", "database restore completed")
				wsSend(ws, "db_restore_status", client.OutgoingMessage{
					Type: "db_restore_status",
					Payload: map[string]any{
						"restore_id":     restoreID,
						"container_name": containerName,
						"status":         "completed",
					},
				})
			}()

		case "db_update_schedule":
			handlerSem <- struct{}{}
			go func() {
				defer func() { <-handlerSem }()
				instanceIDFloat, _ := env.Payload["instance_id"].(float64)
				instanceID := int(instanceIDFloat)
				enabled, _ := env.Payload["enabled"].(bool)

				if !enabled {
					snapshotScheduler.RemoveSchedule(instanceID)
					log.Printf("db_update_schedule: removed schedule for instance %d", instanceID)
					wsSend(ws, "db_schedule_status", client.OutgoingMessage{
						Type: "db_schedule_status",
						Payload: map[string]any{
							"instance_id": instanceID,
							"status":      "removed",
						},
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
				backupDest, _ := env.Payload["backup_dest"].(map[string]any)

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

				log.Printf("db_update_schedule: updated schedule for instance %d (cron=%s)", instanceID, cron)
				wsSend(ws, "db_schedule_status", client.OutgoingMessage{
					Type: "db_schedule_status",
					Payload: map[string]any{
						"instance_id": instanceID,
						"status":      "updated",
						"cron":        cron,
					},
				})
			}()

		case "backup_dest_test":
			handlerSem <- struct{}{}
			go func() {
				defer func() { <-handlerSem }()
				destType, _ := env.Payload["dest_type"].(string)
				destConfig, _ := env.Payload["dest_config"].(map[string]any)

				dest, err := backup.NewDestination(destType, destConfig)
				if err != nil {
					log.Printf("backup_dest_test: failed to create destination: %v", err)
					wsSend(ws, "backup_dest_test_result", client.OutgoingMessage{
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
					log.Printf("backup_dest_test: test failed: %v", err)
					wsSend(ws, "backup_dest_test_result", client.OutgoingMessage{
						Type: "backup_dest_test_result",
						Payload: map[string]any{
							"command_id": env.CommandID,
							"status":     "failed",
							"message":    fmt.Sprintf("connection test failed: %v", err),
						},
					})
					return
				}

				log.Printf("backup_dest_test: test passed for %s destination", destType)
				wsSend(ws, "backup_dest_test_result", client.OutgoingMessage{
					Type: "backup_dest_test_result",
					Payload: map[string]any{
						"command_id": env.CommandID,
						"status":     "success",
						"message":    "connection test passed",
					},
				})
			}()

		case "db_delete_snapshot_file":
			handlerSem <- struct{}{}
			go func() {
				defer func() { <-handlerSem }()
				destType, _ := env.Payload["dest_type"].(string)
				destConfig, _ := env.Payload["dest_config"].(map[string]any)
				remotePath, _ := env.Payload["remote_path"].(string)
				snapshotID, _ := env.Payload["snapshot_id"].(string)

				if remotePath == "" {
					wsSend(ws, "db_delete_snapshot_result", client.OutgoingMessage{
						Type: "db_delete_snapshot_result",
						Payload: map[string]any{
							"snapshot_id": snapshotID,
							"status":     "failed",
							"message":    "missing remote_path",
						},
					})
					return
				}

				dest, err := backup.NewDestination(destType, destConfig)
				if err != nil {
					log.Printf("db_delete_snapshot_file: failed to create destination: %v", err)
					wsSend(ws, "db_delete_snapshot_result", client.OutgoingMessage{
						Type: "db_delete_snapshot_result",
						Payload: map[string]any{
							"snapshot_id": snapshotID,
							"status":     "failed",
							"message":    fmt.Sprintf("failed to create destination: %v", err),
						},
					})
					return
				}

				if err := dest.Delete(ctx, remotePath); err != nil {
					log.Printf("db_delete_snapshot_file: delete failed: %v", err)
					wsSend(ws, "db_delete_snapshot_result", client.OutgoingMessage{
						Type: "db_delete_snapshot_result",
						Payload: map[string]any{
							"snapshot_id": snapshotID,
							"status":     "failed",
							"message":    fmt.Sprintf("delete failed: %v", err),
						},
					})
					return
				}

				log.Printf("db_delete_snapshot_file: deleted %s", remotePath)
				wsSend(ws, "db_delete_snapshot_result", client.OutgoingMessage{
					Type: "db_delete_snapshot_result",
					Payload: map[string]any{
						"snapshot_id": snapshotID,
						"status":     "success",
					},
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
	safeGo(ws, "log-streamer", func() { logStreamer.Run(ctx) })

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
	safeGo(ws, "net-monitor", func() {
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
	safeGo(ws, "heartbeat", func() {
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

				wsSend(ws, "heartbeat", client.OutgoingMessage{
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

	log.Println("shutting down gracefully...")

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
		log.Println("waiting for in-flight deployment to complete...")
		time.Sleep(2 * time.Second)
	}

	wsSend(ws, "worker_shutdown", client.OutgoingMessage{
		Type: "worker_shutdown",
		Payload: map[string]any{
			"reason":  "graceful",
			"message": "runner shutting down gracefully",
		},
	})
	cancel() // signal all goroutines to stop
	time.Sleep(2 * time.Second) // let in-flight work finish
	ws.Drain(5 * time.Second)   // drain remaining messages
	ws.Close()
	log.Println("runner stopped")
}

// sendLifecycleLog sends a verbose lifecycle log entry to the orchestrator so
// it gets persisted in the lifecycle_logs table and broadcast to the admin UI.
func sendLifecycleLog(ws *client.WSClient, containerName, event, message string) {
	log.Printf("[lifecycle] %s: %s — %s", containerName, event, message)
	wsSend(ws, "lifecycle_log", client.OutgoingMessage{
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
func handleScheduledSnapshot(ws *client.WSClient, docker *dockerclient.Client, job scheduler.Job) {
	if job.BackupDest == nil {
		log.Printf("scheduled snapshot for instance %d: no backup destination configured", job.InstanceID)
		return
	}

	destType, ok := job.BackupDest["type"].(string)
	if !ok || destType == "" {
		log.Printf("scheduled snapshot for instance %d: missing backup destination type", job.InstanceID)
		return
	}
	destConfig, _ := job.BackupDest["config"].(map[string]any)
	if destConfig == nil {
		log.Printf("scheduled snapshot for instance %d: missing backup destination config", job.InstanceID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	containerName := job.ContainerName
	snapshotID := fmt.Sprintf("scheduled-%d-%d", job.InstanceID, time.Now().Unix())

	sendLifecycleLog(ws, containerName, "db_snapshot", fmt.Sprintf("scheduled snapshot triggered (instance=%d)", job.InstanceID))

	wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
		Type: "db_snapshot_status",
		Payload: map[string]any{
			"snapshot_id":    snapshotID,
			"container_name": containerName,
			"instance_id":    job.InstanceID,
			"scheduled":      true,
			"status":         "uploading",
		},
	})

	id, err := docker.FindContainerByName(ctx, containerName)
	if err != nil || id == "" {
		log.Printf("scheduled snapshot: container %s not found", containerName)
		sendLifecycleLog(ws, containerName, "db_snapshot", "scheduled snapshot failed: container not found")
		wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
			Type: "db_snapshot_status",
			Payload: map[string]any{
				"snapshot_id":    snapshotID,
				"container_name": containerName,
				"instance_id":    job.InstanceID,
				"scheduled":      true,
				"status":         "failed",
				"error_message":  "container not found",
			},
		})
		return
	}

	sendLifecycleLog(ws, containerName, "db_snapshot", fmt.Sprintf("executing %s dump (scheduled)…", job.Engine))
	dumpReader, err := docker.ExecDatabaseDump(ctx, id, job.Engine, job.DatabaseName, job.Username, job.Password)
	if err != nil {
		log.Printf("scheduled snapshot: dump failed for %s: %v", containerName, err)
		sendLifecycleLog(ws, containerName, "db_snapshot", fmt.Sprintf("scheduled dump failed: %v", err))
		wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
			Type: "db_snapshot_status",
			Payload: map[string]any{
				"snapshot_id":    snapshotID,
				"container_name": containerName,
				"instance_id":    job.InstanceID,
				"scheduled":      true,
				"status":         "failed",
				"error_message":  fmt.Sprintf("dump failed: %v", err),
			},
		})
		return
	}

	// Write dump to temp file
	tmpDir, err := os.MkdirTemp("", "lattice-scheduled-snapshot-*")
	if err != nil {
		log.Printf("scheduled snapshot: failed to create temp dir: %v", err)
		wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
			Type: "db_snapshot_status",
			Payload: map[string]any{
				"snapshot_id":    snapshotID,
				"container_name": containerName,
				"instance_id":    job.InstanceID,
				"scheduled":      true,
				"status":         "failed",
				"error_message":  fmt.Sprintf("failed to create temp dir: %v", err),
			},
		})
		return
	}
	defer os.RemoveAll(tmpDir)

	tmpFile := filepath.Join(tmpDir, "dump.sql")
	f, err := os.Create(tmpFile)
	if err != nil {
		log.Printf("scheduled snapshot: failed to create temp file: %v", err)
		wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
			Type: "db_snapshot_status",
			Payload: map[string]any{
				"snapshot_id":    snapshotID,
				"container_name": containerName,
				"instance_id":    job.InstanceID,
				"scheduled":      true,
				"status":         "failed",
				"error_message":  fmt.Sprintf("failed to create temp file: %v", err),
			},
		})
		return
	}
	if _, err := io.Copy(f, dumpReader); err != nil {
		f.Close()
		log.Printf("scheduled snapshot: failed to write dump: %v", err)
		wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
			Type: "db_snapshot_status",
			Payload: map[string]any{
				"snapshot_id":    snapshotID,
				"container_name": containerName,
				"instance_id":    job.InstanceID,
				"scheduled":      true,
				"status":         "failed",
				"error_message":  fmt.Sprintf("failed to write dump: %v", err),
			},
		})
		return
	}
	f.Close()

	// Determine remote path
	remotePath := fmt.Sprintf("%s/%s-%s.sql", containerName, job.DatabaseName, time.Now().UTC().Format("20060102-150405"))

	sendLifecycleLog(ws, containerName, "db_snapshot", "uploading scheduled snapshot to backup destination…")
	dest, err := backup.NewDestination(destType, destConfig)
	if err != nil {
		log.Printf("scheduled snapshot: failed to create backup destination: %v", err)
		wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
			Type: "db_snapshot_status",
			Payload: map[string]any{
				"snapshot_id":    snapshotID,
				"container_name": containerName,
				"instance_id":    job.InstanceID,
				"scheduled":      true,
				"status":         "failed",
				"error_message":  fmt.Sprintf("failed to create backup destination: %v", err),
			},
		})
		return
	}

	size, err := dest.Upload(ctx, tmpFile, remotePath)
	if err != nil {
		log.Printf("scheduled snapshot: upload failed for %s: %v", containerName, err)
		sendLifecycleLog(ws, containerName, "db_snapshot", fmt.Sprintf("scheduled upload failed: %v", err))
		wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
			Type: "db_snapshot_status",
			Payload: map[string]any{
				"snapshot_id":    snapshotID,
				"container_name": containerName,
				"instance_id":    job.InstanceID,
				"scheduled":      true,
				"status":         "failed",
				"error_message":  fmt.Sprintf("upload failed: %v", err),
			},
		})
		return
	}

	log.Printf("scheduled snapshot: completed for %s (size=%d bytes)", containerName, size)
	sendLifecycleLog(ws, containerName, "db_snapshot", fmt.Sprintf("scheduled snapshot completed (size=%d bytes)", size))
	wsSend(ws, "db_snapshot_status", client.OutgoingMessage{
		Type: "db_snapshot_status",
		Payload: map[string]any{
			"snapshot_id":    snapshotID,
			"container_name": containerName,
			"instance_id":    job.InstanceID,
			"scheduled":      true,
			"status":         "completed",
			"size_bytes":     size,
			"remote_path":    remotePath,
		},
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
func safeGo(ws *client.WSClient, name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 8192)
				n := runtime.Stack(buf, false)
				stackStr := string(buf[:n])
				log.Printf("[%s] PANIC: %v\n%s", name, r, stackStr)
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
