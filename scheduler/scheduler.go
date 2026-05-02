package scheduler

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SnapshotFunc is called when a scheduled snapshot should be executed.
type SnapshotFunc func(job Job)

// Job defines a scheduled snapshot job.
type Job struct {
	InstanceID     int
	ContainerName  string
	Engine         string
	DatabaseName   string
	Username       string
	Password       string
	Cron           string // 5-field cron expression
	RetentionCount int
	BackupDest     map[string]any // type + config for the backup destination
}

// Scheduler manages cron-scheduled snapshot jobs.
type Scheduler struct {
	mu       sync.Mutex
	jobs     map[int]*Job // keyed by database instance ID
	onFire   SnapshotFunc
	inflight sync.Map
}

// New creates a scheduler with the given callback for when jobs fire.
func New(onFire SnapshotFunc) *Scheduler {
	return &Scheduler{
		jobs:   make(map[int]*Job),
		onFire: onFire,
	}
}

// UpdateSchedule adds or updates a scheduled job.
func (s *Scheduler) UpdateSchedule(job Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.InstanceID] = &job
	log.Printf("scheduler: updated schedule for instance %d: %s", job.InstanceID, job.Cron)
}

// RemoveSchedule removes a scheduled job.
func (s *Scheduler) RemoveSchedule(instanceID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.jobs, instanceID)
	log.Printf("scheduler: removed schedule for instance %d", instanceID)
}

// Run starts the scheduler loop. It re-aligns to the next minute boundary on
// each iteration to prevent drift. Blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		now := time.Now()
		nextMin := now.Truncate(time.Minute).Add(time.Minute)
		timer := time.NewTimer(time.Until(nextMin))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.checkAndFire()
		}
	}
}

func (s *Scheduler) checkAndFire() {
	now := time.Now()
	s.mu.Lock()
	// Copy jobs to avoid holding lock during callback
	var toFire []Job
	for _, job := range s.jobs {
		if cronMatches(job.Cron, now) {
			toFire = append(toFire, *job)
		}
	}
	s.mu.Unlock()

	for _, job := range toFire {
		j := job
		if _, loaded := s.inflight.LoadOrStore(j.InstanceID, true); loaded {
			log.Printf("scheduler: skipping instance %d, previous snapshot still running", j.InstanceID)
			continue
		}
		go func() {
			defer s.inflight.Delete(j.InstanceID)
			s.onFire(j)
		}()
	}
}

// cronMatches checks if a 5-field cron expression matches the given time.
// Fields: minute hour day-of-month month day-of-week
// Supports: * (any), specific numbers, comma-separated lists, ranges (1-5), steps (*/5, 1-30/5).
func cronMatches(expr string, t time.Time) bool {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		log.Printf("scheduler: invalid cron expression (expected 5 fields): %q", expr)
		return false
	}

	minute := t.Minute()          // 0-59
	hour := t.Hour()              // 0-23
	dayOfMonth := t.Day()         // 1-31
	month := int(t.Month())      // 1-12
	dayOfWeek := int(t.Weekday()) // 0-6, 0=Sunday

	checks := []struct {
		field string
		value int
		min   int
		max   int
	}{
		{fields[0], minute, 0, 59},
		{fields[1], hour, 0, 23},
		{fields[2], dayOfMonth, 1, 31},
		{fields[3], month, 1, 12},
		{fields[4], dayOfWeek, 0, 6},
	}

	for _, c := range checks {
		matched, err := fieldMatches(c.field, c.value, c.min, c.max)
		if err != nil {
			log.Printf("scheduler: error parsing cron field %q: %v", c.field, err)
			return false
		}
		if !matched {
			return false
		}
	}
	return true
}

// fieldMatches checks whether a single cron field matches the given value.
// A field can contain comma-separated terms. Each term is one of:
//   - "*"       — matches any value
//   - "*/N"     — matches every N (step from min)
//   - "N"       — exact match
//   - "N-M"     — range (inclusive)
//   - "N-M/S"   — range with step
func fieldMatches(field string, value, min, max int) (bool, error) {
	// Split on commas for lists like "1,5,10"
	parts := strings.Split(field, ",")
	for _, part := range parts {
		matched, err := termMatches(strings.TrimSpace(part), value, min, max)
		if err != nil {
			return false, err
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func termMatches(term string, value, min, max int) (bool, error) {
	// Check for step: split on "/"
	var step int
	rangeExpr := term
	if idx := strings.Index(term, "/"); idx != -1 {
		stepStr := term[idx+1:]
		s, err := strconv.Atoi(stepStr)
		if err != nil || s <= 0 {
			return false, fmt.Errorf("invalid step %q", stepStr)
		}
		step = s
		rangeExpr = term[:idx]
	}

	// Determine the set of values from the range expression
	var rangeMin, rangeMax int
	if rangeExpr == "*" {
		rangeMin = min
		rangeMax = max
	} else if idx := strings.Index(rangeExpr, "-"); idx != -1 {
		lo, err := strconv.Atoi(rangeExpr[:idx])
		if err != nil {
			return false, fmt.Errorf("invalid range start %q", rangeExpr[:idx])
		}
		hi, err := strconv.Atoi(rangeExpr[idx+1:])
		if err != nil {
			return false, fmt.Errorf("invalid range end %q", rangeExpr[idx+1:])
		}
		if lo > hi {
			return false, fmt.Errorf("invalid range: %d > %d", lo, hi)
		}
		rangeMin = lo
		rangeMax = hi
	} else {
		// Exact number
		n, err := strconv.Atoi(rangeExpr)
		if err != nil {
			return false, fmt.Errorf("invalid value %q", rangeExpr)
		}
		if step > 0 {
			// e.g., "5/10" means starting at 5, every 10
			rangeMin = n
			rangeMax = max
		} else {
			return value == n, nil
		}
	}

	// If no step, any value in [rangeMin, rangeMax] matches
	if step == 0 {
		return value >= rangeMin && value <= rangeMax, nil
	}

	// With step: check if value is in the range and aligns with the step
	if value < rangeMin || value > rangeMax {
		return false, nil
	}
	return (value-rangeMin)%step == 0, nil
}
