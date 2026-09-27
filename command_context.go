package main

import (
	"context"
	"fmt"
	"strconv"

	monitor "github.com/aidenappl/go-monitor"
	"github.com/aidenappl/lattice-runner/client"
)

// commandKey marks a context as belonging to an orchestrator command and holds
// that command's command_id (possibly empty, for an older orchestrator).
type commandKey struct{}

// commandContext derives the context a command handler runs under.
//
//   - request_id is the envelope's request_id, else its command_id — the first
//     that monitor-core would accept.
//   - trace_id is the envelope's trace_id when valid.
//   - job_id names the unit of work: the deployment / snapshot / restore /
//     database instance the command acts on, else the command_id, else a
//     fresh id.
//
// It is derived from parent, so runner shutdown still cancels it, and it adds
// no deadline. An envelope without the new fields yields a context with no
// request_id or trace_id and replies stamped with nothing, exactly as before.
func commandContext(parent context.Context, env client.Envelope) context.Context {
	ctx := context.WithValue(parent, commandKey{}, env.CommandID)
	if rid := firstCorrelationID(env.RequestID, env.CommandID); rid != "" {
		ctx = monitor.WithRequestID(ctx, rid)
	}
	if tid := firstCorrelationID(env.TraceID); tid != "" {
		ctx = monitor.WithTraceID(ctx, tid)
	}
	return monitor.WithJobID(ctx, commandJobID(env))
}

// dispatchContext is the context for one inbound envelope. "connected" is the
// socket's own event, not an orchestrator command, so it is a hub cycle with a
// fresh job id and its sends (registration) carry no command ids.
func dispatchContext(parent context.Context, env client.Envelope) context.Context {
	if env.Type == "connected" {
		return cycleContext(parent)
	}
	return commandContext(parent, env)
}

// cycleContext gives one iteration of a runner-initiated loop (heartbeat,
// container sync, netmonitor, scheduled run) its own job id so its logs group
// as a unit. It is not a command context: sends under it are not stamped.
func cycleContext(parent context.Context) context.Context {
	return monitor.WithJobID(parent, monitor.NewJobID())
}

// firstCorrelationID returns the first non-empty id monitor-core would accept.
func firstCorrelationID(ids ...string) string {
	for _, id := range ids {
		if id != "" && monitor.ValidCorrelationID(id) {
			return id
		}
	}
	return ""
}

// Job-id kind tags. monitor-core only accepts a UUID or 8-64 hex characters as
// a job_id, so a bare row id like 42 would be cleared from the event. A unit id
// is therefore rendered as a two-hex-character kind tag followed by the
// zero-padded decimal id — "de00000000000042" is deployment 42 — which is
// valid, 16 characters like a minted id, still readable, and cannot collide
// between a deployment and a database instance that share a number.
const (
	JOB_KIND_DEPLOYMENT = "de"
	JOB_KIND_SNAPSHOT   = "ba"
	JOB_KIND_RESTORE    = "bd"
	JOB_KIND_DATABASE   = "db"
)

// unitJobID renders a numeric unit id as a job_id, or "" if id is not a
// positive integer that fits.
func unitJobID(kind, raw string) string {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return ""
	}
	id := fmt.Sprintf("%s%014d", kind, n)
	if len(id) != 16 {
		return ""
	}
	return id
}

// commandJobID picks the job id for a command. See commandContext.
func commandJobID(env client.Envelope) string {
	p := env.Payload
	var id string
	switch env.Type {
	case "deploy", "deployment_ping":
		id = unitJobID(JOB_KIND_DEPLOYMENT, payloadString(p, "deployment_id"))
	case "db_snapshot", "db_mirror_snapshot", "db_delete_snapshot_file":
		id = unitJobID(JOB_KIND_SNAPSHOT, payloadString(p, "snapshot_id"))
	case "db_restore":
		id = unitJobID(JOB_KIND_RESTORE, payloadString(p, "restore_id", "snapshot_id"))
	}
	if id == "" {
		id = unitJobID(JOB_KIND_DATABASE, payloadString(p, "database_instance_id", "instance_id"))
	}
	if id == "" {
		id = firstCorrelationID(env.CommandID)
	}
	if id == "" {
		id = monitor.NewJobID()
	}
	return id
}

// stampIDs sets command_id, request_id and trace_id on a reply sent under a
// command context, leaving any value the caller set explicitly. Under any
// other context (heartbeat, sync, metrics, logs) it does nothing, so
// unsolicited messages carry no ids.
func stampIDs(ctx context.Context, msg *client.OutgoingMessage) {
	if ctx == nil {
		return
	}
	commandID, ok := ctx.Value(commandKey{}).(string)
	if !ok {
		return
	}
	if msg.CommandID == "" {
		msg.CommandID = commandID
	}
	if msg.RequestID == "" {
		msg.RequestID = monitor.RequestID(ctx)
	}
	if msg.TraceID == "" {
		msg.TraceID = monitor.TraceID(ctx)
	}
}

// stampPayload applies stampIDs to a send payload that is an OutgoingMessage.
func stampPayload(ctx context.Context, payload interface{}) interface{} {
	switch m := payload.(type) {
	case client.OutgoingMessage:
		stampIDs(ctx, &m)
		return m
	case *client.OutgoingMessage:
		if m != nil {
			stampIDs(ctx, m)
		}
		return m
	}
	return payload
}
