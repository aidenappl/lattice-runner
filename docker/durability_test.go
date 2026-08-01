package docker

import (
	"strings"
	"testing"
)

// An unrecognised server flag does not degrade — the engine refuses to start and
// the container crashloops. So version gating is the part of durabilityArgs that
// has to be right, not the flag list.
func TestDurabilityArgs(t *testing.T) {
	tests := []struct {
		name        string
		engine      string
		version     string
		wantAny     bool
		mustContain []string
		mustNotHave []string
	}{
		{
			name:        "mysql 8 gets binary logging and a bounded temp tablespace",
			engine:      "mysql",
			version:     "8",
			wantAny:     true,
			mustContain: []string{"--log-bin=binlog", "--server-id=1", "--binlog-format=ROW"},
			// Removed in 8.4: writing it makes the server refuse to start.
			mustNotHave: []string{"--expire-logs-days"},
		},
		{
			name:        "mysql 8.4 is still gated in, and still never gets expire-logs-days",
			engine:      "mysql",
			version:     "8.4",
			wantAny:     true,
			mustNotHave: []string{"--expire-logs-days"},
		},
		{
			name:    "mysql 5.7 gets nothing rather than a guess",
			engine:  "mysql",
			version: "5.7",
			wantAny: false,
		},
		{
			name:        "mariadb 11 gets binlog expiry in seconds",
			engine:      "mariadb",
			version:     "11",
			wantAny:     true,
			mustContain: []string{"--log-bin=binlog", "--binlog-expire-logs-seconds=604800"},
		},
		{
			name:        "mariadb 10 gets logging but not the seconds flag",
			engine:      "mariadb",
			version:     "10.11",
			wantAny:     true,
			mustContain: []string{"--log-bin=binlog"},
			mustNotHave: []string{"--binlog-expire-logs-seconds"},
		},
		{
			name:        "postgres 16 enables archive_mode now so it need not restart later",
			engine:      "postgres",
			version:     "16",
			wantAny:     true,
			mustContain: []string{"archive_mode=on", "wal_level=replica"},
		},
		{
			name:    "postgres 11 is below the gate",
			engine:  "postgres",
			version: "11",
			wantAny: false,
		},
		{
			name:    "an unreadable version assumes nothing",
			engine:  "mariadb",
			version: "latest",
			wantAny: false,
		},
		{
			name:    "an unknown engine assumes nothing",
			engine:  "cockroach",
			version: "23",
			wantAny: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := durabilityArgs(tt.engine, tt.version, 0)
			joined := strings.Join(args, " ")

			if tt.wantAny && len(args) == 0 {
				t.Fatalf("expected flags for %s:%s, got none", tt.engine, tt.version)
			}
			if !tt.wantAny && len(args) != 0 {
				t.Fatalf("expected no flags for %s:%s, got %v", tt.engine, tt.version, args)
			}
			for _, want := range tt.mustContain {
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q in %v", want, args)
				}
			}
			for _, bad := range tt.mustNotHave {
				if strings.Contains(joined, bad) {
					t.Errorf("must not set %q — %v", bad, args)
				}
			}
		})
	}
}

func TestDurabilityArgsRespectsRetention(t *testing.T) {
	args := durabilityArgs("mysql", "8", 3600)
	if !strings.Contains(strings.Join(args, " "), "--binlog-expire-logs-seconds=3600") {
		t.Errorf("retention not honoured: %v", args)
	}
}

func TestEngineMajor(t *testing.T) {
	cases := map[string]int{"11": 11, "8.4": 8, "16.2": 16, "10.11": 10, "latest": 0, "": 0}
	for in, want := range cases {
		if got := engineMajor(in); got != want {
			t.Errorf("engineMajor(%q) = %d, want %d", in, got, want)
		}
	}
}
