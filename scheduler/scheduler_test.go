package scheduler

import (
	"sync"
	"testing"
	"time"
)

func TestCronMatches(t *testing.T) {
	tests := []struct {
		name string
		expr string
		time time.Time
		want bool
	}{
		// Wildcard
		{"wildcard-matches-any", "* * * * *", time.Date(2025, 6, 15, 14, 35, 0, 0, time.UTC), true},

		// Specific minute
		{"minute-0-match", "0 * * * *", time.Date(2025, 6, 15, 14, 0, 0, 0, time.UTC), true},
		{"minute-0-no-match", "0 * * * *", time.Date(2025, 6, 15, 14, 30, 0, 0, time.UTC), false},

		// Step
		{"step-5-match-0", "*/5 * * * *", time.Date(2025, 6, 15, 14, 0, 0, 0, time.UTC), true},
		{"step-5-match-5", "*/5 * * * *", time.Date(2025, 6, 15, 14, 5, 0, 0, time.UTC), true},
		{"step-5-match-10", "*/5 * * * *", time.Date(2025, 6, 15, 14, 10, 0, 0, time.UTC), true},
		{"step-5-match-55", "*/5 * * * *", time.Date(2025, 6, 15, 14, 55, 0, 0, time.UTC), true},
		{"step-5-no-match-3", "*/5 * * * *", time.Date(2025, 6, 15, 14, 3, 0, 0, time.UTC), false},
		{"step-5-no-match-17", "*/5 * * * *", time.Date(2025, 6, 15, 14, 17, 0, 0, time.UTC), false},

		// Specific time
		{"2:30am-match", "30 2 * * *", time.Date(2025, 6, 15, 2, 30, 0, 0, time.UTC), true},
		{"2:30am-wrong-hour", "30 2 * * *", time.Date(2025, 6, 15, 3, 30, 0, 0, time.UTC), false},
		{"2:30am-wrong-minute", "30 2 * * *", time.Date(2025, 6, 15, 2, 0, 0, 0, time.UTC), false},

		// Day of month
		{"midnight-1st-match", "0 0 1 * *", time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), true},
		{"midnight-1st-wrong-day", "0 0 1 * *", time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC), false},

		// Day of week (0 = Sunday)
		{"sunday-midnight-match", "0 0 * * 0", time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC), true},   // 2025-06-15 is Sunday
		{"sunday-midnight-wrong-day", "0 0 * * 0", time.Date(2025, 6, 16, 0, 0, 0, 0, time.UTC), false}, // Monday

		// Comma-separated list
		{"list-match-1", "1,15,30 * * * *", time.Date(2025, 6, 15, 10, 1, 0, 0, time.UTC), true},
		{"list-match-15", "1,15,30 * * * *", time.Date(2025, 6, 15, 10, 15, 0, 0, time.UTC), true},
		{"list-match-30", "1,15,30 * * * *", time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC), true},
		{"list-no-match-7", "1,15,30 * * * *", time.Date(2025, 6, 15, 10, 7, 0, 0, time.UTC), false},

		// Range
		{"range-9-17-match-9", "0 9-17 * * *", time.Date(2025, 6, 15, 9, 0, 0, 0, time.UTC), true},
		{"range-9-17-match-12", "0 9-17 * * *", time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC), true},
		{"range-9-17-match-17", "0 9-17 * * *", time.Date(2025, 6, 15, 17, 0, 0, 0, time.UTC), true},
		{"range-9-17-no-match-8", "0 9-17 * * *", time.Date(2025, 6, 15, 8, 0, 0, 0, time.UTC), false},
		{"range-9-17-no-match-18", "0 9-17 * * *", time.Date(2025, 6, 15, 18, 0, 0, 0, time.UTC), false},

		// Weekday working hours
		{"weekday-9to5-match", "0 9-17 * * 1-5", time.Date(2025, 6, 16, 9, 0, 0, 0, time.UTC), true},   // Monday
		{"weekday-9to5-saturday", "0 9-17 * * 1-5", time.Date(2025, 6, 14, 9, 0, 0, 0, time.UTC), false}, // Saturday
		{"weekday-9to5-sunday", "0 9-17 * * 1-5", time.Date(2025, 6, 15, 9, 0, 0, 0, time.UTC), false},   // Sunday

		// Invalid expressions
		{"too-few-fields", "* * *", time.Date(2025, 6, 15, 14, 0, 0, 0, time.UTC), false},
		{"too-many-fields", "* * * * * *", time.Date(2025, 6, 15, 14, 0, 0, 0, time.UTC), false},
		{"bad-syntax", "abc * * * *", time.Date(2025, 6, 15, 14, 0, 0, 0, time.UTC), false},

		// Out-of-range value: minute 60 never matches
		{"minute-60-never-matches", "60 * * * *", time.Date(2025, 6, 15, 14, 0, 0, 0, time.UTC), false},
		{"minute-60-never-matches-59", "60 * * * *", time.Date(2025, 6, 15, 14, 59, 0, 0, time.UTC), false},

		// Reverse range returns error (treated as false)
		{"reverse-range", "10-5 * * * *", time.Date(2025, 6, 15, 14, 7, 0, 0, time.UTC), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cronMatches(tt.expr, tt.time)
			if got != tt.want {
				t.Errorf("cronMatches(%q, %v) = %v, want %v", tt.expr, tt.time, got, tt.want)
			}
		})
	}
}

