package docker

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/go-connections/nat"
)

// DatabaseSpec defines a database container to create.
type DatabaseSpec struct {
	ContainerName string
	VolumeName    string
	Engine        string // mysql, mariadb, postgres
	EngineVersion string
	Port          int
	RootPassword  string
	DatabaseName  string
	Username      string
	Password      string
	CPULimit      float64 // CPU cores
	MemoryLimit   int64   // bytes
}

// CreateDatabaseContainer creates and starts a database container with the appropriate
// engine-specific configuration, volume mount, health check, and labels.
func (c *Client) CreateDatabaseContainer(ctx context.Context, spec DatabaseSpec) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	// Validate required fields
	if spec.Engine == "" || spec.EngineVersion == "" {
		return "", fmt.Errorf("engine and engine_version are required")
	}
	if spec.ContainerName == "" || spec.VolumeName == "" {
		return "", fmt.Errorf("container_name and volume_name are required")
	}
	if spec.Port <= 0 {
		return "", fmt.Errorf("port must be positive")
	}
	if spec.DatabaseName == "" || spec.Username == "" {
		return "", fmt.Errorf("database_name and username are required")
	}

	// Determine image
	imageRef := spec.Engine + ":" + spec.EngineVersion

	// Pull the image first
	if err := c.PullImage(ctx, imageRef, nil); err != nil {
		return "", fmt.Errorf("pull database image: %w", err)
	}

	// Engine-specific configuration
	var env []string
	var dataDir string
	var healthCmd []string

	switch spec.Engine {
	case "mysql":
		env = []string{
			"MYSQL_ROOT_PASSWORD=" + spec.RootPassword,
			"MYSQL_DATABASE=" + spec.DatabaseName,
			"MYSQL_USER=" + spec.Username,
			"MYSQL_PASSWORD=" + spec.Password,
		}
		dataDir = "/var/lib/mysql"
		healthCmd = []string{"CMD", "mysqladmin", "ping", "-h", "localhost"}
	case "mariadb":
		env = []string{
			"MARIADB_ROOT_PASSWORD=" + spec.RootPassword,
			"MARIADB_DATABASE=" + spec.DatabaseName,
			"MARIADB_USER=" + spec.Username,
			"MARIADB_PASSWORD=" + spec.Password,
		}
		dataDir = "/var/lib/mysql"
		healthCmd = []string{"CMD", "healthcheck.sh", "--connect", "--innodb_initialized"}
	case "postgres":
		env = []string{
			"POSTGRES_PASSWORD=" + spec.Password,
			"POSTGRES_DB=" + spec.DatabaseName,
			"POSTGRES_USER=" + spec.Username,
		}
		dataDir = "/var/lib/postgresql/data"
		healthCmd = []string{"CMD", "pg_isready", "-U", spec.Username}
	default:
		return "", fmt.Errorf("unsupported database engine: %s", spec.Engine)
	}

	// Create the named volume
	_, err := c.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   spec.VolumeName,
		Driver: "local",
	})
	if err != nil {
		return "", fmt.Errorf("create volume %s: %w", spec.VolumeName, err)
	}

	var containerID string
	cleanupOnError := func() {
		if containerID != "" {
			_ = c.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		}
	}

	// Port mapping
	containerPort := nat.Port(fmt.Sprintf("%d/tcp", defaultDBPort(spec.Engine)))
	exposedPorts := nat.PortSet{containerPort: struct{}{}}
	portBindings := nat.PortMap{
		containerPort: []nat.PortBinding{
			{HostPort: fmt.Sprintf("%d", spec.Port)},
		},
	}

	// Health check config
	healthInterval := 10 * time.Second
	healthTimeout := 5 * time.Second
	healthRetries := 5
	healthStart := 30 * time.Second

	// Resource limits
	resources := container.Resources{}
	if spec.CPULimit > 0 {
		resources.NanoCPUs = int64(spec.CPULimit * 1e9)
	}
	if spec.MemoryLimit > 0 {
		resources.Memory = spec.MemoryLimit
	}

	// Labels
	labels := map[string]string{
		"managed-by":     "lattice",
		"lattice-type":   "database",
		"lattice-engine": spec.Engine,
	}

	// Create the container
	resp, err := c.cli.ContainerCreate(ctx,
		&container.Config{
			Image:        imageRef,
			Env:          env,
			ExposedPorts: exposedPorts,
			Healthcheck: &container.HealthConfig{
				Test:        healthCmd,
				Interval:    healthInterval,
				Timeout:     healthTimeout,
				Retries:     healthRetries,
				StartPeriod: healthStart,
			},
			Labels: labels,
		},
		&container.HostConfig{
			PortBindings:  portBindings,
			Binds:         []string{spec.VolumeName + ":" + dataDir},
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			Resources:     resources,
		},
		nil, nil, spec.ContainerName,
	)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	containerID = resp.ID

	// Start
	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		cleanupOnError()
		return "", fmt.Errorf("start container: %w", err)
	}

	return resp.ID, nil
}

// defaultDBPort returns the default internal port for a database engine.
func defaultDBPort(engine string) int {
	switch engine {
	case "mysql", "mariadb":
		return 3306
	case "postgres":
		return 5432
	default:
		return 3306
	}
}
