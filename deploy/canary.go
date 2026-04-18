package deploy

import (
	"context"
	"fmt"
	"log"
	"time"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

const canaryMonitorDuration = 30 * time.Second

func (e *Executor) executeCanary(ctx context.Context, spec DeploymentSpec) error {
	if len(spec.Containers) == 0 {
		return fmt.Errorf("no containers in deployment spec")
	}

	// Use the first container as the canary
	canarySpec := spec.Containers[0]
	canaryName := canarySpec.Name + "-canary"
	imageRef := canarySpec.Image + ":" + canarySpec.Tag

	e.reportProgress(spec.DeploymentID, "deploying",
		fmt.Sprintf("starting canary: %s", canaryName), nil)

	// Pull image
	var regAuth *dockerclient.RegistryAuth
	if canarySpec.RegistryAuth != nil {
		regAuth = &dockerclient.RegistryAuth{
			Username: canarySpec.RegistryAuth.Username,
			Password: canarySpec.RegistryAuth.Password,
		}
	}

	if err := e.Docker.PullImage(ctx, imageRef, regAuth); err != nil {
		return fmt.Errorf("pull canary image: %w", err)
	}

	// Start canary (no port bindings — runs alongside existing)
	dockerSpec := dockerclient.ContainerSpec{
		Name:          canaryName,
		Image:         canarySpec.Image,
		Tag:           canarySpec.Tag,
		EnvVars:       canarySpec.EnvVars,
		Volumes:       canarySpec.Volumes,
		CPULimit:      canarySpec.CPULimit,
		MemoryLimit:   canarySpec.MemoryLimit,
		RestartPolicy: "no",
		Command:       canarySpec.Command,
		Entrypoint:    canarySpec.Entrypoint,
		Networks:      canarySpec.Networks,
	}

	canaryID, err := e.Docker.CreateAndStartContainer(ctx, dockerSpec)
	if err != nil {
		return fmt.Errorf("create canary: %w", err)
	}

	e.reportProgress(spec.DeploymentID, "deploying",
		fmt.Sprintf("canary running, monitoring for %v", canaryMonitorDuration),
		map[string]any{"canary_container_id": canaryID, "step": "monitoring"})

	// Monitor canary
	monitorCtx, monitorCancel := context.WithTimeout(ctx, canaryMonitorDuration)
	defer monitorCancel()

	healthy := e.monitorCanary(monitorCtx, canaryID)

	// Clean up canary
	_ = e.Docker.StopContainer(ctx, canaryID, 10)
	_ = e.Docker.RemoveContainer(ctx, canaryID, true)

	if !healthy {
		return fmt.Errorf("canary health check failed — aborting deployment")
	}

	e.reportProgress(spec.DeploymentID, "deploying",
		"canary healthy, proceeding with rolling update", nil)

	// Canary passed — proceed with rolling deployment for all containers
	return e.executeRolling(ctx, spec)
}

func (e *Executor) monitorCanary(ctx context.Context, containerID string) bool {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Timeout reached — canary survived the monitoring period
			return true
		case <-ticker.C:
			info, err := e.Docker.InspectContainer(ctx, containerID)
			if err != nil {
				log.Printf("deploy: canary inspect error: %v", err)
				return false
			}
			if !info.State.Running {
				log.Printf("deploy: canary exited with code %d", info.State.ExitCode)
				return false
			}
		}
	}
}
