package deploy

import (
	"context"
	"fmt"
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
	tag := canarySpec.Tag
	if tag == "" {
		tag = "latest"
	}
	imageRef := canarySpec.Image + ":" + tag

	e.reportProgress(spec.DeploymentID, "deploying",
		fmt.Sprintf("starting canary deployment: %s (image=%s:%s)", canaryName, canarySpec.Image, canarySpec.Tag), nil)

	// Pull image
	var regAuth *dockerclient.RegistryAuth
	if canarySpec.RegistryAuth != nil {
		regAuth = &dockerclient.RegistryAuth{
			Username: canarySpec.RegistryAuth.Username,
			Password: canarySpec.RegistryAuth.Password,
		}
	}

	e.reportProgress(spec.DeploymentID, "deploying",
		fmt.Sprintf("pulling canary image: %s:%s", canarySpec.Image, canarySpec.Tag),
		map[string]any{"container_name": canaryName, "step": "pulling"})

	if err := e.Docker.PullImage(ctx, imageRef, regAuth); err != nil {
		return fmt.Errorf("pull canary image: %w", err)
	}

	e.reportProgress(spec.DeploymentID, "deploying",
		fmt.Sprintf("canary image pulled, creating container: %s", canaryName),
		map[string]any{"container_name": canaryName, "step": "creating"})

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
		HealthCheck:   convertHealthCheck(canarySpec.HealthCheck),
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

	healthy := e.monitorCanary(monitorCtx, canaryID, spec.DeploymentID)

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

func (e *Executor) monitorCanary(ctx context.Context, containerID string, deploymentID int) bool {
	checks := 6 // 6 checks * 5s = 30s monitoring window
	for i := 0; i < checks; i++ {
		time.Sleep(5 * time.Second)
		info, err := e.Docker.InspectContainer(ctx, containerID)
		if err != nil {
			e.reportProgress(deploymentID, "deploying",
				fmt.Sprintf("canary check %d/%d: inspect failed: %v", i+1, checks, err), nil)
			return false
		}
		if !info.State.Running {
			e.reportProgress(deploymentID, "deploying",
				fmt.Sprintf("canary check %d/%d: container stopped", i+1, checks), nil)
			return false
		}
		// Check Docker health status if configured
		if info.State.Health != nil {
			status := info.State.Health.Status
			if status == "unhealthy" {
				e.reportProgress(deploymentID, "deploying",
					fmt.Sprintf("canary check %d/%d: unhealthy", i+1, checks), nil)
				return false
			}
			e.reportProgress(deploymentID, "deploying",
				fmt.Sprintf("canary check %d/%d: running (health=%s)", i+1, checks, status), nil)
		} else {
			e.reportProgress(deploymentID, "deploying",
				fmt.Sprintf("canary check %d/%d: running", i+1, checks), nil)
		}
	}
	return true
}
