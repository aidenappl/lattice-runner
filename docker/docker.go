package docker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

// Client wraps the Docker Engine API client.
type Client struct {
	cli *client.Client
}

func NewClient() (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	return &Client{cli: cli}, nil
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.cli.Ping(ctx)
	return err
}

func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	info, err := c.cli.ServerVersion(ctx)
	if err != nil {
		return "", err
	}
	return info.Version, nil
}

// HealthCheck defines a container health check configuration.
type HealthCheck struct {
	Test        []string
	Interval    string
	Timeout     string
	Retries     int
	StartPeriod string
}

// ContainerSpec defines a container to create.
type ContainerSpec struct {
	Name          string
	Image         string
	Tag           string
	PortMappings  []PortMapping
	EnvVars       map[string]string
	Volumes       map[string]string // host:container
	CPULimit      float64           // CPU cores
	MemoryLimit   int64             // bytes
	RestartPolicy string
	Command       []string
	Entrypoint    []string
	Networks       []string
	NetworkAliases []string
	HealthCheck    *HealthCheck
}

type PortMapping struct {
	HostPort      string
	ContainerPort string
	Protocol      string // tcp or udp
}

// PullImage pulls an image from a registry.
func (c *Client) PullImage(ctx context.Context, imageRef string, authConfig *RegistryAuth) error {
	opts := image.PullOptions{}
	if authConfig != nil {
		encoded, err := encodeAuthConfig(authConfig)
		if err != nil {
			return fmt.Errorf("encode auth: %w", err)
		}
		opts.RegistryAuth = encoded
	}

	reader, err := c.cli.ImagePull(ctx, imageRef, opts)
	if err != nil {
		return fmt.Errorf("image pull: %w", err)
	}
	defer reader.Close()

	// Consume the pull output
	_, err = io.Copy(io.Discard, reader)
	return err
}

type RegistryAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func encodeAuthConfig(auth *RegistryAuth) (string, error) {
	b, err := json.Marshal(auth)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// GenerateSuffix returns a 6-character lowercase alphanumeric string for unique container naming.
func GenerateSuffix() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

// StopAndRemoveContainer stops then removes a container, verifying each step.
// Returns nil if the container is fully removed.
func (c *Client) StopAndRemoveContainer(ctx context.Context, containerID string, timeout int) error {
	// Stop
	if err := c.StopContainer(ctx, containerID, timeout); err != nil {
		log.Printf("docker: stop failed for %s: %v, trying kill", containerID[:12], err)
		_ = c.KillContainer(ctx, containerID)
		time.Sleep(1 * time.Second)
	}

	// Verify stopped
	if info, err := c.InspectContainer(ctx, containerID); err == nil && info.State.Running {
		log.Printf("docker: container %s still running after stop, killing", containerID[:12])
		_ = c.KillContainer(ctx, containerID)
		time.Sleep(2 * time.Second)
	}

	// Remove with retries
	for attempt := 0; attempt < 3; attempt++ {
		if err := c.RemoveContainer(ctx, containerID, true); err == nil {
			return nil
		} else {
			log.Printf("docker: remove attempt %d for %s failed: %v", attempt+1, containerID[:12], err)
			time.Sleep(2 * time.Second)
		}
	}

	return fmt.Errorf("failed to remove container %s after 3 attempts", containerID[:12])
}

// CreateAndStartContainer creates and starts a container from the given spec.
func (c *Client) CreateAndStartContainer(ctx context.Context, spec ContainerSpec) (string, error) {
	imageRef := spec.Image
	if spec.Tag != "" {
		imageRef = spec.Image + ":" + spec.Tag
	}

	// Environment variables
	env := make([]string, 0, len(spec.EnvVars))
	for k, v := range spec.EnvVars {
		env = append(env, k+"="+v)
	}

	// Port bindings
	exposedPorts := nat.PortSet{}
	portBindings := nat.PortMap{}
	for _, pm := range spec.PortMappings {
		proto := pm.Protocol
		if proto == "" {
			proto = "tcp"
		}
		containerPort := nat.Port(pm.ContainerPort + "/" + proto)
		exposedPorts[containerPort] = struct{}{}
		portBindings[containerPort] = []nat.PortBinding{
			{HostPort: pm.HostPort},
		}
	}

	// Volume binds
	binds := make([]string, 0, len(spec.Volumes))
	for hostPath, containerPath := range spec.Volumes {
		binds = append(binds, hostPath+":"+containerPath)
	}

	// Restart policy
	restartPolicy := container.RestartPolicy{Name: container.RestartPolicyUnlessStopped}
	if spec.RestartPolicy != "" {
		restartPolicy.Name = container.RestartPolicyMode(spec.RestartPolicy)
	}

	// Resource limits
	resources := container.Resources{}
	if spec.CPULimit > 0 {
		resources.NanoCPUs = int64(spec.CPULimit * 1e9)
	}
	if spec.MemoryLimit > 0 {
		resources.Memory = spec.MemoryLimit
	}

	containerConfig := &container.Config{
		Image:        imageRef,
		Env:          env,
		ExposedPorts: exposedPorts,
		Labels:       map[string]string{"managed-by": "lattice"},
	}
	if len(spec.Command) > 0 {
		containerConfig.Cmd = spec.Command
	}
	if len(spec.Entrypoint) > 0 {
		containerConfig.Entrypoint = spec.Entrypoint
	}
	if spec.HealthCheck != nil {
		hc := &container.HealthConfig{
			Test:    spec.HealthCheck.Test,
			Retries: spec.HealthCheck.Retries,
		}
		if spec.HealthCheck.Interval != "" {
			if d, err := time.ParseDuration(spec.HealthCheck.Interval); err == nil {
				hc.Interval = d
			}
		}
		if spec.HealthCheck.Timeout != "" {
			if d, err := time.ParseDuration(spec.HealthCheck.Timeout); err == nil {
				hc.Timeout = d
			}
		}
		if spec.HealthCheck.StartPeriod != "" {
			if d, err := time.ParseDuration(spec.HealthCheck.StartPeriod); err == nil {
				hc.StartPeriod = d
			}
		}
		containerConfig.Healthcheck = hc
	}

	hostConfig := &container.HostConfig{
		PortBindings:  portBindings,
		Binds:         binds,
		RestartPolicy: restartPolicy,
		Resources:     resources,
	}

	// Build networking config — attach to specified networks at creation time
	// so the container uses Docker's internal DNS from the start.
	// When custom networks are specified, we also set NetworkMode to the first
	// network so Docker does NOT auto-attach the default bridge.
	// Network aliases (e.g. compose service names) are applied to every
	// user-defined network so other containers can resolve them by service name.
	networkConfig := &network.NetworkingConfig{}
	if len(spec.Networks) > 0 {
		networkConfig.EndpointsConfig = make(map[string]*network.EndpointSettings)
		for _, netName := range spec.Networks {
			endpoint := &network.EndpointSettings{}
			// Apply network aliases to user-defined networks (not bridge/host/none)
			if len(spec.NetworkAliases) > 0 && netName != "bridge" && netName != "host" && netName != "none" {
				endpoint.Aliases = spec.NetworkAliases
			}
			networkConfig.EndpointsConfig[netName] = endpoint
		}
		hostConfig.NetworkMode = container.NetworkMode(spec.Networks[0])
	}

	resp, err := c.cli.ContainerCreate(ctx, containerConfig, hostConfig, networkConfig, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("container create: %w", err)
	}

	// If there are multiple networks, the first was set via NetworkMode;
	// connect to the remaining ones explicitly, with aliases on user-defined networks.
	if len(spec.Networks) > 1 {
		for _, netName := range spec.Networks[1:] {
			var endpointConfig *network.EndpointSettings
			if len(spec.NetworkAliases) > 0 && netName != "bridge" && netName != "host" && netName != "none" {
				endpointConfig = &network.EndpointSettings{Aliases: spec.NetworkAliases}
			}
			if err := c.cli.NetworkConnect(ctx, netName, resp.ID, endpointConfig); err != nil {
				log.Printf("docker: failed to connect container %s to network %s: %v", spec.Name, netName, err)
			}
		}
	}

	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("container start: %w", err)
	}

	return resp.ID, nil
}

