package main

import (
	"context"
	"sync"
)

// Managed-database operations on one container name are serialised, and a
// remove cancels a create that is still in flight.
//
// db_create and db_remove each run in their own handler goroutine. Without
// this, a delete sent while a create was still pulling its image found no
// container, reported success (an absent container is the goal of a remove),
// and the control plane retired the record — then the create finished and
// left a running container and a populated data volume that nothing owned
// (APL-1). The next instance created under the same name inherited both.
//
// With it, a remove first cancels the in-flight create's context (aborting the
// pull or the Docker calls), then waits for the name's lock, which the create
// holds until it has returned, so the remove always sees and destroys whatever
// the create managed to produce.

type dbNameOp struct {
	mu     sync.Mutex // held for the whole of a create or remove
	cancel context.CancelFunc
	refs   int
}

var (
	dbOpsMu sync.Mutex
	dbOps   = map[string]*dbNameOp{}
)

func dbOpFor(name string) *dbNameOp {
	dbOpsMu.Lock()
	defer dbOpsMu.Unlock()
	op, ok := dbOps[name]
	if !ok {
		op = &dbNameOp{}
		dbOps[name] = op
	}
	op.refs++
	return op
}

func releaseDbOp(name string, op *dbNameOp) {
	dbOpsMu.Lock()
	defer dbOpsMu.Unlock()
	op.refs--
	if op.refs == 0 {
		delete(dbOps, name)
	}
}

// beginDbCreate takes the name's lock for a create and returns a context a
// remove can cancel. finish must be called when the create has fully returned.
func beginDbCreate(parent context.Context, name string) (ctx context.Context, finish func()) {
	op := dbOpFor(name)
	op.mu.Lock()
	ctx, cancel := context.WithCancel(parent)
	dbOpsMu.Lock()
	op.cancel = cancel
	dbOpsMu.Unlock()
	return ctx, func() {
		dbOpsMu.Lock()
		op.cancel = nil
		dbOpsMu.Unlock()
		cancel()
		op.mu.Unlock()
		releaseDbOp(name, op)
	}
}

// beginDbRemove cancels any in-flight create for name, then waits for the
// name's lock. It reports whether a create was cancelled, so the remove can say
// so in its lifecycle log. finish must be called when the remove is done.
func beginDbRemove(name string) (cancelledCreate bool, finish func()) {
	op := dbOpFor(name)
	dbOpsMu.Lock()
	if op.cancel != nil {
		op.cancel()
		cancelledCreate = true
	}
	dbOpsMu.Unlock()
	op.mu.Lock()
	return cancelledCreate, func() {
		op.mu.Unlock()
		releaseDbOp(name, op)
	}
}
