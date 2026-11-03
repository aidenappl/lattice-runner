package main

import (
	"context"
	"testing"
	"time"
)

// A remove sent mid-create must cancel the create and run only after it has
// returned, so it can see (and destroy) whatever the create produced (APL-1).
func TestRemoveCancelsAndWaitsForInflightCreate(t *testing.T) {
	createCtx, finishCreate := beginDbCreate(context.Background(), "lattice-db-x")

	produced := make(chan struct{})
	go func() {
		// The create notices the cancel, then still "leaves something behind"
		// (as a create past ContainerCreate would) before returning.
		<-createCtx.Done()
		time.Sleep(20 * time.Millisecond)
		close(produced)
		finishCreate()
	}()

	cancelled, finishRemove := beginDbRemove("lattice-db-x")
	defer finishRemove()
	if !cancelled {
		t.Fatal("remove did not report cancelling the in-flight create")
	}
	select {
	case <-produced:
	default:
		t.Fatal("remove proceeded before the create returned")
	}
}

func TestRemoveWithoutCreateDoesNotWait(t *testing.T) {
	done := make(chan bool)
	go func() {
		cancelled, finish := beginDbRemove("lattice-db-y")
		finish()
		done <- cancelled
	}()
	select {
	case cancelled := <-done:
		if cancelled {
			t.Fatal("reported a cancelled create when none was in flight")
		}
	case <-time.After(time.Second):
		t.Fatal("remove blocked with no create in flight")
	}
}

// A create arriving while a remove holds the name waits for it rather than
// racing it, and the registry forgets names once nothing uses them.
func TestCreateWaitsForRemoveAndRegistryEmpties(t *testing.T) {
	_, finishRemove := beginDbRemove("lattice-db-z")
	started := make(chan struct{})
	go func() {
		_, finish := beginDbCreate(context.Background(), "lattice-db-z")
		close(started)
		finish()
	}()
	select {
	case <-started:
		t.Fatal("create started while the remove held the name")
	case <-time.After(30 * time.Millisecond):
	}
	finishRemove()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("create never started after the remove finished")
	}
	time.Sleep(10 * time.Millisecond)
	dbOpsMu.Lock()
	n := len(dbOps)
	dbOpsMu.Unlock()
	if n != 0 {
		t.Fatalf("registry kept %d names", n)
	}
}