func (c *Client) StopContainer(ctx context.Context, containerID string, timeout int) error {
	t := timeout
	return c.cli.ContainerStop(ctx, containerID, container.StopOptions{Timeout: &t})
}

func (c *Client) StartContainer(ctx context.Context, containerID string) error {
	return c.cli.ContainerStart(ctx, containerID, container.StartOptions{})
}

func (c *Client) KillContainer(ctx context.Context, containerID string) error {
	return c.cli.ContainerKill(ctx, containerID, "SIGKILL")
}

func (c *Client) PauseContainer(ctx context.Context, containerID string) error {
	return c.cli.ContainerPause(ctx, containerID)
}

func (c *Client) UnpauseContainer(ctx context.Context, containerID string) error {
	return c.cli.ContainerUnpause(ctx, containerID)
}

func (c *Client) RemoveContainer(ctx context.Context, containerID string, force bool) error {
	return c.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: force})
}

func (c *Client) RenameContainer(ctx context.Context, containerID, newName string) error {
	return c.cli.ContainerRename(ctx, containerID, newName)
}

func (c *Client) RestartContainer(ctx context.Context, containerID string, timeout int) error {
	t := timeout
	return c.cli.ContainerRestart(ctx, containerID, container.StopOptions{Timeout: &t})
}

func (c *Client) InspectContainer(ctx context.Context, containerID string) (*types.ContainerJSON, error) {
	resp, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListContainers returns all containers matching the optional name filter.
func (c *Client) ListContainers(ctx context.Context, nameFilter string) ([]types.Container, error) {
	opts := container.ListOptions{All: true}
	if nameFilter != "" {
		f := filters.NewArgs()
		f.Add("name", nameFilter)
		opts.Filters = f
	}
	return c.cli.ContainerList(ctx, opts)
}

// FindContainerByName returns the container ID for a container with the given name, or empty string.
func (c *Client) FindContainerByName(ctx context.Context, name string) (string, error) {
	containers, err := c.ListContainers(ctx, name)
	if err != nil {
		return "", err
	}
	for _, ctr := range containers {
		for _, n := range ctr.Names {
			if strings.TrimPrefix(n, "/") == name {
				return ctr.ID, nil
			}
		}
	}
	return "", nil
}

// FindContainersByPrefix returns all containers whose name starts with the given prefix.
func (c *Client) FindContainersByPrefix(ctx context.Context, prefix string) ([]types.Container, error) {
	containers, err := c.ListContainers(ctx, "")
	if err != nil {
		return nil, err
	}
	var matches []types.Container
	for _, ct := range containers {
		for _, n := range ct.Names {
			if strings.HasPrefix(strings.TrimPrefix(n, "/"), prefix) {
				matches = append(matches, ct)
				break
			}
		}
	}
	return matches, nil
}

func (c *Client) ContainerLogs(ctx context.Context, containerID string, tail string) (io.ReadCloser, error) {
	return c.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tail,
	})
}

// StreamContainerLogs follows the log stream for a container.
// On the first connect (since.IsZero()) it tails the last 100 lines so
// startup logs are captured. On reconnects, since is the Docker-recorded
// timestamp of the last received line, obtained by parsing the RFC3339Nano
// prefix that Docker prepends when Timestamps=true. Using an exact Docker
// timestamp (+ 1ns) instead of a wall-clock window eliminates duplicate
// log entries on reconnect.
func (c *Client) StreamContainerLogs(ctx context.Context, containerID string, since time.Time) (io.ReadCloser, error) {
	opts := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: true, // prepend RFC3339Nano timestamp to each line
	}
	if since.IsZero() {
		opts.Tail = "100"
	} else {
		// Advance by 1 ns so Docker's >= filter excludes the last-seen line,
		// giving us zero duplicates on reconnect.
		opts.Since = since.Add(time.Nanosecond).UTC().Format(time.RFC3339Nano)
		opts.Tail = "all"
	}
	return c.cli.ContainerLogs(ctx, containerID, opts)
}

