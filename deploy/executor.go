package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"syscall"
	"time"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

// shellMetaChars contains characters that could be used for shell injection.
const shellMetaChars = ";$`|&"

// validContainerName checks that a container name is safe to use.
// Allows alphanumeric, hyphens, underscores, dots, and forward slashes (for compose prefixes).
func validContainerName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '/') {
			return false
		}
	}
	return true
}

// Validate checks the deployment spec for safety and sanity limits.
func (s *DeploymentSpec) Validate() error {
	if len(s.Containers) > 100 {
		return fmt.Errorf("too many containers: %d (max 100)", len(s.Containers))
	}
	if len(s.Networks) > 50 {
		return fmt.Errorf("too many networks: %d (max 50)", len(s.Networks))
	}
	if len(s.Volumes) > 50 {
		return fmt.Errorf("too many volumes: %d (max 50)", len(s.Volumes))
	}
	for i, c := range s.Containers {
		if !validContainerName(c.Name) {
			return fmt.Errorf("container[%d]: invalid name %q", i, c.Name)
		}
		if c.Image == "" {
			return fmt.Errorf("container[%d] %q: image is required", i, c.Name)
		}
		imageRef := c.Image
		if c.Tag != "" {
			imageRef += ":" + c.Tag
		}
		if strings.ContainsAny(imageRef, shellMetaChars) {
			return fmt.Errorf("container[%d] %q: image ref contains shell metacharacters", i, c.Name)
		}
	}
	return nil
}

// DeploymentSpec is the deployment specification received from the orchestrator.
type DeploymentSpec struct {
	DeploymentID int             `json:"deployment_id"`
	StackName    string          `json:"stack_name"`
	Strategy     string          `json:"strategy"`
	Containers   []ContainerSpec `json:"containers"`
	Networks     []NetworkSpec   `json:"networks"`
	Volumes      []VolumeSpec    `json:"volumes"`
}

type ContainerSpec struct {
	ID            int               `json:"id"`
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	Tag           string            `json:"tag"`
	PortMappings  []PortMapping     `json:"port_mappings"`
	EnvVars       map[string]string `json:"env_vars"`
	Volumes       map[string]string `json:"volumes"`
	CPULimit      float64           `json:"cpu_limit"`
	MemoryLimit   int64             `json:"memory_limit"`
	Replicas      int               `json:"replicas"`
	RestartPolicy string            `json:"restart_policy"`
	Command       []string          `json:"command"`
	Entrypoint    []string          `json:"entrypoint"`
	Networks       []string          `json:"networks"`
	NetworkAliases []string         `json:"network_aliases,omitempty"`
	HealthCheck   *HealthCheck      `json:"health_check,omitempty"`
	RegistryAuth  *RegistryAuth     `json:"registry_auth,omitempty"`
	DependsOn     []string          `json:"depends_on,omitempty"`
}

type HealthCheck struct {
	Test        []string `json:"test"`
	Interval    string   `json:"interval"`
	Timeout     string   `json:"timeout"`
	Retries     int      `json:"retries"`
	StartPeriod string   `json:"start_period"`
}

func (h *HealthCheck) UnmarshalJSON(data []byte) error {
	// Use an alias to avoid infinite recursion
	type Alias HealthCheck
	raw := struct {
		Alias
		Test json.RawMessage `json:"test"`
	}{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*h = HealthCheck(raw.Alias)

	if len(raw.Test) == 0 {
		return nil
	}

	// Try []string first
	var arr []string
	if err := json.Unmarshal(raw.Test, &arr); err == nil {
		h.Test = arr
		return nil
	}

	// Fall back to bare string → ["CMD-SHELL", "command"]
	var s string
	if err := json.Unmarshal(raw.Test, &s); err == nil && s != "" {
		h.Test = []string{"CMD-SHELL", s}
	}

	return nil
}

type PortMapping struct {
	HostPort      string `json:"host_port"`
	ContainerPort string `json:"container_port"`
	Protocol      string `json:"protocol"`
}

type NetworkSpec struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
}

type VolumeSpec struct {
	Name   string `json:"name"`
	Driver string `json:"driver"`
}

type RegistryAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// ProgressCallback reports deployment progress back to the orchestrator.
type ProgressCallback func(deploymentID int, status string, message string, payload map[string]any)

// Executor handles deployment execution across strategies.
type Executor struct {
	Docker   *dockerclient.Client
	Progress ProgressCallback
}

func NewExecutor(docker *dockerclient.Client, progress ProgressCallback) *Executor {
	return &Executor{Docker: docker, Progress: progress}
}