func TestSchedulerUpdateRemove(t *testing.T) {
	s := New(func(job Job) {})

	t.Run("add-job", func(t *testing.T) {
		s.UpdateSchedule(Job{InstanceID: 1, ContainerName: "db1", Cron: "* * * * *"})
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.jobs) != 1 {
			t.Fatalf("expected 1 job, got %d", len(s.jobs))
		}
		if s.jobs[1].ContainerName != "db1" {
			t.Errorf("expected container name db1, got %s", s.jobs[1].ContainerName)
		}
	})

	t.Run("update-existing-job", func(t *testing.T) {
		s.UpdateSchedule(Job{InstanceID: 1, ContainerName: "db1-updated", Cron: "0 * * * *"})
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.jobs) != 1 {
			t.Fatalf("expected 1 job after update, got %d", len(s.jobs))
		}
		if s.jobs[1].ContainerName != "db1-updated" {
			t.Errorf("expected container name db1-updated, got %s", s.jobs[1].ContainerName)
		}
		if s.jobs[1].Cron != "0 * * * *" {
			t.Errorf("expected cron 0 * * * *, got %s", s.jobs[1].Cron)
		}
	})

	t.Run("add-second-job", func(t *testing.T) {
		s.UpdateSchedule(Job{InstanceID: 2, ContainerName: "db2", Cron: "30 2 * * *"})
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.jobs) != 2 {
			t.Fatalf("expected 2 jobs, got %d", len(s.jobs))
		}
	})

	t.Run("remove-first-job", func(t *testing.T) {
		s.RemoveSchedule(1)
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.jobs) != 1 {
			t.Fatalf("expected 1 job after removal, got %d", len(s.jobs))
		}
		if _, ok := s.jobs[1]; ok {
			t.Error("job with instance ID 1 should have been removed")
		}
		if _, ok := s.jobs[2]; !ok {
			t.Error("job with instance ID 2 should still exist")
		}
	})

	t.Run("remove-nonexistent-job", func(t *testing.T) {
		s.RemoveSchedule(999)
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.jobs) != 1 {
			t.Fatalf("expected 1 job after removing nonexistent, got %d", len(s.jobs))
		}
	})

	t.Run("remove-last-job", func(t *testing.T) {
		s.RemoveSchedule(2)
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.jobs) != 0 {
			t.Fatalf("expected 0 jobs after removing all, got %d", len(s.jobs))
		}
	})
}

func TestSchedulerFire(t *testing.T) {
	var mu sync.Mutex
	var fired []int

	s := New(func(job Job) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, job.InstanceID)
	})

	// Use a time that matches "* * * * *" (any) and "30 14 * * *" (14:30)
	// We'll set up jobs and directly call checkAndFire.

	t.Run("fires-matching-jobs", func(t *testing.T) {
		// June 15, 2025, 14:30 UTC
		s.UpdateSchedule(Job{InstanceID: 1, ContainerName: "db1", Cron: "* * * * *"})      // matches any time
		s.UpdateSchedule(Job{InstanceID: 2, ContainerName: "db2", Cron: "30 14 * * *"})    // matches 14:30
		s.UpdateSchedule(Job{InstanceID: 3, ContainerName: "db3", Cron: "0 0 1 1 *"})     // only Jan 1 midnight

		s.checkAndFire()

		// Wait briefly for goroutines to complete
		time.Sleep(100 * time.Millisecond)

		mu.Lock()
		defer mu.Unlock()

		// Job 1 and 2 should fire (wildcard and current time), job 3 should not
		// Since checkAndFire uses time.Now(), we use wildcard for reliable testing
		if len(fired) < 1 {
			t.Errorf("expected at least 1 job to fire, got %d", len(fired))
		}
		// Job 1 with wildcard cron should always fire
		found := false
		for _, id := range fired {
			if id == 1 {
				found = true
			}
		}
		if !found {
			t.Error("expected job 1 (wildcard cron) to fire")
		}
	})
}

func TestSchedulerInflight(t *testing.T) {
	blocker := make(chan struct{})

	var mu sync.Mutex
	fireCount := 0

	s := New(func(job Job) {
		mu.Lock()
		fireCount++
		mu.Unlock()
		<-blocker // Block until released
	})

	s.UpdateSchedule(Job{InstanceID: 1, ContainerName: "db1", Cron: "* * * * *"})

	// First fire - should start the job
	s.checkAndFire()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	firstFire := fireCount
	mu.Unlock()

	if firstFire != 1 {
		t.Fatalf("expected 1 fire on first call, got %d", firstFire)
	}

	// Second fire - should be skipped because job 1 is still inflight
	s.checkAndFire()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	secondFire := fireCount
	mu.Unlock()

	if secondFire != 1 {
		t.Errorf("expected fire count still 1 (inflight dedup), got %d", secondFire)
	}

	// Release the blocker so the first job completes
	close(blocker)
	time.Sleep(50 * time.Millisecond)

	// Third fire - should now succeed since the first job completed
	s.checkAndFire()
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	thirdFire := fireCount
	mu.Unlock()

	if thirdFire != 2 {
		t.Errorf("expected fire count 2 after inflight cleared, got %d", thirdFire)
	}
}
