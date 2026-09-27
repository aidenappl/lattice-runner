package telemetry

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	monitor "github.com/aidenappl/go-monitor"
)

const (
	// WARN_LIMIT is how many warnings with the same component and message reach
	// Monitor per WARN_WINDOW. The rest are counted and reported as
	// suppressed=<n> on the next one that goes through.
	WARN_LIMIT  = 20
	WARN_WINDOW = time.Minute

	// warnLimiterSweepAt bounds the limiter's memory: messages still carry
	// variable text (container names, IDs), so keys are not a fixed set.
	warnLimiterSweepAt = 4096
)

// logBridgeWrite is the function slog.SetDefault installs as the standard
// logger's output. A record whose Handle is called from it came from
// log.Printf and friends.
const logBridgeWrite = "log/slog.(*handlerWriter).Write"

// installLogBridge routes the standard logger through slog, and slog through
// runnerHandler to stderr and Monitor. log.Lshortfile must be set before
// slog.SetDefault: that is what makes the bridge capture each log.Printf
// caller's PC, which Monitor turns into source_file/source_func/source_line.
//
// Nothing may call log.SetOutput or log.SetFlags after this: slog.SetDefault
// points the standard logger at slog's handlerWriter and clears its flags, and
// replacing either would disconnect the bridge.
func installLogBridge(debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	log.SetFlags(log.Lshortfile)
	slog.SetDefault(slog.New(newRunnerHandler(
		newLineHandler(os.Stderr, level),
		monitor.NewSlogHandler(nil, &monitor.SlogOptions{Level: level}),
	)))
}

// runnerHandler sits in front of Monitor's slog handler. It writes every
// record to out (stderr) unchanged, and hands Monitor a copy that keeps the
// event names the runner has always used (<component>.log.<level>), carries
// the worker, and has a level for lines that were written without one.
//
// out gets the original record, not the enriched copy, so the journal keeps
// the "[component] message" text and gains no event/worker noise.
type runnerHandler struct {
	out slog.Handler
	mon slog.Handler
	lim *warnLimiter

	// From WithAttrs, only while no group is open.
	component string
	event     string
	grouped   bool
}

func newRunnerHandler(out, mon slog.Handler) *runnerHandler {
	return &runnerHandler{out: out, mon: mon, lim: newWarnLimiter(WARN_LIMIT, WARN_WINDOW, time.Now)}
}

func (h *runnerHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return (h.out != nil && h.out.Enabled(ctx, l)) || h.mon.Enabled(ctx, l)
}

func (h *runnerHandler) Handle(ctx context.Context, r slog.Record) error {
	// The bridge (slog's handlerWriter) emits info records with no attrs, so
	// the level and attr checks let most slog.Info calls skip the frame walk.
	fromBridge := r.Level == slog.LevelInfo && r.NumAttrs() == 0 && calledFromLogBridge()

	var err error
	if h.out != nil && h.out.Enabled(ctx, r.Level) {
		err = h.out.Handle(ctx, r)
	}

	component, event := h.component, h.event
	if !h.grouped {
		r.Attrs(func(a slog.Attr) bool {
			if a.Value.Kind() != slog.KindString {
				return true
			}
			switch a.Key {
			case "component":
				if s := a.Value.String(); s != "" {
					component = s
				}
			case "event":
				if s := a.Value.String(); s != "" {
					event = s
				}
			}
			return true
		})
	}

	msg := r.Message
	hasComponent := component != ""
	if !hasComponent {
		component, msg = parseComponent(msg)
	}
	// Panics are reported with their stack by ReportPanic; the log line that
	// accompanies one would be a second, poorer copy.
	if panicReportLine.MatchString(msg) || (event == "" && strings.TrimSpace(msg) == "") {
		return err
	}

	level := r.Level
	if fromBridge {
		level = slogLevel(classify(msg))
	}
	if !h.mon.Enabled(ctx, level) {
		return err
	}
	mlevel := monitorLevel(level)

	var suppressed int
	if mlevel == monitor.LevelWarn {
		var ok bool
		if ok, suppressed = h.lim.allow(component + "\x00" + msg); !ok {
			return err
		}
	}

	nr := slog.NewRecord(r.Time, level, msg, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(a)
		return true
	})
	if event == "" {
		name := component
		if name == "" {
			name = "runner"
		}
		nr.AddAttrs(slog.String("event", name+".log."+mlevel))
	}
	if !hasComponent {
		nr.AddAttrs(slog.String("component", component))
	}
	nr.AddAttrs(slog.String("worker", workerName()))
	if suppressed > 0 {
		nr.AddAttrs(slog.Int("suppressed", suppressed))
	}
	if merr := h.mon.Handle(ctx, nr); err == nil {
		err = merr
	}
	return err
}

func (h *runnerHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	c := *h
	if !c.grouped {
		for _, a := range attrs {
			if a.Value.Kind() != slog.KindString || a.Value.String() == "" {
				continue
			}
			switch a.Key {
			case "component":
				c.component = a.Value.String()
			case "event":
				c.event = a.Value.String()
			}
		}
	}
	if c.out != nil {
		c.out = c.out.WithAttrs(attrs)
	}
	c.mon = c.mon.WithAttrs(attrs)
	return &c
}

func (h *runnerHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.grouped = true
	if c.out != nil {
		c.out = c.out.WithGroup(name)
	}
	c.mon = c.mon.WithGroup(name)
	return &c
}