// CreateNetwork creates a Docker network.
func (c *Client) CreateNetwork(ctx context.Context, name, driver string) error {
	_, err := c.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: driver,
	})
	return err
}

func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	return c.cli.NetworkRemove(ctx, name)
}

// CreateVolume creates a Docker volume.
func (c *Client) CreateVolume(ctx context.Context, name, driver string) error {
	_, err := c.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   name,
		Driver: driver,
	})
	return err
}

func (c *Client) RemoveVolume(ctx context.Context, name string, force bool) error {
	return c.cli.VolumeRemove(ctx, name, force)
}

// ListVolumes returns all Docker volumes.
func (c *Client) ListVolumes(ctx context.Context) ([]*volume.Volume, error) {
	resp, err := c.cli.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, err
	}
	return resp.Volumes, nil
}

// ListNetworks returns all Docker networks.
func (c *Client) ListNetworks(ctx context.Context) ([]network.Summary, error) {
	return c.cli.NetworkList(ctx, network.ListOptions{})
}

// RecreateContainer stops, removes, and recreates a container with the same config.
func (c *Client) RecreateContainer(ctx context.Context, containerID string, name string) (string, error) {
	info, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}

	// Preserve network configuration from the original container
	networkConfig := &network.NetworkingConfig{}
	if info.NetworkSettings != nil && len(info.NetworkSettings.Networks) > 0 {
		networkConfig.EndpointsConfig = make(map[string]*network.EndpointSettings)
		for netName, netSettings := range info.NetworkSettings.Networks {
			networkConfig.EndpointsConfig[netName] = &network.EndpointSettings{
				IPAMConfig: netSettings.IPAMConfig,
				Aliases:    netSettings.Aliases,
			}
		}
	}

	_ = c.StopContainer(ctx, containerID, 10)
	_ = c.RemoveContainer(ctx, containerID, true)

	resp, err := c.cli.ContainerCreate(ctx, info.Config, info.HostConfig, networkConfig, nil, name)
	if err != nil {
		return "", fmt.Errorf("recreate container: %w", err)
	}

	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start recreated container: %w", err)
	}

	return resp.ID, nil
}

