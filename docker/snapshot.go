package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// ExecDatabaseDump executes a database dump command inside the container and returns
// the dump output as an io.Reader. The caller is responsible for consuming the reader.
func (c *Client) ExecDatabaseDump(ctx context.Context, containerID, engine, dbName, user, password string) (io.Reader, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	var cmd []string
	var envOverride []string
	switch engine {
	case "mysql", "mariadb":
		cmd = []string{"mysqldump", "-u", user, "--single-transaction", "--routines", "--triggers", dbName}
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

	// Read all stdout into a buffer.
	// Docker multiplexed streams have an 8-byte header per frame.
	// Use StdCopy to demux stdout from stderr.
	var stdout, stderr bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, resp.Reader)
	resp.Close()
	if err != nil {
		return nil, fmt.Errorf("read dump output: %w", err)
	}

	// Check exit code
	inspectResp, err := c.cli.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return nil, fmt.Errorf("exec inspect: %w", err)
	}
	if inspectResp.ExitCode != 0 {
		return nil, fmt.Errorf("dump exited with code %d: %s", inspectResp.ExitCode, stderr.String())
	}

	return &stdout, nil
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
		cmd = []string{"mysql", "-u", user, dbName}
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

	// Write data to stdin
	go func() {
		defer resp.CloseWrite()
		io.Copy(resp.Conn, data)
	}()

	// Read and discard stdout/stderr
	var stderr bytes.Buffer
	stdcopy.StdCopy(io.Discard, &stderr, resp.Reader)
	resp.Close()

	// Check exit code
	inspectResp, err := c.cli.ContainerExecInspect(ctx, execID.ID)
	if err != nil {
		return fmt.Errorf("exec inspect: %w", err)
	}
	if inspectResp.ExitCode != 0 {
		return fmt.Errorf("restore exited with code %d: %s", inspectResp.ExitCode, stderr.String())
	}

	return nil
}
