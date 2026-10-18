package deploy

import (
	"context"
	"testing"
	"time"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

// acquiredWithin reports whether LockContainer(name) returns within d, and
// releases the lock if it did.
func acquiredWithin(name string, d time.Duration) bool {
	got := make(chan func(), 1)
	go func() { got <- LockContainer(context.Background(), name) }()
	select {
	case unlock := <-got:
		unlock()
		return true
	case <-time.After(d):
		// Drain once the holder releases, so the goroutine does not leak a lock.
		go func() { (<-got)() }()
		return false
	}
}

func TestLockContainerSerialisesNameVariants(t *testing.T) {
	suffixed := "lockvariant-" + dockerclient.GenerateSuffix()
	tests := []struct {
		name    string
		held    string
		other   string
		blocked bool
	}{
		{"same name", "lockvariant", "lockvariant", true},
		{"retired variant of a held name", "lockvariant", "lockvariant-retired-1792262579961593623", true},
		{"deploy-suffixed variant of a held name", "lockvariant", suffixed, true},
		{"updating variant of a held name", "lockvariant", "lockvariant-lattice-updating", true},
		{"a different container", "lockvariant", "lockvariant-worker", false},
		{"an unrelated container", "lockvariant", "other-service", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unlock := LockContainer(context.Background(), tt.held)
			got := !acquiredWithin(tt.other, 100*time.Millisecond)
			unlock()
			if got != tt.blocked {
				t.Errorf("lock on %q while %q is held: blocked = %v, want %v", tt.other, tt.held, got, tt.blocked)
			}
		})
	}
}

func TestLockContainerReleasesToTheWaiter(t *testing.T) {
	unlock := LockContainer(context.Background(), "lockwaiter")
	done := make(chan struct{})
	go func() {
		LockContainer(context.Background(), "lockwaiter-retired-1")()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the waiter acquired the lock while it was held")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the waiter never acquired the lock after release")
	}
}

func TestLockSpecContainersCoversEveryReplica(t *testing.T) {
	spec := DeploymentSpec{Containers: []ContainerSpec{
		{Name: "lockspec-web", Replicas: 2},
		{Name: "lockspec-api"},
	}}
	unlock := lockSpecContainers(context.Background(), spec)

	for _, name := range []string{"lockspec-web-1", "lockspec-web-2", "lockspec-api"} {
		if acquiredWithin(name, 50*time.Millisecond) {
			t.Errorf("%s was lockable while the deployment held it", name)
		}
	}
	if !acquiredWithin("lockspec-db", 50*time.Millisecond) {
		t.Error("a container outside the spec was blocked")
	}

	unlock()
	for _, name := range []string{"lockspec-web-1", "lockspec-web-2", "lockspec-api"} {
		if !acquiredWithin(name, time.Second) {
			t.Errorf("%s stayed locked after the deployment released it", name)
		}
	}
}

func TestLockContainerProceedsAfterTheWaitLimit(t *testing.T) {
	prev := LOCK_WAIT_LIMIT
	LOCK_WAIT_LIMIT = 300 * time.Millisecond
	defer func() { LOCK_WAIT_LIMIT = prev }()

	unlock := LockContainer(context.Background(), "lockstuck")
	defer unlock()

	start := time.Now()
	release := LockContainer(context.Background(), "lockstuck")
	release() // a no-op: it never held the lock, and must not release the holder's
	if waited := time.Since(start); waited < 300*time.Millisecond || waited > 2*time.Second {
		t.Errorf("waited %v, want about the 300ms limit", waited)
	}
	if acquiredWithin("lockstuck", 50*time.Millisecond) {
		t.Error("the timed-out waiter's release freed the holder's lock")
	}
}