// GracefulRecreate performs a zero-downtime container replacement:
// 1. Creates a new container with a temporary name (without host port bindings to avoid conflicts)
// 2. Connects it to the same networks
// 3. Waits for it to be running (and healthy if healthcheck configured)
// 4. Stops and removes the old container
// 5. Renames the new container to the original name
// 6. If the original had host port bindings, stops the temp container,
//    recreates it with the full config (including ports), and starts it.
func (c *Client) GracefulRecreate(ctx context.Context, containerID string, newImage string) (string, error) {
	info, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}

	if info.Config == nil || info.HostConfig == nil {
		return "", fmt.Errorf("inspect returned incomplete container config")
	}

	originalName := strings.TrimPrefix(info.Name, "/")
	tempName := originalName + "-lattice-updating"

	// Clean up any leftover temp container from a previous attempt
	if tempID, _ := c.FindContainerByName(ctx, tempName); tempID != "" {
		_ = c.StopContainer(ctx, tempID, 5)
		_ = c.RemoveContainer(ctx, tempID, true)
	}

	// Update image if provided
	if newImage != "" {
		info.Config.Image = newImage
	}

	// Create new container WITHOUT host port bindings (to avoid conflicts)
	// Keep the network ports exposed but remove host-side bindings temporarily
	tempHostConfig := *info.HostConfig
	hasPortBindings := len(info.HostConfig.PortBindings) > 0
	if hasPortBindings {
		tempHostConfig.PortBindings = nil
	}

	// Preserve network configuration
	networkConfig := &network.NetworkingConfig{}
	if info.NetworkSettings != nil && len(info.NetworkSettings.Networks) > 0 {
		networkConfig.EndpointsConfig = make(map[string]*network.EndpointSettings)
		for netName, netSettings := range info.NetworkSettings.Networks {
			endpoint := &network.EndpointSettings{
				IPAMConfig: netSettings.IPAMConfig,
			}
			// Aliases are only supported on user-defined networks, not bridge/host/none
			if netName != "bridge" && netName != "host" && netName != "none" {
				endpoint.Aliases = append(netSettings.Aliases, originalName)
			}
			networkConfig.EndpointsConfig[netName] = endpoint
		}
	}

	// Ensure lattice label
	if info.Config.Labels == nil {
		info.Config.Labels = make(map[string]string)
	}
	info.Config.Labels["managed-by"] = "lattice"

	// Step 1: Create and start temp container
	resp, err := c.cli.ContainerCreate(ctx, info.Config, &tempHostConfig, networkConfig, nil, tempName)
	if err != nil {
		return "", fmt.Errorf("create temp container: %w", err)
	}

	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		_ = c.RemoveContainer(ctx, resp.ID, true)
		return "", fmt.Errorf("start temp container: %w", err)
	}

	// Step 2: Wait for health (up to 30s)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		check, inspErr := c.cli.ContainerInspect(ctx, resp.ID)
		if inspErr != nil {
			break
		}
		if !check.State.Running {
			// Container crashed — abort
			_ = c.RemoveContainer(ctx, resp.ID, true)
			return "", fmt.Errorf("temp container stopped unexpectedly")
		}
		if check.State.Health == nil || check.State.Health.Status == "healthy" {
			break // No healthcheck or healthy
		}
		if check.State.Health.Status == "unhealthy" {
			_ = c.StopContainer(ctx, resp.ID, 5)
			_ = c.RemoveContainer(ctx, resp.ID, true)
			return "", fmt.Errorf("temp container is unhealthy")
		}
		time.Sleep(2 * time.Second)
	}

	// Step 3: Rename old container to free the name, then stop and remove
	retiredName := originalName + "-retired-" + fmt.Sprintf("%d", time.Now().UnixNano())
	if renameErr := c.cli.ContainerRename(ctx, containerID, retiredName); renameErr != nil {
		log.Printf("graceful-recreate: rename failed for %s: %v — falling back to stop+remove", originalName, renameErr)
		_ = c.StopContainer(ctx, containerID, 10)
		_ = c.RemoveContainer(ctx, containerID, true)
		// Wait briefly for name release
		time.Sleep(2 * time.Second)
	} else {
		// Renamed — clean up in background
		go func() {
			_ = c.StopContainer(context.Background(), containerID, 10)
			_ = c.RemoveContainer(context.Background(), containerID, true)
		}()
	}

	// Step 4: If there were port bindings, we need to recreate with the full config
	if hasPortBindings {
		_ = c.StopContainer(ctx, resp.ID, 5)
		_ = c.RemoveContainer(ctx, resp.ID, true)

		// Recreate with original host config (including ports) and original name
		finalResp, err := c.cli.ContainerCreate(ctx, info.Config, info.HostConfig, networkConfig, nil, originalName)
		if err != nil {
			return "", fmt.Errorf("recreate with ports: %w", err)
		}
		if err := c.cli.ContainerStart(ctx, finalResp.ID, container.StartOptions{}); err != nil {
			return "", fmt.Errorf("start final container: %w", err)
		}
		return finalResp.ID, nil
	}

	// Step 5: No port bindings — just rename the temp container
	if err := c.cli.ContainerRename(ctx, resp.ID, originalName); err != nil {
		// Rename failed — container is running with temp name, not critical
		log.Printf("warning: failed to rename %s to %s: %v", tempName, originalName, err)
	}

	return resp.ID, nil
}

// ContainerExecCreate creates an exec instance in a container.
func (c *Client) ContainerExecCreate(ctx context.Context, containerID string, cmd []string) (string, error) {
	resp, err := c.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd:          cmd,
	})
	if err != nil {
		return "", fmt.Errorf("exec create: %w", err)
	}
	return resp.ID, nil
}

// ContainerExecAttach attaches to an exec instance and returns the hijacked connection.
func (c *Client) ContainerExecAttach(ctx context.Context, execID string) (types.HijackedResponse, error) {
	return c.cli.ContainerExecAttach(ctx, execID, container.ExecAttachOptions{
		Tty: true,
	})
}

