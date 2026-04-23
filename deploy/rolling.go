package deploy

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

// containerSnapshot captures the pre-deployment state of a container for rollback.
type containerSnapshot struct {
	Name     string
	OldImage string // full image:tag before upgrade
	SpecIdx  int    // index into spec.Containers
	Replica  int    // which replica
	OldID    string // Docker container ID of old container (for cleanup)
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
			oldID := ""
			// Find existing container — could be exact name or a suffixed variant
			if id, _ := e.findCanonicalContainer(ctx, name); id != "" {
				oldID = id
				if info, err := e.Docker.InspectContainer(ctx, id); err == nil {
					oldImage = info.Config.Image
				}
			}
			snapshots = append(snapshots, containerSnapshot{
				Name:     name,
				OldImage: oldImage,
				SpecIdx:  i,
				Replica:  replica,
				OldID:    oldID,
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

			// Find ALL existing containers matching this canonical name
			oldContainers := e.findAllMatchingContainers(ctx, name)

			// Generate a unique name for the new container
			suffix := dockerclient.GenerateSuffix()
			deployName := name + "-" + suffix

			e.reportProgress(spec.DeploymentID, "deploying",
				fmt.Sprintf("[%d/%d] creating container: %s (as %s)", i+1, len(spec.Containers), name, deployName),
				map[string]any{"container_name": name, "step": "starting"})

			// Build port mappings
			portMappings := make([]dockerclient.PortMapping, len(cSpec.PortMappings))
			for j, pm := range cSpec.PortMappings {
				portMappings[j] = dockerclient.PortMapping{
					HostPort:      pm.HostPort,
					ContainerPort: pm.ContainerPort,
					Protocol:      pm.Protocol,
				}
			}

			// If old containers hold host ports, stop them FIRST to free the ports
			if len(portMappings) > 0 && len(oldContainers) > 0 {
				e.reportProgress(spec.DeploymentID, "deploying",
					fmt.Sprintf("[%d/%d] stopping old container(s) to free ports for %s", i+1, len(spec.Containers), name), nil)
				for _, old := range oldContainers {
					log.Printf("deploy: stopping old container %s (id=%s) to free ports", old.name, old.id[:12])
					if err := e.Docker.StopContainer(ctx, old.id, 10); err != nil {
						log.Printf("deploy: stop failed for %s: %v, trying kill", old.name, err)
						_ = e.Docker.KillContainer(ctx, old.id)
					}
					// Verify stopped
					if info, err := e.Docker.InspectContainer(ctx, old.id); err == nil && info.State.Running {
						log.Printf("deploy: container %s still running, killing", old.name)
						_ = e.Docker.KillContainer(ctx, old.id)
						time.Sleep(1 * time.Second)
					}
				}
			}

			// Create and start new container with unique name
			dockerSpec := dockerclient.ContainerSpec{
				Name:           deployName,
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

			containerID, err := e.Docker.CreateAndStartContainer(ctx, dockerSpec)
			if err != nil {
				e.reportProgress(spec.DeploymentID, "deploying",
					fmt.Sprintf("[%d/%d] failed to create %s: %v", i+1, len(spec.Containers), deployName, err), nil)
				e.rollbackContainers(ctx, spec, snapshots, updatedContainers)
				return fmt.Errorf("create container %s: %w", name, err)
			}

			// Verify new container is running
			if info, err := e.Docker.InspectContainer(ctx, containerID); err != nil || !info.State.Running {
				e.reportProgress(spec.DeploymentID, "deploying",
					fmt.Sprintf("[%d/%d] container %s created but not running", i+1, len(spec.Containers), deployName), nil)
				e.rollbackContainers(ctx, spec, snapshots, updatedContainers)
				return fmt.Errorf("container %s not running after create", name)
			}

			log.Printf("deploy: container %s started (id=%s)", deployName, containerID[:12])

			// Clean up ALL old containers (stop if needed, remove)
			for _, old := range oldContainers {
				log.Printf("deploy: removing old container %s (id=%s)", old.name, old.id[:12])
				if err := e.Docker.StopAndRemoveContainer(ctx, old.id, 10); err != nil {
					log.Printf("deploy: failed to remove old container %s: %v (will be orphaned)", old.name, err)
				}
			}

			// Best-effort rename to canonical name for clean display
			if renameErr := e.Docker.RenameContainer(ctx, containerID, name); renameErr != nil {
				log.Printf("deploy: rename %s -> %s failed: %v (container running with suffixed name)", deployName, name, renameErr)
				// Not fatal — container is running, just with the suffixed name
			} else {
				deployName = name
			}

			e.reportProgress(spec.DeploymentID, "deploying",
				fmt.Sprintf("container %s started successfully", deployName),
				map[string]any{"container_name": name, "step": "running", "container_id": containerID})

			updatedContainers = append(updatedContainers, snapshotIdx)
			snapshotIdx++
		}
	}

	return nil
}

// oldContainer tracks an existing container found by canonical name matching.
type oldContainer struct {
	id   string
	name string
}

// findCanonicalContainer finds a container by exact name or by canonical prefix with suffix.
func (e *Executor) findCanonicalContainer(ctx context.Context, canonicalName string) (string, string) {
	// Try exact name first
	if id, err := e.Docker.FindContainerByName(ctx, canonicalName); err == nil && id != "" {
		return id, canonicalName
	}
	// Try finding by prefix (canonical name + suffix pattern)
	containers := e.findAllMatchingContainers(ctx, canonicalName)
	if len(containers) > 0 {
		return containers[0].id, containers[0].name
	}
	return "", ""
}

// findAllMatchingContainers finds all containers that match the canonical name.
// This includes: exact match, suffixed variants (name-XXXXXX), and orphan variants (name-retired-*).
func (e *Executor) findAllMatchingContainers(ctx context.Context, canonicalName string) []oldContainer {
	all, err := e.Docker.ListContainers(ctx, "")
	if err != nil {
		return nil
	}

	var matches []oldContainer
	for _, ct := range all {
		for _, n := range ct.Names {
			cName := strings.TrimPrefix(n, "/")
			if cName == canonicalName || isCanonicalVariant(canonicalName, cName) {
				matches = append(matches, oldContainer{id: ct.ID, name: cName})
				break
			}
		}
	}
	return matches
}

// isCanonicalVariant checks if dockerName is a variant of canonicalName.
// Matches: name-XXXXXX (suffix), name-retired-*, name-lattice-updating
func isCanonicalVariant(canonicalName, dockerName string) bool {
	if !strings.HasPrefix(dockerName, canonicalName+"-") {
		return false
	}
	rest := dockerName[len(canonicalName)+1:]
	// Retired/updating patterns
	if strings.HasPrefix(rest, "retired") || rest == "lattice-updating" {
		return true
	}
	// 6-char alphanumeric suffix (our deploy suffix)
	if len(rest) == 6 {
		for _, c := range rest {
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
				return false
			}
		}
		return true
	}
	return false
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

		// Stop and remove all current containers with this canonical name
		for _, old := range e.findAllMatchingContainers(ctx, snap.Name) {
			_ = e.Docker.StopAndRemoveContainer(ctx, old.id, 10)
		}

		// Recreate with old image and canonical name
		portMappings := make([]dockerclient.PortMapping, len(cSpec.PortMappings))
		for k, pm := range cSpec.PortMappings {
			portMappings[k] = dockerclient.PortMapping{
				HostPort:      pm.HostPort,
				ContainerPort: pm.ContainerPort,
				Protocol:      pm.Protocol,
			}
		}

		// Try with suffixed name first, then rename
		rollbackSuffix := dockerclient.GenerateSuffix()
		rollbackName := snap.Name + "-" + rollbackSuffix

		dockerSpec := dockerclient.ContainerSpec{
			Name:           rollbackName,
			Image:          snap.OldImage,
			Tag:            "", // OldImage already contains tag
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
		containerID, createErr := e.Docker.CreateAndStartContainer(ctx, dockerSpec)
		if createErr != nil {
			log.Printf("deploy: rollback create failed for %s: %v", snap.Name, createErr)
			continue
		}
		// Best-effort rename to canonical
		if renameErr := e.Docker.RenameContainer(ctx, containerID, snap.Name); renameErr != nil {
			log.Printf("deploy: rollback rename %s -> %s failed: %v", rollbackName, snap.Name, renameErr)
		}
	}
}

// CleanupOrphanedContainers finds containers with deploy suffixes, -retired-, or -lattice-updating
// patterns that are leftovers from failed deploys and returns their names.
// It does NOT remove them — the caller decides what to do.
func (e *Executor) CleanupOrphanedContainers(ctx context.Context) []string {
	containers, err := e.Docker.ListContainers(ctx, "")
	if err != nil {
		log.Printf("deploy: failed to list containers for orphan check: %v", err)
		return nil
	}

	var orphans []string
	for _, ct := range containers {
		for _, n := range ct.Names {
			name := strings.TrimPrefix(n, "/")
			if strings.Contains(name, "-retired-") ||
				strings.HasSuffix(name, "-lattice-retired") ||
				strings.HasSuffix(name, "-lattice-updating") {
				orphans = append(orphans, name)
				log.Printf("deploy: orphaned container detected: %s (state=%s)", name, ct.State)
			}
		}
	}
	return orphans
}
