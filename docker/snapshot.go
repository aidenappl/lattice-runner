package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/aidenappl/lattice-runner/telemetry"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// mysqlFamilyCmd builds an argv that prefers the MariaDB-named client and falls
// back to the MySQL-named one.
//
// MariaDB removed the mysql*-named symlinks: `mysqldump` does not exist in a
// mariadb:11 image, and exec'ing it exits **127**, which is how the first real
// snapshot on this platform failed. Choosing by engine version would work until
// the next rename; probing the container costs an extra round trip. Letting the
// container's own shell decide is both cheaper and version-proof, and it fails
// loudly if neither binary is present rather than silently producing nothing.
//
// The arguments are passed positionally so a password or database name can never
// be reinterpreted as shell syntax.
func mysqlFamilyCmd(preferred, fallback string, args ...string) []string {
	script := fmt.Sprintf(
		`if command -v %s >/dev/null 2>&1; then exec %s "$@"; else exec %s "$@"; fi`,
		preferred, preferred, fallback)
	return append([]string{"sh", "-c", script, "sh"}, args...)
}

// ExecDatabaseDump executes a database dump inside the container and returns the
// output as a stream.
//
// The dump used to be read into a bytes.Buffer in full before the caller wrote it
// anywhere, so a 4GB database meant a 4GB allocation inside the process that owns
// the Docker socket and the control-plane WebSocket for the entire worker — a
// backup could take down the agent that manages every container on the host.
//
// Two properties of the returned reader matter and are easy to lose:
//
//   - The exit code is checked *after* the stream drains, and a non-zero exit (or
//     a mid-stream failure) surfaces as a read error via CloseWithError rather
//     than as a short, clean EOF. That is what lets an uploader abort a multipart
//     upload instead of completing a truncated object. `mysqldump | gzip | s3 cp`
//     has exactly this bug — the compressor happily produces a valid archive of a
//     truncated dump and exits 0 (MySQL bugs #50272 and #90538), which is how
//     backups "succeed" for months and fail on the day you need them.
//   - io.Pipe has no internal buffering, so a stalled upload applies backpressure
//     directly to the dump's stdout while it holds its read snapshot. Callers
//     should put a buffered writer between compression and upload.
//
// The caller must Close the reader.
func (c *Client) ExecDatabaseDump(ctx context.Context, containerID, engine, dbName, user, password string) (io.ReadCloser, error) {
	var cmd []string
	var envOverride []string
	switch engine {
	case "mysql", "mariadb":
		// No --force: it turns errors into a fully-trailered dump that is missing
		// objects and still exits 0.
		cmd = mysqlFamilyCmd("mariadb-dump", "mysqldump",
			"-u", user, "--single-transaction", "--routines", "--triggers", dbName)
		envOverride = []string{"MYSQL_PWD=" + password}
	case "postgres":
		cmd = []string{"pg_dump", "-U", user, "-Fc", dbName}
		envOverride = []string{"PGPASSWORD=" + password}
	default:
		return nil, fmt.Errorf("unsupported engine for dump: %s", engine)
	}

	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		Env:          envOverride,
	}

	execID, err := c.cli.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}

	resp, err := c.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}

	pr, pw := io.Pipe()

	go func() {
		defer resp.Close()
		defer func() {
			// A panic must still end the stream with an error, or the uploader
			// reading pr waits forever with the database's dump open.
			if rec := recover(); rec != nil {
				telemetry.ReportPanic("snapshot.dump", rec, nil)
				pw.CloseWithError(fmt.Errorf("dump stream panicked: %v", rec))
			}
		}()

		// stderr is captured rather than streamed: it is the diagnosis attached to
		// a failure, and it is bounded by the engine's own error output.
		var stderr bytes.Buffer
		_, copyErr := stdcopy.StdCopy(pw, &stderr, resp.Reader)

		if copyErr != nil {
			pw.CloseWithError(fmt.Errorf("read dump output: %w", copyErr))
			return
		}

		// Inspect with a fresh context: the caller's may already be cancelled by a
		// failed upload, and the exit code is the only thing that distinguishes a
		// complete dump from a truncated one.
		inspectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		inspectResp, inspectErr := c.cli.ContainerExecInspect(inspectCtx, execID.ID)
		switch {
		case inspectErr != nil:
			pw.CloseWithError(fmt.Errorf("exec inspect: %w", inspectErr))
		case inspectResp.ExitCode != 0:
			pw.CloseWithError(fmt.Errorf("dump exited with code %d: %s",
				inspectResp.ExitCode, strings.TrimSpace(stderr.String())))
		default:
			pw.Close()
		}
	}()

	return pr, nil
}

