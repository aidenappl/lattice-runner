package docker

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
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
	// BinlogRetentionSeconds bounds how long binary logs are kept. Set from the
	// instance's snapshot retention so the PITR window and the snapshot window
	// cannot silently disagree. Zero means a 7-day default.
	BinlogRetentionSeconds int
}

// engineMajor extracts the leading major version from an image tag like "11",
// "8.4" or "16.2". Returns 0 when it cannot be read, which callers must treat as
// "assume nothing".
func engineMajor(version string) int {
	digits := ""
	for _, c := range version {
		if c < '0' || c > '9' {
			break
		}
		digits += string(c)
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0
	}
	return n
}

// durabilityArgs returns the server flags that must be set at *create* time for
// point-in-time recovery to be possible later, plus two bounds worth setting
// once rather than discovering.
//
// These are create-time decisions in the strictest sense: binary logging,
// server_id and Postgres' archive_mode all require a server restart to change,
// so an instance created without them cannot gain PITR without downtime. Turning
// them on now costs a few flags and some disk; retrofitting costs a restart of
// every database on the platform. Nothing here *performs* PITR — it only refuses
// to foreclose it.
//
// Two further bounds, both learned the expensive way by other people:
//
//   - innodb_temp_data_file_path is unbounded by default, and MySQL's own manual
//     says the only way to reclaim that space is to restart the server. One bad
//     query permanently inflates the instance until someone notices.
//   - binlog_expire_logs_seconds is set explicitly rather than inherited. The
//     default is 30 days; a 35-day snapshot retention alongside it yields a
//     30-day recovery window, and *nothing in either system reports the
//     disagreement*.
//
// Flags are gated on engine version because an unrecognised flag does not
// degrade — the server refuses to start. expire_logs_days was removed in MySQL
// 8.4 and writing it makes the container crashloop; this function exists partly
// so that class of mistake has one place to live.
func durabilityArgs(engine, version string, binlogRetentionSeconds int) []string {
	major := engineMajor(version)
	if binlogRetentionSeconds <= 0 {
		binlogRetentionSeconds = 7 * 24 * 60 * 60
	}

	switch engine {
	case "mysql":
		if major < 8 {
			return nil
		}
		return []string{
			"--log-bin=binlog",
			"--server-id=1",
			"--binlog-format=ROW",
			fmt.Sprintf("--binlog-expire-logs-seconds=%d", binlogRetentionSeconds),
			"--innodb-temp-data-file-path=ibtmp1:12M:autoextend:max:500M",
		}
	case "mariadb":
		// binlog_expire_logs_seconds landed in MariaDB 10.6. Below 10 the safe
		// move is to set nothing rather than guess at expire_logs_days.
		if major < 10 {
			return nil
		}
		args := []string{
			"--log-bin=binlog",
			"--server-id=1",
			"--binlog-format=ROW",
			"--innodb-temp-data-file-path=ibtmp1:12M:autoextend:max:500M",
		}
		if major >= 11 {
			args = append(args, fmt.Sprintf("--binlog-expire-logs-seconds=%d", binlogRetentionSeconds))
		}
		return args
	case "postgres":
		if major < 12 {
			return nil
		}
		// archive_mode requires a restart to change; archive_command only a
		// reload. Enabling the former now with a no-op command means WAL
		// archiving can be switched on later without touching the container.
		return []string{
			"-c", "wal_level=replica",
			"-c", "archive_mode=on",
			"-c", "archive_command=/bin/true",
		}
	}
	return nil
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
			Image: imageRef,
			// Durability flags are create-time only: binary logging, server_id
			// and archive_mode all need a restart to change.
			Cmd:          durabilityArgs(spec.Engine, spec.EngineVersion, spec.BinlogRetentionSeconds),
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

// VolumeSize returns the on-disk size of a Docker volume in bytes.
//
// Deliberately walks the volume's mountpoint rather than asking Docker. There is
// no per-volume size in the Docker API: `docker system df -v` is the only thing
// that reports one, and it holds the daemon's main container lock while it
// computes — long enough on a host with a few dozen containers to make every
// concurrent `docker ps` hang for minutes. An agent that polled it would
// intermittently wedge its own control path.
//
// This walk is O(files), so it must be called on a slow cadence, never per sync.
func (c *Client) VolumeSize(ctx context.Context, volumeName string) (int64, error) {
	vol, err := c.cli.VolumeInspect(ctx, volumeName)
	if err != nil {
		return 0, fmt.Errorf("inspect volume %s: %w", volumeName, err)
	}
	if vol.Mountpoint == "" {
		return 0, fmt.Errorf("volume %s has no mountpoint", volumeName)
	}

	var total int64
	err = filepath.WalkDir(vol.Mountpoint, func(path string, d fs.DirEntry, err error) error {
		// A file vanishing mid-walk is normal for a live database; it is not a
		// reason to fail the measurement.
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walk volume %s: %w", volumeName, err)
	}
	return total, nil
}

// DatabaseVolumeName returns the data volume attached to a managed database
// container, read from its mounts rather than derived from its name.
func (c *Client) DatabaseVolumeName(ctx context.Context, containerID string) (string, error) {
	info, err := c.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}
	for _, m := range info.Mounts {
		if m.Type == "volume" && m.Name != "" {
			return m.Name, nil
		}
	}
	return "", nil
}
