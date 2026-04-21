package deploy

import (
	"context"
	"fmt"
	"log"
	"time"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

func (e *Executor) executeBlueGreen(ctx context.Context, spec DeploymentSpec) error {
	e.reportProgress(spec.DeploymentID, "deploying", fmt.Sprintf("starting blue-green deployment with %d containers", len(spec.Containers)), nil)

	// Phase 1: Start all "green" containers with a temporary name suffix
	greenIDs := make(map[string]string) // containerName -> greenContainerID

	for i, cSpec := range spec.Containers {
		tag := cSpec.Tag
		if tag == "" {
			tag = "latest"
		}
		imageRef := cSpec.Image + ":" + tag

		var regAuth *dockerclient.RegistryAuth
		if cSpec.RegistryAuth != nil {
			regAuth = &dockerclient.RegistryAuth{
				Username: cSpec.RegistryAuth.Username,
				Password: cSpec.RegistryAuth.Password,
			}
		}

		replicas := cSpec.Replicas
		if replicas <= 0 {
			replicas = 1
		}

		e.reportProgress(spec.DeploymentID, "deploying",
			fmt.Sprintf("[%d/%d] pulling image %s for green containers", i+1, len(spec.Containers), imageRef),
			map[string]any{"container_name": cSpec.Name, "step": "pulling"})

		log.Printf("deploy: pulling image %s for green", imageRef)
		if err := e.Docker.PullImage(ctx, imageRef, regAuth); err != nil {
			e.cleanupGreen(ctx, greenIDs)
			return fmt.Errorf("pull image %s: %w", imageRef, err)
		}

		for replica := 0; replica < replicas; replica++ {
			name := cSpec.Name
			if replicas > 1 {
				name = fmt.Sprintf("%s-%d", cSpec.Name, replica+1)
			}
			greenName := name + "-green"

			// Green containers use different host ports temporarily
			// In a real blue-green, you'd use a reverse proxy switch
			// For simplicity, we create green, verify health, then swap names
			dockerSpec := dockerclient.ContainerSpec{
				Name:          greenName,
				Image:         cSpec.Image,
				Tag:           cSpec.Tag,
				PortMappings:  nil, // Don't bind ports yet
				EnvVars:       cSpec.EnvVars,
				Volumes:       cSpec.Volumes,
				CPULimit:      cSpec.CPULimit,
				MemoryLimit:   cSpec.MemoryLimit,
				RestartPolicy: cSpec.RestartPolicy,
				Command:       cSpec.Command,
				Entrypoint:    cSpec.Entrypoint,
				Networks:      cSpec.Networks,
				HealthCheck:   convertHealthCheck(cSpec.HealthCheck),
			}

			containerID, err := e.Docker.CreateAndStartContainer(ctx, dockerSpec)
			if err != nil {
				e.cleanupGreen(ctx, greenIDs)
				return fmt.Errorf("create green container %s: %w", greenName, err)
			}

			greenIDs[name] = containerID
			e.reportProgress(spec.DeploymentID, "deploying",
				fmt.Sprintf("green container %s started", greenName),
				map[string]any{"container_name": greenName, "step": "green_running"})
		}
	}

	// Phase 2: Wait for green containers to become healthy
	e.reportProgress(spec.DeploymentID, "deploying", "all green containers running, performing health checks", nil)
	for cName, id := range greenIDs {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			info, err := e.Docker.InspectContainer(ctx, id)
			if err == nil && info.State.Running {
				// If no healthcheck configured, running is good enough
				if info.State.Health == nil || info.State.Health.Status == "healthy" {
					break
				}
			}
			time.Sleep(2 * time.Second)
		}

		// After the deadline, verify the container is actually healthy
		info, err := e.Docker.InspectContainer(ctx, id)
		if err != nil || !info.State.Running {
			e.reportProgress(spec.DeploymentID, "failed", fmt.Sprintf("health check failed: container %s not running", cName), nil)
			e.cleanupGreen(ctx, greenIDs)
			return fmt.Errorf("health check failed for %s: container not running", cName)
		}
		if info.State.Health != nil && info.State.Health.Status != "healthy" {
			e.reportProgress(spec.DeploymentID, "failed", fmt.Sprintf("health check failed: %s is %s", cName, info.State.Health.Status), nil)
			e.cleanupGreen(ctx, greenIDs)
			return fmt.Errorf("health check failed for %s: status %s", cName, info.State.Health.Status)
		}
	}

	// Phase 3: Stop blue (old) and rename green to take over
	e.reportProgress(spec.DeploymentID, "deploying", "health check passed, swapping blue→green", nil)

	// Track blue containers before removing them so we can attempt restart on failure
	type blueBackup struct {
		name string
		id   string
	}
	var blueBackups []blueBackup
	var swapErr error

	for _, cSpec := range spec.Containers {
		if swapErr != nil {
			break
		}
		replicas := cSpec.Replicas
		if replicas <= 0 {
			replicas = 1
		}

		for replica := 0; replica < replicas; replica++ {
			blueName := cSpec.Name
			if replicas > 1 {
				blueName = fmt.Sprintf("%s-%d", cSpec.Name, replica+1)
			}

			// Capture blue ID before stopping
			var blueID string
			if id, err := e.Docker.FindContainerByName(ctx, blueName); err == nil && id != "" {
				blueID = id
				blueBackups = append(blueBackups, blueBackup{name: blueName, id: blueID})
			}

			// Stop and remove blue
			e.reportProgress(spec.DeploymentID, "deploying",
				fmt.Sprintf("stopping blue (old) container: %s", blueName),
				map[string]any{"container_name": blueName, "step": "stopping_blue"})
			if blueID != "" {
				_ = e.Docker.StopContainer(ctx, blueID, 30)
				_ = e.Docker.RemoveContainer(ctx, blueID, true)
			}

			// Stop green, remove it, recreate with correct name and ports
			if greenID, ok := greenIDs[blueName]; ok {
				_ = e.Docker.StopContainer(ctx, greenID, 10)
				_ = e.Docker.RemoveContainer(ctx, greenID, true)
			}

			// Recreate with the proper name and port bindings
			portMappings := make([]dockerclient.PortMapping, len(cSpec.PortMappings))
			for j, pm := range cSpec.PortMappings {
				portMappings[j] = dockerclient.PortMapping{
					HostPort:      pm.HostPort,
					ContainerPort: pm.ContainerPort,
					Protocol:      pm.Protocol,
				}
			}

			dockerSpec := dockerclient.ContainerSpec{
				Name:          blueName,
				Image:         cSpec.Image,
				Tag:           cSpec.Tag,
				PortMappings:  portMappings,
				EnvVars:       cSpec.EnvVars,
				Volumes:       cSpec.Volumes,
				CPULimit:      cSpec.CPULimit,
				MemoryLimit:   cSpec.MemoryLimit,
				RestartPolicy: cSpec.RestartPolicy,
				Command:       cSpec.Command,
				Entrypoint:    cSpec.Entrypoint,
				Networks:      cSpec.Networks,
				HealthCheck:   convertHealthCheck(cSpec.HealthCheck),
			}

			_, err := e.Docker.CreateAndStartContainer(ctx, dockerSpec)
			if err != nil {
				swapErr = fmt.Errorf("recreate container %s: %w", blueName, err)
				break
			}

			e.reportProgress(spec.DeploymentID, "deploying",
				fmt.Sprintf("swapped to new container: %s", blueName),
				map[string]any{"container_name": blueName, "step": "swapped"})
		}
	}

	// If swap failed, try to restart blue containers as rollback
	if swapErr != nil {
		log.Printf("deploy: blue-green swap failed, attempting to restart blue containers: %v", swapErr)
		for _, bb := range blueBackups {
			_ = e.Docker.StartContainer(ctx, bb.id) // may fail if already removed
		}
		return swapErr
	}

	return nil
}

func (e *Executor) cleanupGreen(ctx context.Context, greenIDs map[string]string) {
	for name, id := range greenIDs {
		log.Printf("deploy: cleaning up green container %s-green", name)
		_ = e.Docker.StopContainer(ctx, id, 10)
		_ = e.Docker.RemoveContainer(ctx, id, true)
	}
}