// calledFromLogBridge reports whether runnerHandler.Handle was called by the
// standard logger's slog bridge, i.e. the record is a log.Printf line with no
// level of its own. It looks at the few frames above itself by name, so it
// holds whether or not it is inlined into Handle.
func calledFromLogBridge() bool {
	var pcs [4]uintptr
	n := runtime.Callers(1, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if f.Function == logBridgeWrite {
			return true
		}
		if !more {
			return false
		}
	}
}

// parseComponent takes the component from the runner's "[name] …" or
// "name: …" conventions. The bracket form is stripped from the message; the
// colon form is part of the sentence and stays.
func parseComponent(line string) (component, msg string) {
	msg = line
	if m := bracketComponent.FindStringSubmatch(msg); m != nil {
		return m[1], msg[len(m[0]):]
	}
	if m := colonComponent.FindStringSubmatch(msg); m != nil {
		return m[1], msg
	}
	return "", msg
}

// monitorLevel mirrors go-monitor's slog level mapping, which the handler
// applies to the same record, so the event name and level always agree.
func monitorLevel(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return monitor.LevelDebug
	case l < slog.LevelWarn:
		return monitor.LevelInfo
	case l < slog.LevelError:
		return monitor.LevelWarn
	case l < slog.LevelError+4:
		return monitor.LevelError
	default:
		return monitor.LevelFatal
	}
}

func slogLevel(level string) slog.Level {
	switch level {
	case monitor.LevelDebug:
		return slog.LevelDebug
	case monitor.LevelWarn:
		return slog.LevelWarn
	case monitor.LevelError:
		return slog.LevelError
	case monitor.LevelFatal:
		return slog.LevelError + 4
	default:
		return slog.LevelInfo
	}
}

// warnLimiter lets through at most limit warnings per key per window. What it
// holds back is counted and handed to the next warning that gets through.
// Errors never reach it: they are never sampled.
type warnLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	keys   map[string]*warnWindow
}

type warnWindow struct {
	start      time.Time
	sent       int
	suppressed int
}

func newWarnLimiter(limit int, window time.Duration, now func() time.Time) *warnLimiter {
	return &warnLimiter{limit: limit, window: window, now: now, keys: map[string]*warnWindow{}}
}

// allow reports whether a warning with this key may be sent and, if so, how
// many were suppressed before it.
func (l *warnLimiter) allow(key string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.keys[key]
	if w == nil {
		if len(l.keys) >= warnLimiterSweepAt {
			l.sweep(now)
		}
		w = &warnWindow{start: now}
		l.keys[key] = w
	} else if now.Sub(w.start) >= l.window {
		w.start, w.sent = now, 0
	}
	if w.sent >= l.limit {
		w.suppressed++
		return false, 0
	}
	w.sent++
	n := w.suppressed
	w.suppressed = 0
	return true, n
}

// sweep drops keys whose window has ended with nothing pending, and, if that
// is not enough, everything: forgetting a count beats unbounded growth.
func (l *warnLimiter) sweep(now time.Time) {
	for k, w := range l.keys {
		if now.Sub(w.start) >= l.window && w.suppressed == 0 {
			delete(l.keys, k)
		}
	}
	if len(l.keys) >= warnLimiterSweepAt {
		clear(l.keys)
	}
}

// lineHandler writes records as the standard logger did with
// log.LstdFlags|log.Lshortfile — "2006/01/02 15:04:05 file.go:12: message" —
// so the journal reads as it always has. A level other than info is written
// before the message and attributes after it as key=value.
//
// It writes to w directly: it sits under slog.SetDefault, so writing through
// the log package would feed its own output back to it.
type lineHandler struct {
	mu     *sync.Mutex
	w      io.Writer
	level  slog.Leveler
	attrs  []byte // preformatted " k=v" from WithAttrs
	prefix string // "group." from WithGroup
}

func newLineHandler(w io.Writer, level slog.Leveler) *lineHandler {
	return &lineHandler{mu: &sync.Mutex{}, w: w, level: level}
}

func (h *lineHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level.Level()
}

func (h *lineHandler) Handle(_ context.Context, r slog.Record) error {
	buf := make([]byte, 0, 128+len(r.Message))
	buf = r.Time.AppendFormat(buf, "2006/01/02 15:04:05")
	buf = append(buf, ' ')
	if r.PC != 0 {
		f, _ := runtime.CallersFrames([]uintptr{r.PC}).Next()
		buf = append(buf, filepath.Base(f.File)...)
		buf = append(buf, ':')
		buf = strconv.AppendInt(buf, int64(f.Line), 10)
		buf = append(buf, ": "...)
	}
	if r.Level != slog.LevelInfo {
		buf = append(buf, r.Level.String()...)
		buf = append(buf, ' ')
	}
	buf = append(buf, r.Message...)
	buf = append(buf, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		buf = appendAttr(buf, h.prefix, a)
		return true
	})
	buf = append(buf, '\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(buf)
	return err
}

func (h *lineHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append([]byte(nil), h.attrs...)
	for _, a := range attrs {
		c.attrs = appendAttr(c.attrs, h.prefix, a)
	}
	return &c
}

func (h *lineHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.prefix = h.prefix + name + "."
	return &c
}

func appendAttr(buf []byte, prefix string, a slog.Attr) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return buf
	}
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p = prefix + a.Key + "."
		}
		for _, ga := range a.Value.Group() {
			buf = appendAttr(buf, p, ga)
		}
		return buf
	}
	buf = append(buf, ' ')
	buf = append(buf, prefix...)
	buf = append(buf, a.Key...)
	buf = append(buf, '=')
	s := a.Value.String()
	if s == "" || strings.ContainsAny(s, " =\"\t\n") || !strconv.CanBackquote(s) {
		return strconv.AppendQuote(buf, s)
	}
	return append(buf, s...)
}
