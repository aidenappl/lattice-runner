package backup

import (
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

type gdriveDestination struct {
	service  *drive.Service
	folderID string
}

func newGDriveDestination(config map[string]any) (*gdriveDestination, error) {
	clientID := getString(config, "client_id")
	clientSecret := getString(config, "client_secret")
	refreshToken := getString(config, "refresh_token")
	folderID := getString(config, "folder_id")

	if clientID == "" || clientSecret == "" || refreshToken == "" {
		return nil, fmt.Errorf("google_drive: client_id, client_secret, and refresh_token are required")
	}

	oauthConfig := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{drive.DriveFileScope},
	}

	token := &oauth2.Token{RefreshToken: refreshToken}
	tokenSource := oauthConfig.TokenSource(context.Background(), token)

	service, err := drive.NewService(context.Background(), option.WithTokenSource(tokenSource))
	if err != nil {
		return nil, fmt.Errorf("google_drive: create service: %w", err)
	}

	return &gdriveDestination{
		service:  service,
		folderID: folderID,
	}, nil
}

func (d *gdriveDestination) Upload(ctx context.Context, localPath, remotePath string) (int64, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return 0, fmt.Errorf("open file: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat file: %w", err)
	}

	driveFile := &drive.File{
		Name: remotePath,
	}
	if d.folderID != "" {
		driveFile.Parents = []string{d.folderID}
	}

	_, err = d.service.Files.Create(driveFile).Media(file).Context(ctx).Do()
	if err != nil {
		return 0, fmt.Errorf("gdrive upload: %w", err)
	}

	return stat.Size(), nil
}

func (d *gdriveDestination) Download(ctx context.Context, remotePath, localPath string) error {
	// Find file by name in the folder
	escapedName := strings.ReplaceAll(remotePath, "\\", "\\\\")
	escapedName = strings.ReplaceAll(escapedName, "'", "\\'")
	query := fmt.Sprintf("name = '%s' and trashed = false", escapedName)
	if d.folderID != "" {
		query += fmt.Sprintf(" and '%s' in parents", d.folderID)
	}

	fileList, err := d.service.Files.List().Q(query).PageSize(1).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("gdrive find file: %w", err)
	}
	if len(fileList.Files) == 0 {
		return fmt.Errorf("gdrive: file %q not found", remotePath)
	}

	httpResp, err := d.service.Files.Get(fileList.Files[0].Id).Context(ctx).Download()
	if err != nil {
		return fmt.Errorf("gdrive download: %w", err)
	}
	defer httpResp.Body.Close()

	outFile, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer outFile.Close()

	if _, err := outFile.ReadFrom(httpResp.Body); err != nil {
		os.Remove(localPath) // Clean up partial file
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}

func (d *gdriveDestination) Test(ctx context.Context) error {
	_, err := d.service.About.Get().Fields("user").Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("gdrive test: %w", err)
	}
	return nil
}

func (d *gdriveDestination) Delete(ctx context.Context, remotePath string) error {
	escapedName := strings.ReplaceAll(remotePath, "\\", "\\\\")
	escapedName = strings.ReplaceAll(escapedName, "'", "\\'")
	query := fmt.Sprintf("name = '%s' and trashed = false", escapedName)
	if d.folderID != "" {
		query += fmt.Sprintf(" and '%s' in parents", d.folderID)
	}

	fileList, err := d.service.Files.List().Q(query).PageSize(1).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("gdrive find file: %w", err)
	}
	if len(fileList.Files) == 0 {
		return nil // already deleted
	}

	if err := d.service.Files.Delete(fileList.Files[0].Id).Context(ctx).Do(); err != nil {
		return fmt.Errorf("gdrive delete: %w", err)
	}
	return nil
}