// Execute runs a deployment according to the specified strategy.
func (e *Executor) Execute(ctx context.Context, spec DeploymentSpec) error {
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("spec validation failed: %w", err)
	}

	// Pre-flight check: ensure sufficient disk space
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err == nil {
		availGB := float64(stat.Bavail*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
		if availGB < 1.0 {
			return fmt.Errorf("insufficient disk space: %.1fGB available, need at least 1GB", availGB)
		}
	}

	log.Printf("deploy: starting deployment=%d strategy=%s stack=%s", spec.DeploymentID, spec.Strategy, spec.StackName)

	e.reportProgress(spec.DeploymentID, "deploying", fmt.Sprintf("starting deployment: strategy=%s, containers=%d, networks=%d, volumes=%d",
		spec.Strategy, len(spec.Containers), len(spec.Networks), len(spec.Volumes)), nil)

	// Create networks
	for _, net := range spec.Networks {
		driver := net.Driver
		if driver == "" {
			driver = "bridge"
		}
		e.reportProgress(spec.DeploymentID, "deploying", fmt.Sprintf("ensuring network: %s (driver=%s)", net.Name, driver),
			map[string]any{"step": "network"})
		if err := e.Docker.CreateNetwork(ctx, net.Name, driver); err != nil {
			log.Printf("deploy: network %s may already exist: %v", net.Name, err)
		}
	}

	// Create volumes
	for _, vol := range spec.Volumes {
		driver := vol.Driver
		if driver == "" {
			driver = "local"
		}
		e.reportProgress(spec.DeploymentID, "deploying", fmt.Sprintf("ensuring volume: %s (driver=%s)", vol.Name, driver),
			map[string]any{"step": "volume"})
		if err := e.Docker.CreateVolume(ctx, vol.Name, driver); err != nil {
			log.Printf("deploy: volume %s may already exist: %v", vol.Name, err)
		}
	}

	// Clean up stale containers from this stack that are no longer in the spec
	// (e.g. renamed or removed from compose). This prevents port conflicts and
	// orphaned containers when compose services are renamed.
	e.cleanupStaleContainers(ctx, spec)

	var err error
	strategyName := spec.Strategy
	switch spec.Strategy {
	case "rolling":
		err = e.executeRolling(ctx, spec)
	case "blue-green":
		err = e.executeBlueGreen(ctx, spec)
	case "canary":
		err = e.executeCanary(ctx, spec)
	default:
		strategyName = "rolling (default)"
		err = e.executeRolling(ctx, spec) // default to rolling
	}

	if err != nil {
		e.reportProgress(spec.DeploymentID, "failed", fmt.Sprintf("deployment failed during %s: %v", strategyName, err), nil)
		return err
	}

	// Post-deploy verification: check containers at intervals to ensure they
	// stabilize and don't immediately crash-loop.
	if verifyErr := e.postDeployVerify(ctx, spec); verifyErr != nil {
		e.reportProgress(spec.DeploymentID, "failed", fmt.Sprintf("post-deploy verification failed: %v", verifyErr), nil)
		return verifyErr
	}

	e.reportProgress(spec.DeploymentID, "deployed", fmt.Sprintf("deployment completed successfully via %s strategy", strategyName), nil)
	return nil
}

// cleanupStaleContainers finds containers that belong to this stack (via the
// lattice-stack label) but are NOT in the new deployment spec. These are
// containers that were renamed or removed from the compose file. They must be
// stopped before deploying to free ports and avoid orphans.
//
// For containers without the lattice-stack label (created before labels were
// added), falls back to a port-conflict check: if a lattice-managed container
// holds a host port needed by the new spec and isn't in the spec by name,
// it is stopped.
func (e *Executor) cleanupStaleContainers(ctx context.Context, spec DeploymentSpec) {
	// Build set of canonical names in the new spec
	specNames := make(map[string]bool)
	for _, c := range spec.Containers {
		specNames[c.Name] = true
	}

	// Build set of host ports needed by the new spec
	neededPorts := make(map[string]bool)
	for _, c := range spec.Containers {
		for _, pm := range c.PortMappings {
			if pm.HostPort != "" {
				neededPorts[pm.HostPort] = true
			}
		}
	}

	containers, err := e.Docker.ListContainers(ctx, "")
	if err != nil {
		log.Printf("deploy: cleanup: failed to list containers: %v", err)
		return
	}

	for _, c := range containers {
		if c.Labels["managed-by"] != "lattice" {
			continue
		}

		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if name == "" {
			continue
		}

		canonical := dockerclient.CanonicalContainerName(name)

		// Skip containers that are part of the new spec
		if specNames[canonical] || specNames[name] {
			continue
		}

		shouldRemove := false
		reason := ""

		// Check 1: container has the lattice-stack label matching this stack
		if c.Labels["lattice-stack"] == spec.StackName {
			shouldRemove = true
			reason = fmt.Sprintf("stale stack container (was in stack %q, not in new spec)", spec.StackName)
		}

		// Check 2: container holds a port we need (fallback for unlabeled containers)
		if !shouldRemove && len(neededPorts) > 0 {
			for _, p := range c.Ports {
				if p.PublicPort > 0 && neededPorts[fmt.Sprintf("%d", p.PublicPort)] {
					shouldRemove = true
					reason = fmt.Sprintf("holds port %d needed by new spec", p.PublicPort)
					break
				}
			}
		}

		if shouldRemove {
			log.Printf("deploy: cleanup: removing %s (%s)", name, reason)
			e.reportProgress(spec.DeploymentID, "deploying",
				fmt.Sprintf("removing stale container %s (%s)", name, reason),
				map[string]any{"step": "cleanup"})
			if err := e.Docker.StopAndRemoveContainer(ctx, c.ID, 10); err != nil {
				log.Printf("deploy: cleanup: failed to remove %s: %v", name, err)
			}
		}
	}
}

