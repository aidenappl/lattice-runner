package telemetry

import (
	"context"
	"log/slog"
	"runtime"
	"time"
)

// LogAt logs msg at level through the default slog logger, attributed to a
// frame above its caller: skip 0 is the function that called LogAt, 1 is that
// function's caller, and so on. It is for helpers that log on behalf of their
// callers (wsSend, sendLifecycleLog), so the event's source_file/func/line
// name the call site that asked for the work rather than the helper.
//
// args are slog key/value pairs, as for slog.Info. It follows the log/slog
// "wrapping output methods" example.
func LogAt(ctx context.Context, skip int, level slog.Level, msg string, args ...any) {
	if ctx == nil {
		ctx = context.Background()
	}
	h := slog.Default().Handler()
	if !h.Enabled(ctx, level) {
		return
	}
	var pcs [1]uintptr
	// Skip runtime.Callers and LogAt itself.
	runtime.Callers(skip+2, pcs[:])
	r := slog.NewRecord(time.Now(), level, msg, pcs[0])
	r.Add(args...)
	_ = h.Handle(ctx, r)
}
