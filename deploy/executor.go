package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

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

	e.reportProgress(spec.DeploymentID, "deployed", fmt.Sprintf("deployment completed successfully via %s strategy", strategyName), nil)
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
