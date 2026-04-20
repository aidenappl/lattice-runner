package docker

import (
	"context"
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
	Networks      []string
	HealthCheck   *HealthCheck
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
	return base64.URLEncoding.EncodeToString(b), nil
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

	resp, err := c.cli.ContainerCreate(ctx, containerConfig, hostConfig, &network.NetworkingConfig{}, nil, spec.Name)
	if err != nil {
		return "", fmt.Errorf("container create: %w", err)
	}

	// Connect to networks
	for _, netName := range spec.Networks {
		if err := c.cli.NetworkConnect(ctx, netName, resp.ID, nil); err != nil {
			log.Printf("docker: failed to connect container %s to network %s: %v", spec.Name, netName, err)
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

// ListContainers returns containers matching the optional name filter.
func (c *Client) ListContainers(ctx context.Context, nameFilter string) ([]types.Container, error) {
	opts := container.ListOptions{All: true}
	if nameFilter != "" {
		opts.Filters = filters.NewArgs(filters.Arg("name", nameFilter))
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

	_ = c.StopContainer(ctx, containerID, 10)
	_ = c.RemoveContainer(ctx, containerID, true)

	resp, err := c.cli.ContainerCreate(ctx, info.Config, info.HostConfig, &network.NetworkingConfig{}, nil, name)
	if err != nil {
		return "", fmt.Errorf("recreate container: %w", err)
	}

	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("start recreated container: %w", err)
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

func (c *Client) Close() error {
	return c.cli.Close()
}
