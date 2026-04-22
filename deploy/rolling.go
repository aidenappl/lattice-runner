package deploy

import (
	"context"
	"fmt"
	"log"
	"time"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

// containerSnapshot captures the pre-deployment state of a container for rollback.
type containerSnapshot struct {
	Name     string
	OldImage string // full image:tag before upgrade
	SpecIdx  int    // index into spec.Containers
	Replica  int    // which replica
}

func (e *Executor) executeRolling(ctx context.Context, spec DeploymentSpec) error {
	// Capture current state before rolling update for rollback purposes
	var snapshots []containerSnapshot
	for i, cSpec := range spec.Containers {
		tag := cSpec.Tag
		if tag == "" {
			tag = "latest"
		}
		replicas := cSpec.Replicas
		if replicas <= 0 {
			replicas = 1
		}
		for replica := 0; replica < replicas; replica++ {
			name := cSpec.Name
			if replicas > 1 {
				name = fmt.Sprintf("%s-%d", cSpec.Name, replica+1)
			}
			oldImage := cSpec.Image + ":" + tag
			// Try to find existing container to get its current image
			if id, err := e.Docker.FindContainerByName(ctx, name); err == nil && id != "" {
				info, err := e.Docker.InspectContainer(ctx, id)
				if err == nil {
					oldImage = info.Config.Image
				}
			}
			snapshots = append(snapshots, containerSnapshot{
				Name:     name,
				OldImage: oldImage,
				SpecIdx:  i,
				Replica:  replica,
			})
		}
	}

	var updatedContainers []int // indices into snapshots of successfully updated containers
	snapshotIdx := 0

	for i, cSpec := range spec.Containers {
		tag := cSpec.Tag
		if tag == "" {
			tag = "latest"
		}
		imageRef := cSpec.Image + ":" + tag

		replicas := cSpec.Replicas
		if replicas <= 0 {
			replicas = 1
		}

		e.reportProgress(spec.DeploymentID, "deploying",
			fmt.Sprintf("[%d/%d] pulling image %s for container %s", i+1, len(spec.Containers), imageRef, cSpec.Name),
			map[string]any{"container_name": cSpec.Name, "step": "pulling"})

		// Pull the image
		var regAuth *dockerclient.RegistryAuth
		if cSpec.RegistryAuth != nil {
			regAuth = &dockerclient.RegistryAuth{
				Username: cSpec.RegistryAuth.Username,
				Password: cSpec.RegistryAuth.Password,
			}
		}

		log.Printf("deploy: pulling image %s", imageRef)
		if err := e.Docker.PullImage(ctx, imageRef, regAuth); err != nil {
			e.rollbackContainers(ctx, spec, snapshots, updatedContainers)
			return fmt.Errorf("pull image %s, rolled back %d containers: %w", imageRef, len(updatedContainers), err)
		}

		e.reportProgress(spec.DeploymentID, "deploying",
			fmt.Sprintf("[%d/%d] image %s pulled successfully", i+1, len(spec.Containers), imageRef),
			map[string]any{"container_name": cSpec.Name, "step": "pulled"})

		for replica := 0; replica < replicas; replica++ {
			name := cSpec.Name
			if replicas > 1 {
				name = fmt.Sprintf("%s-%d", cSpec.Name, replica+1)
			}

			// Stop and remove old container if it exists
			existingID, err := e.Docker.FindContainerByName(ctx, name)
			if err != nil {
				log.Printf("deploy: error finding container %s: %v", name, err)
			}
			if existingID != "" {
				// Rename the old container first so the name is immediately free
				retiredName := name + "-lattice-retired"
				if renameErr := e.Docker.RenameContainer(ctx, existingID, retiredName); renameErr != nil {
					log.Printf("deploy: rename failed for %s: %v — will try force remove", name, renameErr)
				}

				e.reportProgress(spec.DeploymentID, "deploying",
					fmt.Sprintf("[%d/%d] stopping old container: %s", i+1, len(spec.Containers), name),
					map[string]any{"container_name": name, "step": "stopping"})
				if err := e.Docker.StopContainer(ctx, existingID, 30); err != nil {
					log.Printf("deploy: error stopping container %s: %v", name, err)
				}
				if err := e.Docker.RemoveContainer(ctx, existingID, true); err != nil {
					log.Printf("deploy: error removing container %s: %v", name, err)
				}
			} else {
				e.reportProgress(spec.DeploymentID, "deploying",
					fmt.Sprintf("[%d/%d] no existing container found for %s, creating new", i+1, len(spec.Containers), name),
					map[string]any{"container_name": name, "step": "new"})
			}

			// Create and start new container
			e.reportProgress(spec.DeploymentID, "deploying",
				fmt.Sprintf("[%d/%d] creating and starting container: %s", i+1, len(spec.Containers), name),
				map[string]any{"container_name": name, "step": "starting"})

			portMappings := make([]dockerclient.PortMapping, len(cSpec.PortMappings))
			for j, pm := range cSpec.PortMappings {
				portMappings[j] = dockerclient.PortMapping{
					HostPort:      pm.HostPort,
					ContainerPort: pm.ContainerPort,
					Protocol:      pm.Protocol,
				}
			}

			dockerSpec := dockerclient.ContainerSpec{
				Name:          name,
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

			// Try to create, with one retry after a delay (port release can be slow)
			containerID, err := e.Docker.CreateAndStartContainer(ctx, dockerSpec)
			if err != nil {
				log.Printf("deploy: first create attempt failed for %s: %v — retrying in 3s", name, err)
				time.Sleep(3 * time.Second)
				containerID, err = e.Docker.CreateAndStartContainer(ctx, dockerSpec)
			}
			if err != nil {
				e.rollbackContainers(ctx, spec, snapshots, updatedContainers)
				return fmt.Errorf("create container %s, rolled back %d containers: %w", name, len(updatedContainers), err)
			}

			log.Printf("deploy: container %s started (id=%s)", name, containerID[:12])
			e.reportProgress(spec.DeploymentID, "deploying",
				fmt.Sprintf("container %s started successfully", name),
				map[string]any{"container_name": name, "step": "running", "container_id": containerID})

			updatedContainers = append(updatedContainers, snapshotIdx)
			snapshotIdx++
		}
	}

	return nil
}

// rollbackContainers restores previously-updated containers to their original image.
func (e *Executor) rollbackContainers(ctx context.Context, spec DeploymentSpec, snapshots []containerSnapshot, updatedContainers []int) {
	if len(updatedContainers) == 0 {
		return
	}

	e.reportProgress(spec.DeploymentID, "failed",
		fmt.Sprintf("rolling back %d containers", len(updatedContainers)), nil)

	for j := len(updatedContainers) - 1; j >= 0; j-- {
		idx := updatedContainers[j]
		snap := snapshots[idx]
		cSpec := spec.Containers[snap.SpecIdx]

		e.reportProgress(spec.DeploymentID, "deploying",
			fmt.Sprintf("rollback: restoring %s to %s", snap.Name, snap.OldImage), nil)

		// Pull old image
		if err := e.Docker.PullImage(ctx, snap.OldImage, nil); err != nil {
			log.Printf("deploy: rollback pull failed for %s: %v", snap.Name, err)
		}

		// Stop and remove the new (broken) container
		if oldID, findErr := e.Docker.FindContainerByName(ctx, snap.Name); findErr == nil && oldID != "" {
			_ = e.Docker.StopContainer(ctx, oldID, 10)
			_ = e.Docker.RemoveContainer(ctx, oldID, true)
		}

		// Recreate with old image but full original spec (ports, env, volumes, etc.)
		portMappings := make([]dockerclient.PortMapping, len(cSpec.PortMappings))
		for k, pm := range cSpec.PortMappings {
			portMappings[k] = dockerclient.PortMapping{
				HostPort:      pm.HostPort,
				ContainerPort: pm.ContainerPort,
				Protocol:      pm.Protocol,
			}
		}

		dockerSpec := dockerclient.ContainerSpec{
			Name:          snap.Name,
			Image:         snap.OldImage,
			Tag:           "", // OldImage already contains tag
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
		_, createErr := e.Docker.CreateAndStartContainer(ctx, dockerSpec)
		if createErr != nil {
			log.Printf("deploy: rollback failed for %s: %v", snap.Name, createErr)
		}
	}
}
