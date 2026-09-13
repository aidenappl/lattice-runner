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
	"io"
	"log"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"sync/atomic"

	monitor "github.com/aidenappl/go-monitor"
	"github.com/aidenappl/lattice-runner/config"
)

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

// Init configures Monitor and tees the standard logger into it. It never
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
	})
	if err != nil {
		log.Printf("telemetry: Monitor disabled: %v", err)
		return
	}
	InstallLogTee()
	if cfg.IngestURL == "" {
		log.Printf("telemetry: MONITOR_INGEST_URL is not set; events are not being shipped")
	}
	emit(monitor.LevelInfo, "service.startup", map[string]any{
		"version": version,
		"spool":   cfg.SpoolDir != "",
	})
}

// InstallLogTee sends every standard-library log line to Monitor as well as to
// stderr, and adds the file:line of each call. The runner logs through the
// standard logger everywhere, so this is what makes "every error" true without
// rewriting two hundred call sites.
func InstallLogTee() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetOutput(io.MultiWriter(os.Stderr, logTee{}))
}

type logTee struct{}

// Write receives one log entry per call — the log package serialises them. It
// never fails: a telemetry problem must not become a logging problem.
func (logTee) Write(p []byte) (int, error) {
	emitLogLine(string(p))
	return len(p), nil
}

var (
	stdPrefix        = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)? `)
	callerPrefix     = regexp.MustCompile(`^([\w.-]+\.go:\d+): `)
	bracketComponent = regexp.MustCompile(`^\[([A-Za-z0-9_-]+)\] ?`)
	colonComponent   = regexp.MustCompile(`^([a-z][a-z0-9_-]*): `)
)

// parseLine splits a formatted log line into the call site, a component taken
// from the runner's "[name] …" or "name: …" conventions, and the message.
func parseLine(line string) (caller, component, msg string) {
	msg = strings.TrimRight(line, "\n")
	msg = stdPrefix.ReplaceAllString(msg, "")
	if m := callerPrefix.FindStringSubmatch(msg); m != nil {
		caller = m[1]
		msg = msg[len(m[0]):]
	}
	if m := bracketComponent.FindStringSubmatch(msg); m != nil {
		component = m[1]
		msg = msg[len(m[0]):]
	} else if m := colonComponent.FindStringSubmatch(msg); m != nil {
		component = m[1]
	}
	return caller, component, msg
}

var (
	// softFailure is an error the code already handles — a retry, a fallback.
	// Checked first: these lines usually contain the word "failed" too.
	softFailure = regexp.MustCompile(`(?i)(\battempt \d+|\bretry|\bretrying|trying kill|falling back|may already exist|already absent|will be orphaned|stopping in place|skipping|ignoring duplicate)`)
	hardFailure = regexp.MustCompile(`(?i)\b(fail|failed|failure|fails|error|errors|cannot|can't|unable|refused|fatal|corrupt)\b`)
	caution     = regexp.MustCompile(`(?i)\b(invalid|rejected|not found|orphan|orphaned|timed out|timeout|full|dropped|denied|missing|warning|stale|offline|disconnected)\b`)
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

func emitLogLine(line string) {
	caller, component, msg := parseLine(line)
	// Panics are reported with their stack by ReportPanic; the log line that
	// accompanies one would be a second, poorer copy.
	if strings.TrimSpace(msg) == "" || strings.Contains(msg, "PANIC") {
		return
	}
	level := classify(msg)
	name := component
	if name == "" {
		name = "runner"
	}
	emit(level, name+".log."+level, map[string]any{
		"message":   msg,
		"component": component,
		"caller":    caller,
	})
}

// emit stamps the worker on every event the runner sends.
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
