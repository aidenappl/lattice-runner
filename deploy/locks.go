package deploy

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	dockerclient "github.com/aidenappl/lattice-runner/docker"
)

// Per-container operation locks.
//
// ─────────────────────────────────────────────────────────────────────────────
// ⚠️ WITHOUT THESE, TWO OPERATIONS ON ONE CONTAINER INTERLEAVED AND ONE FAILED.
//
// A CI deploy token triggers a single-container recreate (GracefulRecreate: the
// old container is renamed "<name>-retired-<ns>", a new "<name>" is started,
// then the retired one is removed). On 2026-10-17 a stack deploy (#321) for the
// same container arrived 13s into one: findAllMatchingContainers saw only the
// retired container, stopped it, and its own create failed with "Bind for
// 0.0.0.0:3030 failed: port is already allocated" — the recreate's new
// container held the port. The recreate then failed to remove the retired
// container the deploy had already rolled back over ("No such container").
//
// Every operation that replaces, stops or removes a container — each deploy
// strategy's per-container step and every container action — takes the lock
// for the container's CANONICAL name first, so a second operation waits for
// the first instead of racing it. Canonical, because the two sides name the
// same container differently ("monitor-web", "monitor-web-ltcxxxxxx",
// "monitor-web-retired-…").
//
// A deployment holds the locks of every container it replaces for its whole
// run (lockSpecContainers), taken in sorted name order, so two deployments
// that share containers cannot deadlock. Container actions take exactly one.
// Locks are never removed from the map: one small entry per container name a
// worker has ever touched.
// ─────────────────────────────────────────────────────────────────────────────

var (
	containerLocksMu sync.Mutex
	containerLocks   = map[string]*sync.Mutex{}
)

// LOCK_WAIT_LIMIT bounds how long an operation waits for a container held by
// another one. The runner puts no timeout on a deployment, so an unbounded wait
// would let one stuck deploy block every action on its containers forever —
// worse than the race the lock prevents. Past the limit the waiter logs a
// warning and proceeds unlocked, which is the pre-lock behaviour.
var LOCK_WAIT_LIMIT = 10 * time.Minute

// lockPollInterval is how often a waiter retries a held lock.
const lockPollInterval = 250 * time.Millisecond

// LockContainer takes the operation lock for a container and returns the
// function that releases it. Use it as `defer deploy.LockContainer(ctx, name)()`.
// A wait is logged, so an operation that is slow because another one held the
// container says so.
func LockContainer(ctx context.Context, name string) (unlock func()) {
	canonical := dockerclient.CanonicalContainerName(name)

	containerLocksMu.Lock()
	mu, ok := containerLocks[canonical]
	if !ok {
		mu = &sync.Mutex{}
		containerLocks[canonical] = mu
	}
	containerLocksMu.Unlock()

	if mu.TryLock() {
		return mu.Unlock
	}
	slog.InfoContext(ctx, "waiting for another operation on this container to finish", "component", "deploy",
		"container", canonical)
	start := time.Now()
	ticker := time.NewTicker(lockPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		if mu.TryLock() {
			slog.InfoContext(ctx, "container lock acquired after waiting", "component", "deploy",
				"container", canonical, "waited_ms", time.Since(start).Milliseconds())
			return mu.Unlock
		}
		if time.Since(start) >= LOCK_WAIT_LIMIT {
			slog.WarnContext(ctx, "container still held after the wait limit; proceeding without the lock", "component", "deploy",
				"container", canonical, "waited_ms", time.Since(start).Milliseconds())
			return func() {}
		}
	}
	return func() {}
}

// lockSpecContainers takes the lock for every container a deployment spec
// replaces (each replica's name, as the strategies name them) in sorted order,
// and returns the function that releases them all.
func lockSpecContainers(ctx context.Context, spec DeploymentSpec) (unlock func()) {
	seen := map[string]bool{}
	var names []string
	for _, c := range spec.Containers {
		replicas := c.Replicas
		if replicas <= 0 {
			replicas = 1
		}
		for r := 0; r < replicas; r++ {
			name := c.Name
			if replicas > 1 {
				name = fmt.Sprintf("%s-%d", c.Name, r+1)
			}
			canonical := dockerclient.CanonicalContainerName(name)
			if !seen[canonical] {
				seen[canonical] = true
				names = append(names, canonical)
			}
		}
	}
	sort.Strings(names)

	unlocks := make([]func(), 0, len(names))
	for _, n := range names {
		unlocks = append(unlocks, LockContainer(ctx, n))
	}
	return func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
}
