package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// smbEscape escapes double quotes in a string for use in smbclient -c commands.
func smbEscape(s string) string {
	return strings.ReplaceAll(s, `"`, `\"`)
}

type sambaDestination struct {
	server   string
	share    string
	username string
	password string
	path     string
}

func newSambaDestination(config map[string]any) (*sambaDestination, error) {
	server := getString(config, "server")
	share := getString(config, "share")
	username := getString(config, "username")
	password := getString(config, "password")
	path := getString(config, "path")

	if server == "" || share == "" || username == "" || password == "" {
		return nil, fmt.Errorf("samba: server, share, username, and password are required")
	}
	if strings.Contains(username, "%") {
		return nil, fmt.Errorf("samba: username must not contain '%%'")
	}

	return &sambaDestination{
		server:   server,
		share:    share,
		username: username,
		password: password,
		path:     path,
	}, nil
}

func (d *sambaDestination) smbURI() string {
	return fmt.Sprintf("//%s/%s", d.server, d.share)
}

func (d *sambaDestination) remoteDir() string {
	if d.path != "" {
		return strings.TrimSuffix(d.path, "/")
	}
	return ""
}

func (d *sambaDestination) Upload(ctx context.Context, localPath, remotePath string) (int64, error) {
	remote := remotePath
	if d.remoteDir() != "" {
		remote = d.remoteDir() + "/" + remotePath
	}

	// Ensure remote directory exists
	if d.remoteDir() != "" {
		mkdirCmd := fmt.Sprintf(`mkdir "%s"`, smbEscape(d.remoteDir()))
		cmd := exec.CommandContext(ctx, "smbclient", d.smbURI(), "-U", d.username, "-c", mkdirCmd)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + os.Getenv("HOME"),
			"PASSWD=" + d.password,
		}
		_ = cmd.Run() // ignore error if dir already exists
	}

	putCmd := fmt.Sprintf(`put "%s" "%s"`, smbEscape(localPath), smbEscape(remote))
	cmd := exec.CommandContext(ctx, "smbclient", d.smbURI(), "-U", d.username, "-c", putCmd)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"PASSWD=" + d.password,
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("samba upload: %s: %w", strings.TrimSpace(string(output)), err)
	}

	// Get file size from local file
	info, err := os.Stat(localPath)
	if err != nil {
		return 0, fmt.Errorf("stat uploaded file: %w", err)
	}
	return info.Size(), nil
}

func (d *sambaDestination) Download(ctx context.Context, remotePath, localPath string) error {
	remote := remotePath
	if d.remoteDir() != "" {
		remote = d.remoteDir() + "/" + remotePath
	}

	getCmd := fmt.Sprintf(`get "%s" "%s"`, smbEscape(remote), smbEscape(localPath))
	cmd := exec.CommandContext(ctx, "smbclient", d.smbURI(), "-U", d.username, "-c", getCmd)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"PASSWD=" + d.password,
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("samba download: %s: %w", strings.TrimSpace(string(output)), err)
	}

	return nil
}

func (d *sambaDestination) Test(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "smbclient", d.smbURI(), "-U", d.username, "-c", "ls")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"PASSWD=" + d.password,
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("samba test: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

func (d *sambaDestination) Delete(ctx context.Context, remotePath string) error {
	remote := remotePath
	if d.remoteDir() != "" {
		remote = d.remoteDir() + "/" + remotePath
	}

	delCmd := fmt.Sprintf(`del "%s"`, smbEscape(remote))
	cmd := exec.CommandContext(ctx, "smbclient", d.smbURI(), "-U", d.username, "-c", delCmd)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"PASSWD=" + d.password,
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("samba delete: %s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}
