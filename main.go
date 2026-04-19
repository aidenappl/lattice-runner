package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/aidenappl/lattice-runner/client"
	"github.com/aidenappl/lattice-runner/cmd"
	"github.com/aidenappl/lattice-runner/config"
	"github.com/aidenappl/lattice-runner/deploy"
	dockerclient "github.com/aidenappl/lattice-runner/docker"
	"github.com/aidenappl/lattice-runner/metrics"
	"github.com/aidenappl/lattice-runner/web"
)

// Set via -ldflags at build time: -ldflags "-X main.Version=abc1234"
var Version = "v0.1.5"

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

	// Initialize Docker client
	fmt.Print("Connecting to Docker...")
	docker, err := dockerclient.NewClient()
	if err != nil {
		log.Fatal("failed to create docker client: ", err)
	}
	defer docker.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := docker.Ping(ctx); err != nil {
		log.Fatal("docker not reachable: ", err)
	}

	dockerVersion, _ := docker.ServerVersion(ctx)
	fmt.Printf(" ✅ Done (Docker %s)\n", dockerVersion)

	// Create WebSocket client
	ws := client.NewWSClient(cfg.OrchestratorURL, cfg.WorkerToken, cfg.ReconnectInterval)

	// Create deploy executor
	executor := deploy.NewExecutor(docker, func(deploymentID int, status, message string, payload map[string]any) {
		_ = ws.SendJSON(client.OutgoingMessage{
			Type:    "deployment_progress",
			Payload: payload,
		})
	})

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
			_ = ws.SendJSON(client.OutgoingMessage{
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
			go func() {
				spec, err := deploy.ParseDeploymentSpec(env.Payload)
				if err != nil {
					log.Printf("invalid deploy spec: %v", err)
					_ = ws.SendJSON(client.OutgoingMessage{
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
				if err := executor.Execute(ctx, *spec); err != nil {
					log.Printf("deployment failed: %v", err)
				}
			}()

		case "stop":
			go func() {
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					return
				}
				sendLifecycleLog(ws, containerName, "stop", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "stop", "container not found")
					_ = ws.SendJSON(client.OutgoingMessage{
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
					_ = ws.SendJSON(client.OutgoingMessage{
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
					_ = ws.SendJSON(client.OutgoingMessage{
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
			go func() {
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					return
				}
				sendLifecycleLog(ws, containerName, "start", "looking up container…")
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("container %s not found", containerName)
					sendLifecycleLog(ws, containerName, "start", "container not found")
					_ = ws.SendJSON(client.OutgoingMessage{
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
					_ = ws.SendJSON(client.OutgoingMessage{
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
					_ = ws.SendJSON(client.OutgoingMessage{
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
			go func() {
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
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
			go func() {
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
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
			go func() {
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
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
			go func() {
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
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
			go func() {
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
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
			go func() {
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
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
					log.Printf("container %s not found for recreate", containerName)
					sendLifecycleLog(ws, containerName, "recreate", "container not found")
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "container_status",
						Payload: map[string]any{
							"container_name": containerName,
							"action":         "recreate",
							"status":         "failed",
							"message":        "container not found",
						},
					})
					return
				}

				sendLifecycleLog(ws, containerName, "recreate", fmt.Sprintf("recreating container (old id=%s)… stopping, removing, and creating new container", id[:12]))
				newID, err := docker.RecreateContainer(ctx, id, containerName)
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
			go func() {
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
				log.Println("reboot command received, rebooting system...")
				_ = ws.SendJSON(client.OutgoingMessage{
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
				_ = ws.SendJSON(client.OutgoingMessage{
					Type: "worker_action_status",
					Payload: map[string]any{
						"action":  "upgrade_runner",
						"status":  "accepted",
						"message": "starting runner upgrade",
					},
				})
				out, err := exec.Command("bash", "-c", "curl -fsSL https://lattice-api.appleby.cloud/install/runner | bash").CombinedOutput()
				if err != nil {
					log.Printf("upgrade failed: %v — %s", err, string(out))
					_ = ws.SendJSON(client.OutgoingMessage{
						Type: "worker_action_status",
						Payload: map[string]any{
							"action":  "upgrade_runner",
							"status":  "failed",
							"message": fmt.Sprintf("upgrade failed: %v", err),
						},
					})
				} else {
					log.Printf("upgrade completed: %s", string(out))
					_ = ws.SendJSON(client.OutgoingMessage{
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

	// Heartbeat ticker — also pushes live container states each tick
	safeGo(ws, "heartbeat", func() {
		ticker := time.NewTicker(cfg.HeartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m := metrics.Collect(ctx, docker)
				_ = ws.SendJSON(client.OutgoingMessage{
					Type: "heartbeat",
					Payload: map[string]any{
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
					},
				})

				// Push live container state snapshot so the orchestrator stays in sync
				// even when containers are stopped/started outside of Lattice.
				if containers, err := docker.ListContainers(ctx, ""); err == nil {
					for _, c := range containers {
						name := ""
						for _, n := range c.Names {
							trimmed := strings.TrimPrefix(n, "/")
							if trimmed != "" {
								name = trimmed
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
						case "exited", "dead":
							latticeStatus = "stopped"
						case "created", "restarting":
							latticeStatus = "pending"
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
	}
	go dashboard.Start()

	fmt.Println()
	fmt.Println("Lattice Runner ready")

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down runner...")
	_ = ws.SendJSON(client.OutgoingMessage{
		Type: "worker_shutdown",
		Payload: map[string]any{
			"reason":  "graceful",
			"message": "runner shutting down gracefully",
		},
	})
	ws.Drain(3 * time.Second)
	cancel()
	ws.Close()
	log.Println("runner stopped")
}

// sendLifecycleLog sends a verbose lifecycle log entry to the orchestrator so
// it gets persisted in the lifecycle_logs table and broadcast to the admin UI.
func sendLifecycleLog(ws *client.WSClient, containerName, event, message string) {
	log.Printf("[lifecycle] %s: %s — %s", containerName, event, message)
	_ = ws.SendJSON(client.OutgoingMessage{
		Type: "lifecycle_log",
		Payload: map[string]any{
			"container_name": containerName,
			"event":          event,
			"message":        message,
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
