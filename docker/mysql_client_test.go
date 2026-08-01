package docker

import (
	"strings"
	"testing"
)

// The first real snapshot this platform ever attempted failed with exit 127:
// `mysqldump` does not exist in a mariadb:11 image, because MariaDB removed the
// mysql*-named symlinks. These tests pin the fallback that fixed it.
func TestMysqlFamilyCmdPrefersMariaDBBinary(t *testing.T) {
	cmd := mysqlFamilyCmd("mariadb-dump", "mysqldump", "-u", "proof", "proof")

	if cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("expected a shell wrapper, got %v", cmd[:2])
	}
	script := cmd[2]
	if !strings.Contains(script, "command -v mariadb-dump") {
		t.Error("script does not probe for the MariaDB-named binary first")
	}
	if !strings.Contains(script, "exec mysqldump") {
		t.Error("script has no fallback to the MySQL-named binary")
	}
}

// Arguments must be positional. Interpolating a database name or password into
// the script would make either reinterpretable as shell syntax.
func TestMysqlFamilyCmdPassesArgumentsPositionally(t *testing.T) {
	cmd := mysqlFamilyCmd("mariadb", "mysql", "-u", "user; rm -rf /", "db")

	script := cmd[2]
	if strings.Contains(script, "rm -rf") {
		t.Fatal("an argument was interpolated into the shell script")
	}
	if !strings.Contains(script, `"$@"`) {
		t.Error(`script must forward "$@" so arguments stay positional`)
	}

	// argv[3] is the $0 placeholder; real arguments follow.
	if cmd[3] != "sh" {
		t.Errorf("expected a $0 placeholder before the arguments, got %q", cmd[3])
	}
	if cmd[len(cmd)-1] != "db" || cmd[len(cmd)-2] != "user; rm -rf /" {
		t.Errorf("arguments not forwarded verbatim: %v", cmd)
	}
}

func TestMysqlFamilyCmdFailsLoudlyWhenNeitherExists(t *testing.T) {
	// With no `|| true` anywhere, a missing binary leaves the shell's own
	// non-zero exit — which the dump path turns into a read error and an aborted
	// upload, rather than an empty object.
	script := mysqlFamilyCmd("mariadb-dump", "mysqldump")[2]
	if strings.Contains(script, "|| true") || strings.Contains(script, "2>/dev/null;") {
		t.Error("script suppresses a failure that must surface")
	}
}
