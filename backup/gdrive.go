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

	// Google Drive permits multiple files with the same name in a folder. If a
	// prior upload (or a retried snapshot) already created a file with this name,
	// a plain Create would leave duplicates behind — and Download/Delete resolve
	// a name to the *first* match, so retention could then act on the wrong copy.
	// Keep the name a stable, single object: update the existing file in place if
	// there is one, and trash any extra same-named duplicates.
	existing, err := d.findByName(ctx, remotePath)
	if err != nil {
		return 0, fmt.Errorf("gdrive lookup: %w", err)
	}

	if len(existing) == 0 {
		driveFile := &drive.File{Name: remotePath}
		if d.folderID != "" {
			driveFile.Parents = []string{d.folderID}
		}
		if _, err := d.service.Files.Create(driveFile).Media(file).Context(ctx).Do(); err != nil {
			return 0, fmt.Errorf("gdrive upload: %w", err)
		}
		return stat.Size(), nil
	}

	// Update the first existing file's content (do NOT set Parents on update —
	// changing parents requires addParents/removeParents and would error here).
	if _, err := d.service.Files.Update(existing[0], &drive.File{}).Media(file).Context(ctx).Do(); err != nil {
		return 0, fmt.Errorf("gdrive upload (update): %w", err)
	}
	// Trash any leftover duplicates so the name resolves to exactly one object.
	for _, dupID := range existing[1:] {
		if err := d.service.Files.Delete(dupID).Context(ctx).Do(); err != nil {
			// Best-effort: a failed dedupe doesn't invalidate the upload itself.
			continue
		}
	}

	return stat.Size(), nil
}

// findByName returns the ids of all non-trashed files matching name in the
// destination folder (or across the drive when no folder is configured).
func (d *gdriveDestination) findByName(ctx context.Context, name string) ([]string, error) {
	escapedName := strings.ReplaceAll(name, "\\", "\\\\")
	escapedName = strings.ReplaceAll(escapedName, "'", "\\'")
	query := fmt.Sprintf("name = '%s' and trashed = false", escapedName)
	if d.folderID != "" {
		query += fmt.Sprintf(" and '%s' in parents", d.folderID)
	}

	var ids []string
	pageToken := ""
	for {
		call := d.service.Files.List().Q(query).PageSize(100).Fields("nextPageToken, files(id)").Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		fileList, err := call.Do()
		if err != nil {
			return nil, err
		}
		for _, f := range fileList.Files {
			ids = append(ids, f.Id)
		}
		if fileList.NextPageToken == "" {
			break
		}
		pageToken = fileList.NextPageToken
	}
	return ids, nil
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
