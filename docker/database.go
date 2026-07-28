package docker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
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
	// AdoptVolume permits reusing an existing data volume. Off by default: see
	// the check in CreateDatabaseContainer for why silently reusing one is
	// dangerous.
	AdoptVolume bool
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

	// Refuse to silently adopt an existing data volume.
	//
	// Every official database image only initialises when its data directory is
	// empty. Attaching a volume that already has data means MARIADB_USER,
	// MYSQL_PASSWORD, POSTGRES_PASSWORD and friends are ignored entirely: the
	// container starts, reports healthy, and serves the *old* credentials while
	// the control plane records the new ones it just generated. Nothing looks
	// wrong until someone tries to connect.
	//
	// Volumes deliberately outlive their instances (db_remove preserves them),
	// so this is reachable whenever a database is recreated under a name that
	// was used before.
	if _, inspectErr := c.cli.VolumeInspect(ctx, spec.VolumeName); inspectErr == nil {
		if !spec.AdoptVolume {
			return "", fmt.Errorf(
				"data volume %q already exists and would be reused: %s would skip initialisation "+
					"and keep its previous credentials, ignoring the ones configured here. "+
					"Remove the volume (docker volume rm %s), choose a different instance name, "+
					"or set adopt_existing_volume to reuse it deliberately",
				spec.VolumeName, spec.Engine, spec.VolumeName)
		}
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

	// Health check config.
	//
	// StartPeriod matters more than it looks: failures inside it don't count
	// toward the failing streak that flips a container to unhealthy. A cold
	// database legitimately takes far longer than a few seconds — first-time
	// initdb, or InnoDB crash recovery on restart — and 30s was tight enough to
	// mark a perfectly healthy database unhealthy while it was still starting.
	healthInterval := 10 * time.Second
	healthTimeout := 5 * time.Second
	healthRetries := 5
	healthStart := 60 * time.Second

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

// ContainerHealth is the health state Docker reports for a container.
type ContainerHealth struct {
	Status        string // healthy, unhealthy, starting, or "" when no healthcheck
	FailingStreak int
}

// ObservedDatabaseContainer is the state of one managed database container as
// the worker actually sees it, as opposed to what the control plane believes.
type ObservedDatabaseContainer struct {
	ID           string
	Name         string
	State        string // running, exited, restarting, created, paused, dead
	Health       *ContainerHealth
	RestartCount int
	ExitCode     int
}

// ListDatabaseContainers returns every Lattice-managed database container on
// this host, including stopped ones.
//
// Selection is by the labels applied at creation rather than by name prefix, so
// a container that merely looks like a database is never reported as one.
func (c *Client) ListDatabaseContainers(ctx context.Context) ([]ObservedDatabaseContainer, error) {
	f := filters.NewArgs()
	f.Add("label", "managed-by=lattice")
	f.Add("label", "lattice-type=database")

	list, err := c.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("list database containers: %w", err)
	}

	observed := make([]ObservedDatabaseContainer, 0, len(list))
	for _, ctr := range list {
		name := ""
		if len(ctr.Names) > 0 {
			name = strings.TrimPrefix(ctr.Names[0], "/")
		}

		entry := ObservedDatabaseContainer{
			ID:    ctr.ID,
			Name:  name,
			State: ctr.State,
		}

		// Restart count and health only come back from an inspect. A failure
		// here shouldn't drop the container from the report — knowing it exists
		// is already more than the control plane had before.
		if info, err := c.cli.ContainerInspect(ctx, ctr.ID); err == nil && info.State != nil {
			entry.RestartCount = info.RestartCount
			entry.ExitCode = info.State.ExitCode
			if info.State.Health != nil {
				entry.Health = &ContainerHealth{
					Status:        info.State.Health.Status,
					FailingStreak: info.State.Health.FailingStreak,
				}
			}
		}

		observed = append(observed, entry)
	}

	return observed, nil
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