// ContainerExecResize resizes the TTY of an exec instance.
func (c *Client) ContainerExecResize(ctx context.Context, execID string, height, width uint) error {
	return c.cli.ContainerExecResize(ctx, execID, container.ResizeOptions{
		Height: height,
		Width:  width,
	})
}

// ContainerResourceUsage holds per-container CPU/memory stats.
type ContainerResourceUsage struct {
	Name       string  `json:"name"`
	ID         string  `json:"id"`
	CPUPercent float64 `json:"cpu_percent"`
	MemUsageMB float64 `json:"mem_usage_mb"`
	MemLimitMB float64 `json:"mem_limit_mb"`
	MemPercent float64 `json:"mem_percent"`
}

// ContainerStats collects one-shot CPU/memory stats for all running containers.
func (c *Client) ContainerStats(ctx context.Context) ([]ContainerResourceUsage, error) {
	containers, err := c.ListContainers(ctx, "")
	if err != nil {
		return nil, err
	}

	var stats []ContainerResourceUsage
	for _, ctr := range containers {
		if ctr.State != "running" {
			continue
		}

		resp, err := c.cli.ContainerStatsOneShot(ctx, ctr.ID)
		if err != nil {
			continue
		}

		var s container.StatsResponse
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		// Calculate CPU percentage
		cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage - s.PreCPUStats.CPUUsage.TotalUsage)
		systemDelta := float64(s.CPUStats.SystemUsage - s.PreCPUStats.SystemUsage)
		cpuPercent := 0.0
		if systemDelta > 0 && cpuDelta > 0 {
			cpuPercent = (cpuDelta / systemDelta) * float64(s.CPUStats.OnlineCPUs) * 100.0
		}

		// Memory usage (subtract cache for actual working set)
		cache := uint64(0)
		if v, ok := s.MemoryStats.Stats["cache"]; ok {
			cache = v
		}
		memUsage := float64(s.MemoryStats.Usage - cache)
		memLimit := float64(s.MemoryStats.Limit)
		memPercent := 0.0
		if memLimit > 0 {
			memPercent = (memUsage / memLimit) * 100.0
		}

		name := ""
		for _, n := range ctr.Names {
			trimmed := strings.TrimPrefix(n, "/")
			if trimmed != "" {
				name = trimmed
				break
			}
		}

		idShort := ctr.ID
		if len(idShort) > 12 {
			idShort = idShort[:12]
		}

		stats = append(stats, ContainerResourceUsage{
			Name:       name,
			ID:         idShort,
			CPUPercent: cpuPercent,
			MemUsageMB: memUsage / 1024 / 1024,
			MemLimitMB: memLimit / 1024 / 1024,
			MemPercent: memPercent,
		})
	}
	return stats, nil
}

// ConnectNetwork connects a container to a network.
func (c *Client) ConnectNetwork(ctx context.Context, networkName, containerID string) error {
	return c.cli.NetworkConnect(ctx, networkName, containerID, nil)
}

// DisconnectNetwork disconnects a container from a network.
func (c *Client) DisconnectNetwork(ctx context.Context, networkName, containerID string, force bool) error {
	return c.cli.NetworkDisconnect(ctx, networkName, containerID, force)
}

// ContainerNetworks returns the list of network names a container is attached to.
func (c *Client) ContainerNetworks(ctx context.Context, containerID string) ([]string, error) {
	info, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return nil, err
	}
	var networks []string
	if info.NetworkSettings != nil {
		for name := range info.NetworkSettings.Networks {
			networks = append(networks, name)
		}
	}
	return networks, nil
}

// ExecInContainer runs a command inside a running container and returns combined output.
func (c *Client) ExecInContainer(ctx context.Context, containerID string, cmd []string) (string, error) {
	exec, err := c.cli.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", fmt.Errorf("exec create: %w", err)
	}

	resp, err := c.cli.ContainerExecAttach(ctx, exec.ID, container.ExecStartOptions{})
	if err != nil {
		return "", fmt.Errorf("exec attach: %w", err)
	}
	defer resp.Close()

	out, _ := io.ReadAll(resp.Reader)
	return string(out), nil
}

func (c *Client) Close() error {
	return c.cli.Close()
}
