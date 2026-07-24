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
				Name:           greenName,
				Image:          cSpec.Image,
				Tag:            cSpec.Tag,
				PortMappings:   nil, // Don't bind ports yet
				EnvVars:        cSpec.EnvVars,
				Volumes:        cSpec.Volumes,
				CPULimit:       cSpec.CPULimit,
				MemoryLimit:    cSpec.MemoryLimit,
				RestartPolicy:  cSpec.RestartPolicy,
				Command:        cSpec.Command,
				Entrypoint:     cSpec.Entrypoint,
				Networks:       cSpec.Networks,
				NetworkAliases: cSpec.NetworkAliases,
				StackName:      spec.StackName,
				HealthCheck:    convertHealthCheck(cSpec.HealthCheck),
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

	// Phase 3: Swap — retire blue (rename + keep stopped), remove green, create
	// final with ports + canonical name. Blue is NOT removed until the final
	// container is confirmed running, so a create/crash failure can roll back by
	// renaming the retired blue back to its canonical name and starting it.
	// Order per replica: rename+stop blue (frees name/ports, keeps it recoverable)
	// → remove green → create final → verify → (only on full success) remove blues.
	e.reportProgress(spec.DeploymentID, "deploying", "health check passed, swapping blue→green", nil)

	// blueBackup records a retired blue container so it can be restored on failure
	// or removed on success. retiredName is the name blue was renamed to.
	type blueBackup struct {
		canonicalName string
		id            string
		retiredName   string
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
			canonicalName := cSpec.Name
			if replicas > 1 {
				canonicalName = fmt.Sprintf("%s-%d", cSpec.Name, replica+1)
			}

			// Step 1: Retire blue — rename it out of the way and stop it (do NOT
			// remove it yet). This frees the canonical name and host ports while
			// keeping blue recoverable for rollback.
			e.reportProgress(spec.DeploymentID, "deploying",
				fmt.Sprintf("retiring blue (old) container: %s", canonicalName),
				map[string]any{"container_name": canonicalName, "step": "stopping_blue"})
			if id, err := e.Docker.FindContainerByName(ctx, canonicalName); err == nil && id != "" {
				retiredName := fmt.Sprintf("%s-retired-%d", canonicalName, time.Now().UnixNano())
				if renameErr := e.Docker.RenameContainer(ctx, id, retiredName); renameErr != nil {
					// Rename failed — fall back to stop only so the port frees;
					// record the id under its canonical name for restart-based rollback.
					log.Printf("deploy: blue-green retire rename failed for %s: %v (stopping in place)", canonicalName, renameErr)
					_ = e.Docker.StopContainer(ctx, id, 30)
					blueBackups = append(blueBackups, blueBackup{canonicalName: canonicalName, id: id, retiredName: ""})
				} else {
					_ = e.Docker.StopContainer(ctx, id, 30)
					blueBackups = append(blueBackups, blueBackup{canonicalName: canonicalName, id: id, retiredName: retiredName})
				}
			}

			// Step 2: Remove green container (frees its name, image layers are cached)
			if greenID, ok := greenIDs[canonicalName]; ok {
				_ = e.Docker.StopContainer(ctx, greenID, 10)
				_ = e.Docker.RemoveContainer(ctx, greenID, true)
			}

			// Step 3: Create final container with canonical name + port bindings
			portMappings := make([]dockerclient.PortMapping, len(cSpec.PortMappings))
			for j, pm := range cSpec.PortMappings {
				portMappings[j] = dockerclient.PortMapping{
					HostPort:      pm.HostPort,
					ContainerPort: pm.ContainerPort,
					Protocol:      pm.Protocol,
				}
			}

			dockerSpec := dockerclient.ContainerSpec{
				Name:           canonicalName,
				Image:          cSpec.Image,
				Tag:            cSpec.Tag,
				PortMappings:   portMappings,
				EnvVars:        cSpec.EnvVars,
				Volumes:        cSpec.Volumes,
				CPULimit:       cSpec.CPULimit,
				MemoryLimit:    cSpec.MemoryLimit,
				RestartPolicy:  cSpec.RestartPolicy,
				Command:        cSpec.Command,
				Entrypoint:     cSpec.Entrypoint,
				Networks:       cSpec.Networks,
				NetworkAliases: cSpec.NetworkAliases,
				StackName:      spec.StackName,
				HealthCheck:    convertHealthCheck(cSpec.HealthCheck),
			}

			finalID, err := e.Docker.CreateAndStartContainer(ctx, dockerSpec)
			if err != nil {
				swapErr = fmt.Errorf("recreate container %s: %w", canonicalName, err)
				break
			}

			// Step 4: Verify the final container is actually running before we
			// commit — a crash-on-real-port must not leave old gone with no rollback.
			if info, inspErr := e.Docker.InspectContainer(ctx, finalID); inspErr != nil || !info.State.Running {
				_ = e.Docker.StopAndRemoveContainer(ctx, finalID, 5)
				swapErr = fmt.Errorf("final container %s not running after swap", canonicalName)
				break
			}

			e.reportProgress(spec.DeploymentID, "deploying",
				fmt.Sprintf("swapped to new container: %s", canonicalName),
				map[string]any{"container_name": canonicalName, "step": "swapped"})
		}
	}

	// If swap failed, restore blue containers as rollback, then surface the error.
	if swapErr != nil {
		log.Printf("deploy: blue-green swap failed, restoring blue containers: %v", swapErr)
		for _, bb := range blueBackups {
			// Remove any partially-created final now occupying the canonical name.
			if id, err := e.Docker.FindContainerByName(ctx, bb.canonicalName); err == nil && id != "" && id != bb.id {
				_ = e.Docker.StopAndRemoveContainer(ctx, id, 5)
			}
			// Rename the retired blue back to its canonical name (if it was renamed).
			if bb.retiredName != "" {
				if renameErr := e.Docker.RenameContainer(ctx, bb.id, bb.canonicalName); renameErr != nil {
					log.Printf("deploy: rollback rename %s -> %s failed: %v", bb.retiredName, bb.canonicalName, renameErr)
				}
			}
			// Start blue back up.
			if startErr := e.Docker.StartContainer(ctx, bb.id); startErr != nil {
				log.Printf("deploy: rollback failed to start blue %s: %v", bb.canonicalName, startErr)
			}
		}
		// Clean up any green containers left from un-swapped services.
		e.cleanupGreen(ctx, greenIDs)
		return swapErr
	}

	// Success: the finals are confirmed running, so retired blues are safe to remove.
	for _, bb := range blueBackups {
		_ = e.Docker.RemoveContainer(ctx, bb.id, true)
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