// ExecDatabaseRestore executes a database restore command inside the container,
// piping the provided reader as stdin.
func (c *Client) ExecDatabaseRestore(ctx context.Context, containerID, engine, dbName, user, password string, data io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	var cmd []string
	var envOverride []string
	switch engine {
	case "mysql", "mariadb":
		cmd = mysqlFamilyCmd("mariadb", "mysql", "-u", user, dbName)
		envOverride = []string{"MYSQL_PWD=" + password}
	case "postgres":
		// Use pg_restore for custom-format dumps, falls back gracefully
		cmd = []string{"pg_restore", "-U", user, "-d", dbName, "--clean", "--if-exists"}
		envOverride = []string{"PGPASSWORD=" + password}
	default:
		return fmt.Errorf("unsupported engine for restore: %s", engine)
	}

	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Env:          envOverride,
	}

	execID, err := c.cli.ContainerExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return fmt.Errorf("exec create: %w", err)
	}

	resp, err := c.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("exec attach: %w", err)
	}

	// Write data to stdin. The copy result is kept: a restore whose input was
	// cut short can still exit 0 having applied only part of the dump.
	copyDone := make(chan restoreCopyResult, 1)
	go func() {
		var res restoreCopyResult
		defer func() { copyDone <- res }()
		defer telemetry.Recover("snapshot.restore_stdin", nil)
		defer resp.CloseWrite()
		res.n, res.err = io.Copy(resp.Conn, data)
	}()

	// Read and discard stdout/stderr
	var stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(io.Discard, &stderr, resp.Reader); err != nil {
		slog.WarnContext(ctx, "restore output stream failed, stderr may be incomplete", "component", "snapshot",
			"container_id", containerID, "engine", engine, "error", err)
	}
	resp.Close()
	// Closing the connection fails any write still in flight, so the copy ends
	// promptly unless it is stuck reading the source; don't wait on that forever.
	var copyRes restoreCopyResult
	select {
	case copyRes = <-copyDone:
	case <-time.After(restoreCopyWait):
		copyRes.err = fmt.Errorf("input copy still running %v after the restore exited", restoreCopyWait)
	}

	// Check exit code
	inspectResp, err := c.cli.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return fmt.Errorf("exec inspect: %w", err)
	}
	if inspectResp.ExitCode != 0 {
		if copyRes.err != nil {
			slog.WarnContext(ctx, "restore input stream failed", "component", "snapshot",
				"container_id", containerID, "engine", engine, "bytes_written", copyRes.n, "error", copyRes.err)
		}
		return fmt.Errorf("restore exited with code %d: %s", inspectResp.ExitCode, stderr.String())
	}
	if copyRes.err != nil {
		// The caller logs the returned error; this is the one place it is
		// known how much of the input reached the database.
		return fmt.Errorf("restore input truncated after %d bytes: %w", copyRes.n, copyRes.err)
	}

	return nil
}

// restoreCopyWait bounds how long a restore waits for its stdin copy to report
// once the restore process has exited.
const restoreCopyWait = 30 * time.Second

// restoreCopyResult is what the restore stdin copy managed to write.
type restoreCopyResult struct {
	n   int64
	err error
}
