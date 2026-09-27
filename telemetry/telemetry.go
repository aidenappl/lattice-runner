// Package telemetry connects lattice-runner to Monitor.
//
// Monitor runs as containers on Lattice workers — possibly on this one — so it
// can never be something the runner waits for. Nothing here touches the network
// at startup or fails, and events are spooled on disk until Monitor answers:
// through a Monitor outage, a restart of this process, or the redeploy of the
// very container that hosts Monitor.
package telemetry

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"runtime/debug"
	"sync/atomic"

	monitor "github.com/aidenappl/go-monitor"
	"github.com/aidenappl/lattice-runner/config"
)

// sdkDropTotal is the shipper's running drop total, as last reported to
// OnDrop. OnDrop runs on the goroutine that hit the drop — for a full buffer,
// whoever called Emit — so it only stores a number.
var sdkDropTotal atomic.Int64

func recordSDKDrops(total int64) {
	sdkDropTotal.Store(total)
}

// SDKStats reports go-monitor's shipper counters and the drop total OnDrop last
// saw. The two drop counts agree; OnDrop's is kept because it is what is
// reported the moment a drop happens.
func SDKStats() (monitor.ShipperStats, int64) {
	return monitor.Stats(), sdkDropTotal.Load()
}

// Service is the name every runner event is filed under. Which worker sent it
// is the "worker" field: one failure on two workers is one issue.
const Service = "lattice-runner"

var worker atomic.Pointer[string]

func workerName() string {
	if w := worker.Load(); w != nil {
		return *w
	}
	return ""
}

// Init configures Monitor and routes the standard logger into it through
// slog (see installLogBridge). It never
// returns an error: a misconfiguration is logged and the runner carries on.
func Init(version string, cfg config.Monitor) {
	w := cfg.WorkerName
	worker.Store(&w)

	err := monitor.Init(monitor.Config{
		Service:       Service,
		Env:           cfg.Env,
		Zone:          cfg.Zone,
		IngestURL:     cfg.IngestURL,
		APIKey:        cfg.APIKey,
		SpoolDir:      cfg.SpoolDir,
		Debug:         cfg.Debug,
		DisableStdout: !cfg.Stdout,
		GzipEnabled:   true,
		OnDrop:        recordSDKDrops,
	})
	if err != nil {
		log.Printf("telemetry: Monitor disabled: %v", err)
		return
	}
	installLogBridge(cfg.Debug)
	if cfg.IngestURL == "" {
		log.Printf("telemetry: MONITOR_INGEST_URL is not set; events are not being shipped")
	}
	emit(monitor.LevelInfo, "service.startup", map[string]any{
		"version": version,
		"spool":   cfg.SpoolDir != "",
	})
}

var (
	bracketComponent = regexp.MustCompile(`^\[([A-Za-z0-9_-]+)\] ?`)
	colonComponent   = regexp.MustCompile(`^([a-z][a-z0-9_-]*): `)
)

var (
	// softFailure is an error the code already handles — a retry, a fallback.
	// Checked first: these lines usually contain the word "failed" too.
	softFailure = regexp.MustCompile(`(?i)(\battempt \d+|\bretry|\bretrying|trying kill|falling back|may already exist|already absent|will be orphaned|stopping in place|skipping|ignoring duplicate|will reconnect|proceeding)`)
	// "aborted"/"refusing" are the runner's words for a failure it stopped on
	// deliberately (e.g. an upgrade whose script hash does not match).
	hardFailure = regexp.MustCompile(`(?i)\b(fail|failed|failure|fails|error|errors|cannot|can't|unable|refused|fatal|corrupt|aborted|refusing)\b`)
	// "could not be stopped/started" is the stop_all/start_all summary when
	// some containers failed: each failure is already logged at error, so the
	// summary is a warning rather than a second error. It is matched as an exact
	// phrase so no other "could not …" line changes level.
	caution = regexp.MustCompile(`(?i)\b(invalid|rejected|not found|orphan|orphaned|timed out|timeout|full|dropped|denied|missing|warning|stale|offline|disconnected|rejecting|could not be stopped|could not be started)\b`)
)

