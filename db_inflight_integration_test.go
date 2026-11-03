//go:build integration

package main

import (
	"context"
	"os"
	"testing"
	"time"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

// TestDeleteMidPullLeavesNothing is APL-1 against a real Docker daemon: a
// remove that arrives while db_create is still pulling must leave neither the
// container nor the data volume behind once the create has returned.
//
// It follows the handlers' sequence (beginDbCreate → CreateDatabaseContainer;
// beginDbRemove → find, remove container, purge volume) and needs network
// access to pull an image that is not cached. Override the image with
// LATTICE_IT_DB_IMAGE_TAG (a postgres tag) when the default is already local.
//
//	go test -tags=integration -run TestDeleteMidPull -count=1 .
func TestDeleteMidPullLeavesNothing(t *testing.T) {
	docker, err := dockerclient.NewClient()
	if err != nil {
		t.Skipf("no docker: %v", err)
	}
	tag := os.Getenv("LATTICE_IT_DB_IMAGE_TAG")
	if tag == "" {
		tag = "17-alpine"
	}
	name := "lattice-db-it-apl1-" + time.Now().Format("150405")
	volume := "lattice-dbdata-it-apl1-" + time.Now().Format("150405")
	ctx := context.Background()

	createDone := make(chan error, 1)
	go func() {
		createCtx, finish := beginDbCreate(ctx, name)
		defer finish()
		_, err := docker.CreateDatabaseContainer(createCtx, dockerclient.DatabaseSpec{
			InstanceID: 1, ContainerName: name, VolumeName: volume,
			Engine: "postgres", EngineVersion: tag, Port: 25432,
			RootPassword: "it-root", DatabaseName: "it", Username: "it", Password: "it-pass",
		})
		createDone <- err
	}()

	// Land the delete mid-pull.
	time.Sleep(300 * time.Millisecond)
	cancelled, finishRemove := beginDbRemove(name)
	if id, err := docker.FindContainerByName(ctx, name); err != nil {
		t.Fatalf("find: %v", err)
	} else if id != "" {
		_ = docker.StopContainer(ctx, id, 10)
		if err := docker.RemoveContainer(ctx, id, true); err != nil {
			t.Fatalf("remove container: %v", err)
		}
	}
	if err := docker.RemoveVolume(ctx, volume, true); err != nil {
		t.Fatalf("remove volume: %v", err)
	}
	finishRemove()

	select {
	case err := <-createDone:
		if err == nil {
			t.Fatal("create reported success after being cancelled by a remove")
		}
		t.Logf("create ended: %v (remove cancelled a create: %v)", err, cancelled)
	case <-time.After(6 * time.Minute):
		t.Fatal("create never returned")
	}

	// "After the create would have finished": give any straggling Docker call
	// time to land, then nothing may exist.
	time.Sleep(2 * time.Second)
	if id, _ := docker.FindContainerByName(ctx, name); id != "" {
		_ = docker.RemoveContainer(ctx, id, true)
		t.Fatalf("container %s was left behind", name)
	}
	vols, err := docker.ListDatabaseVolumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vols {
		if v == volume {
			_ = docker.RemoveVolume(ctx, volume, true)
			t.Fatalf("volume %s was left behind", volume)
		}
	}
}
