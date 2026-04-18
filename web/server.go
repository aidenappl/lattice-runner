package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
	"github.com/aidenappl/lattice-runner/metrics"
)

// Server serves a local status dashboard for the runner.
type Server struct {
	Docker     *dockerclient.Client
	WorkerName string
	StartedAt  time.Time
	Port       string
}

func (s *Server) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/containers", s.handleContainers)
	mux.HandleFunc("/api/containers/", s.handleContainerLogs)

	addr := ":" + s.Port
	log.Printf("dashboard: http://0.0.0.0%s", addr)

	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("dashboard: failed to start: %v", err)
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	m := metrics.Collect(r.Context(), s.Docker)

	dockerVersion := ""
	if s.Docker != nil {
		v, _ := s.Docker.ServerVersion(r.Context())
		dockerVersion = v
	}

	resp := map[string]any{
		"worker_name":    s.WorkerName,
		"hostname":       getHostname(),
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
		"go_version":     runtime.Version(),
		"docker_version": dockerVersion,
		"uptime_seconds": int(time.Since(s.StartedAt).Seconds()),
		"metrics":        m,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	if s.Docker == nil {
		http.Error(w, "docker not available", http.StatusServiceUnavailable)
		return
	}

	containers, err := s.Docker.ListContainers(r.Context(), "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type containerInfo struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Image   string `json:"image"`
		State   string `json:"state"`
		Status  string `json:"status"`
		Created int64  `json:"created"`
	}

	result := make([]containerInfo, 0, len(containers))
	for _, c := range containers {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		result = append(result, containerInfo{
			ID:      c.ID[:12],
			Name:    name,
			Image:   c.Image,
			State:   c.State,
			Status:  c.Status,
			Created: c.Created,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	if s.Docker == nil {
		http.Error(w, "docker not available", http.StatusServiceUnavailable)
		return
	}

	// Path: /api/containers/{id}/logs
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/containers/"), "/")
	if len(parts) < 2 || parts[1] != "logs" {
		http.NotFound(w, r)
		return
	}
	containerID := parts[0]

	tail := r.URL.Query().Get("tail")
	if tail == "" {
		tail = "100"
	}

	reader, err := s.Docker.ContainerLogs(context.Background(), containerID, tail)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer reader.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.Copy(w, reader)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, dashboardHTML)
}

func getHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
