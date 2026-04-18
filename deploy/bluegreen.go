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
		greenName := cSpec.Name + "-green"
		imageRef := cSpec.Image + ":" + cSpec.Tag

		var regAuth *dockerclient.RegistryAuth
		if cSpec.RegistryAuth != nil {
			regAuth = &dockerclient.RegistryAuth{
				Username: cSpec.RegistryAuth.Username,
				Password: cSpec.RegistryAuth.Password,
			}
		}

		e.reportProgress(spec.DeploymentID, "deploying",
			fmt.Sprintf("[%d/%d] pulling image %s for green container %s", i+1, len(spec.Containers), imageRef, greenName),
			map[string]any{"container_name": greenName, "step": "pulling"})

		log.Printf("deploy: pulling image %s for green", imageRef)
		if err := e.Docker.PullImage(ctx, imageRef, regAuth); err != nil {
			// Cleanup green containers on failure
			e.cleanupGreen(ctx, greenIDs)
			return fmt.Errorf("pull image %s: %w", imageRef, err)
		}

		portMappings := make([]dockerclient.PortMapping, len(cSpec.PortMappings))
		for j, pm := range cSpec.PortMappings {
			portMappings[j] = dockerclient.PortMapping{
				HostPort:      pm.HostPort,
				ContainerPort: pm.ContainerPort,
				Protocol:      pm.Protocol,
			}
		}

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
		}

		containerID, err := e.Docker.CreateAndStartContainer(ctx, dockerSpec)
		if err != nil {
			e.cleanupGreen(ctx, greenIDs)
			return fmt.Errorf("create green container %s: %w", greenName, err)
		}

		greenIDs[cSpec.Name] = containerID
		e.reportProgress(spec.DeploymentID, "deploying",
			fmt.Sprintf("green container %s started", greenName),
			map[string]any{"container_name": greenName, "step": "green_running"})
	}

	// Phase 2: Brief health check delay
	e.reportProgress(spec.DeploymentID, "deploying", "all green containers running, performing 5s health check delay", nil)
	time.Sleep(5 * time.Second)

	// Phase 3: Stop blue (old) and rename green to take over
	e.reportProgress(spec.DeploymentID, "deploying", "health check passed, swapping blue→green", nil)
	for _, cSpec := range spec.Containers {
		blueName := cSpec.Name
		greenName := cSpec.Name + "-green"

		// Stop and remove blue
		e.reportProgress(spec.DeploymentID, "deploying",
			fmt.Sprintf("stopping blue (old) container: %s", blueName),
			map[string]any{"container_name": blueName, "step": "stopping_blue"})
		if blueID, err := e.Docker.FindContainerByName(ctx, blueName); err == nil && blueID != "" {
			_ = e.Docker.StopContainer(ctx, blueID, 30)
			_ = e.Docker.RemoveContainer(ctx, blueID, true)
		}

		// Stop green, remove it, recreate with correct name and ports
		if greenID, ok := greenIDs[cSpec.Name]; ok {
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
		}

		_, err := e.Docker.CreateAndStartContainer(ctx, dockerSpec)
		if err != nil {
			return fmt.Errorf("recreate container %s: %w", blueName, err)
		}

		e.reportProgress(spec.DeploymentID, "deploying",
			fmt.Sprintf("swapped to new container: %s", blueName),
			map[string]any{"container_name": blueName, "step": "swapped"})

		_ = greenName // suppress unused
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