// classify picks a level for a line that was written without one. Only error
// and fatal events become Monitor issues, so "failed" lines are errors unless
// they say how they were handled.
func classify(msg string) string {
	switch {
	case softFailure.MatchString(msg):
		return monitor.LevelWarn
	case hardFailure.MatchString(msg):
		return monitor.LevelError
	case caution.MatchString(msg):
		return monitor.LevelWarn
	default:
		return monitor.LevelInfo
	}
}

// panicReportLine matches only the runner's own panic log lines, which are
// always written as "[<goroutine>] PANIC…: <value>\n<stack>" next to a
// ReportPanic/ReportCrash call (Recover, safeGo, safeGoResilient and the message
// handler). The goroutine name may contain ':' ("handler:deploy"), which
// bracketComponent does not strip, so the bracket is matched here. Anything else
// mentioning PANIC — e.g. Postgres stderr "PANIC: …" — has no goroutine stack
// and is a real log line that must still be emitted.
var panicReportLine = regexp.MustCompile(`(?s)^(?:\[[^\]\n]+\] )?PANIC\b.*?\ngoroutine \d+ \[`)

// Event sends one event at an explicit level, stamped with the worker like
// every other runner event. Use it where a log line would be misclassified or
// would carry data (script output, IDs) that belongs in a field.
func Event(level, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	emit(level, name, data)
}

// emit stamps the worker on every event the runner sends.
//
// Explicit events keep going straight to monitor.Emit. Its source_* fields are
// the frame two above it, which is this function (telemetry.go, emit), not the
// caller of Event or ReportPanic; log lines get their true call site through
// the slog bridge instead (see installLogBridge).
func emit(level, name string, data map[string]any) {
	data["worker"] = workerName()
	monitor.Emit(context.Background(), name, data, monitor.WithLevel(level))
}

// ReportPanic reports a value the caller has already recovered. Call it from
// the deferred function that recovered, so the stack still shows where the
// panic happened.
func ReportPanic(goroutine string, recovered any, fields map[string]any) {
	data := make(map[string]any, len(fields)+5)
	for k, v := range fields {
		data[k] = v
	}
	data["goroutine"] = goroutine
	data["error"] = fmt.Sprint(recovered)
	data["panic_type"] = fmt.Sprintf("%T", recovered)
	data["stacktrace"] = string(debug.Stack())
	emit(monitor.LevelError, "panic.recovered", data)
}

// Recover recovers a panic in the calling goroutine, reports it, and lets the
// goroutine end. It must be deferred directly —
//
//	defer telemetry.Recover("handler:deploy", fields)
//
// — because recover only stops a panic when the deferred function itself calls
// it. A panic nothing recovers kills the runner, and every container operation
// in flight on this worker with it.
func Recover(goroutine string, fields map[string]any) {
	if rec := recover(); rec != nil {
		ReportPanic(goroutine, rec, fields)
		log.Printf("[%s] PANIC (recovered): %v\n%s", goroutine, rec, debug.Stack())
	}
}

// ReportCrash reports a panic the runner is about to exit over, and flushes.
func ReportCrash(goroutine string, recovered any) {
	ReportPanic(goroutine, recovered, nil)
	emit(monitor.LevelFatal, "service.crashed", map[string]any{
		"goroutine": goroutine,
		"error":     fmt.Sprint(recovered),
	})
	monitor.Shutdown()
}

// CrashGuard reports a panic that is about to kill the process, then lets it,
// for goroutines where crashing is the intended outcome. Defer it directly.
func CrashGuard(goroutine string) {
	rec := recover()
	if rec == nil {
		return
	}
	ReportCrash(goroutine, rec)
	panic(rec)
}

// Fatal reports a boot failure, flushes, and exits.
func Fatal(name, msg string, err error) {
	data := map[string]any{"message": msg}
	if err != nil {
		data["error"] = err.Error()
	}
	emit(monitor.LevelFatal, name, data)
	monitor.Shutdown()
	if err != nil {
		log.Fatal(msg, ": ", err)
	}
	log.Fatal(msg)
}

// Shutdown announces the stop and delivers (or spools) what is buffered.
// Bounded: it never holds the process for more than a few seconds.
func Shutdown(reason string) {
	emit(monitor.LevelInfo, "service.shutdown", map[string]any{"reason": reason})
	monitor.Shutdown()
}
