package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
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
var Version = "v0.0.1"

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
		switch env.Type {
		case "connected":
			log.Println("connected to orchestrator")
			// Send registration info
			_ = ws.SendJSON(client.OutgoingMessage{
				Type: "registration",
				Payload: map[string]any{
					"name":            cfg.WorkerName,
					"hostname":        hostname(),
					"os":              runtime.GOOS,
					"arch":            runtime.GOARCH,
					"docker_version":  dockerVersion,
					"ip_address":      localIP(),
					"runner_version":  Version,
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
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("container %s not found", containerName)
					return
				}
				if err := docker.StopContainer(ctx, id, 30); err != nil {
					log.Printf("failed to stop %s: %v", containerName, err)
				} else {
					log.Printf("stopped container %s", containerName)
				}
			}()

		case "restart":
			go func() {
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					return
				}
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					log.Printf("container %s not found", containerName)
					return
				}
				if err := docker.RestartContainer(ctx, id, 30); err != nil {
					log.Printf("failed to restart %s: %v", containerName, err)
				} else {
					log.Printf("restarted container %s", containerName)
				}
			}()

		case "remove":
			go func() {
				containerName, _ := env.Payload["container_name"].(string)
				if containerName == "" {
					return
				}
				id, err := docker.FindContainerByName(ctx, containerName)
				if err != nil || id == "" {
					return
				}
				_ = docker.StopContainer(ctx, id, 10)
				_ = docker.RemoveContainer(ctx, id, true)
				log.Printf("removed container %s", containerName)
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
				if err := docker.PullImage(ctx, imageRef, regAuth); err != nil {
					log.Printf("failed to pull %s: %v", imageRef, err)
				} else {
					log.Printf("pulled image %s", imageRef)
				}
			}()
		}
	})

	// Start WebSocket connection in background
	go ws.Connect(ctx)

	// Heartbeat ticker
	go func() {
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
			}
		}
	}()

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
	cancel()
	ws.Close()
	log.Println("runner stopped")
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