// postDeployVerify checks that all deployed containers remain healthy over
// a verification window. Runs 6 checks at 10-second intervals (60s total).
// Detects containers that stopped, entered a restart loop, or failed health
// checks shortly after deployment — the most common failure pattern.
func (e *Executor) postDeployVerify(ctx context.Context, spec DeploymentSpec) error {
	const (
		verifyChecks   = 6
		verifyInterval = 10 * time.Second
	)

	// Build expected container names (accounting for replicas)
	expected := make(map[string]bool)
	for _, c := range spec.Containers {
		replicas := c.Replicas
		if replicas <= 0 {
			replicas = 1
		}
		if replicas == 1 {
			expected[c.Name] = true
		} else {
			for r := 1; r <= replicas; r++ {
				expected[fmt.Sprintf("%s-%d", c.Name, r)] = true
			}
		}
	}

	e.reportProgress(spec.DeploymentID, "validating",
		fmt.Sprintf("verifying deployment health (%d checks over %s)", verifyChecks, time.Duration(verifyChecks)*verifyInterval),
		map[string]any{"step": "verify-0", "verify_check": 0, "verify_total": verifyChecks, "container_count": len(expected)})

	for check := 1; check <= verifyChecks; check++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(verifyInterval):
		}

		containers, err := e.Docker.ListContainers(ctx, "")
		if err != nil {
			log.Printf("deploy: verify: failed to list containers: %v", err)
			continue
		}

		// Build a map of running containers by canonical name
		running := make(map[string]string) // canonical name -> state
		for _, c := range containers {
			name := ""
			if len(c.Names) > 0 {
				name = strings.TrimPrefix(c.Names[0], "/")
			}
			canonical := dockerclient.CanonicalContainerName(name)
			running[canonical] = c.State
		}

		var issues []string

		for name := range expected {
			state, found := running[name]
			if !found {
				issues = append(issues, fmt.Sprintf("%s: not found (removed or crashed)", name))
				continue
			}
			switch state {
			case "running":
				// Good — also check for restart loops via inspect
				id := ""
				for _, c := range containers {
					for _, n := range c.Names {
						cn := dockerclient.CanonicalContainerName(strings.TrimPrefix(n, "/"))
						if cn == name {
							id = c.ID
						}
					}
				}
				if id != "" {
					if info, err := e.Docker.InspectContainer(ctx, id); err == nil {
						if info.RestartCount > 0 {
							issues = append(issues, fmt.Sprintf("%s: restarted %d times since deploy", name, info.RestartCount))
						}
						if info.State != nil && info.State.Health != nil && info.State.Health.Status == "unhealthy" {
							issues = append(issues, fmt.Sprintf("%s: health check reports unhealthy", name))
						}
					}
				}
			case "restarting":
				issues = append(issues, fmt.Sprintf("%s: stuck in restarting state", name))
			case "exited", "dead":
				issues = append(issues, fmt.Sprintf("%s: exited (state=%s)", name, state))
			}
		}

		verifyPayload := map[string]any{
			"step":            fmt.Sprintf("verify-%d", check),
			"verify_check":    check,
			"verify_total":    verifyChecks,
			"container_count": len(expected),
		}

		if len(issues) > 0 {
			msg := fmt.Sprintf("verify check %d/%d: %s", check, verifyChecks, strings.Join(issues, "; "))
			e.reportProgress(spec.DeploymentID, "validating", msg, verifyPayload)

			// On the final check, if there are still issues, fail the deployment
			if check == verifyChecks {
				return fmt.Errorf("containers unhealthy after %d checks: %s", verifyChecks, strings.Join(issues, "; "))
			}
		} else {
			e.reportProgress(spec.DeploymentID, "validating",
				fmt.Sprintf("verify check %d/%d: all containers healthy", check, verifyChecks),
				verifyPayload)
		}
	}

	return nil
}

func (e *Executor) reportProgress(deploymentID int, status, message string, extra map[string]any) {
	if e.Progress != nil {
		payload := map[string]any{
			"deployment_id": deploymentID,
			"status":        status,
			"message":       message,
		}
		for k, v := range extra {
			payload[k] = v
		}
		e.Progress(deploymentID, status, message, payload)
	}
}

// convertHealthCheck converts a deploy HealthCheck to a docker HealthCheck.
func convertHealthCheck(hc *HealthCheck) *dockerclient.HealthCheck {
	if hc == nil {
		return nil
	}
	return &dockerclient.HealthCheck{
		Test:        hc.Test,
		Interval:    hc.Interval,
		Timeout:     hc.Timeout,
		Retries:     hc.Retries,
		StartPeriod: hc.StartPeriod,
	}
}

// ParseDeploymentSpec parses a JSON payload into a DeploymentSpec.
func ParseDeploymentSpec(payload map[string]any) (*DeploymentSpec, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}
	var spec DeploymentSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, fmt.Errorf("unmarshal spec: %w", err)
	}
	return &spec, nil
}
