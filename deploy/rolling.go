package deploy

import (
	"context"
	"fmt"
	"log"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

func (e *Executor) executeRolling(ctx context.Context, spec DeploymentSpec) error {
	for i, cSpec := range spec.Containers {
		e.reportProgress(spec.DeploymentID, "deploying",
			fmt.Sprintf("deploying container %d/%d: %s", i+1, len(spec.Containers), cSpec.Name),
			map[string]any{"container_name": cSpec.Name, "step": "pulling"})

		imageRef := cSpec.Image + ":" + cSpec.Tag

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
			return fmt.Errorf("pull image %s: %w", imageRef, err)
		}

		// Stop and remove old container if it exists
		e.reportProgress(spec.DeploymentID, "deploying",
			fmt.Sprintf("stopping old container: %s", cSpec.Name),
			map[string]any{"container_name": cSpec.Name, "step": "stopping"})

		existingID, err := e.Docker.FindContainerByName(ctx, cSpec.Name)
		if err != nil {
			log.Printf("deploy: error finding container %s: %v", cSpec.Name, err)
		}
		if existingID != "" {
			if err := e.Docker.StopContainer(ctx, existingID, 30); err != nil {
				log.Printf("deploy: error stopping container %s: %v", cSpec.Name, err)
			}
			if err := e.Docker.RemoveContainer(ctx, existingID, true); err != nil {
				log.Printf("deploy: error removing container %s: %v", cSpec.Name, err)
			}
		}

		// Create and start new container
		e.reportProgress(spec.DeploymentID, "deploying",
			fmt.Sprintf("starting new container: %s", cSpec.Name),
			map[string]any{"container_name": cSpec.Name, "step": "starting"})

		portMappings := make([]dockerclient.PortMapping, len(cSpec.PortMappings))
		for j, pm := range cSpec.PortMappings {
			portMappings[j] = dockerclient.PortMapping{
				HostPort:      pm.HostPort,
				ContainerPort: pm.ContainerPort,
				Protocol:      pm.Protocol,
			}
		}

		dockerSpec := dockerclient.ContainerSpec{
			Name:          cSpec.Name,
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

		containerID, err := e.Docker.CreateAndStartContainer(ctx, dockerSpec)
		if err != nil {
			return fmt.Errorf("create container %s: %w", cSpec.Name, err)
		}

		log.Printf("deploy: container %s started (id=%s)", cSpec.Name, containerID[:12])
		e.reportProgress(spec.DeploymentID, "deploying",
			fmt.Sprintf("container %s started successfully", cSpec.Name),
			map[string]any{"container_name": cSpec.Name, "step": "running", "container_id": containerID})
	}

	return nil
}
