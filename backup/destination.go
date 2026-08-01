package backup

import (
	"context"
	"fmt"
	"io"
)

// StreamUploader is implemented by destinations that can write an object from a
// reader of unknown length.
//
// Only S3 implements it today. Google Drive's SDK and the smbclient CLI both want
// something file-shaped, so for those the caller stages to a temp file — which is
// a real constraint to know about, not an oversight: a 40GB dump to Samba needs
// 40GB of local disk, while the same dump to S3 needs none.
type StreamUploader interface {
	UploadStream(ctx context.Context, r io.Reader, remotePath string) (int64, error)
}

// Destination is the interface for backup storage providers.
type Destination interface {
	Upload(ctx context.Context, localPath, remotePath string) (int64, error)
	Download(ctx context.Context, remotePath, localPath string) error
	Test(ctx context.Context) error
	Delete(ctx context.Context, remotePath string) error
}

// NewDestination creates a backup destination from a type string and config map.
func NewDestination(destType string, config map[string]any) (Destination, error) {
	switch destType {
	case "s3":
		return newS3Destination(config)
	case "google_drive":
		return newGDriveDestination(config)
	case "samba":
		return newSambaDestination(config)
	default:
		return nil, fmt.Errorf("unsupported backup destination type: %s", destType)
	}
}

// getString extracts a string from a config map, returning empty string if not found.
func getString(config map[string]any, key string) string {
	if v, ok := config[key].(string); ok {
		return v
	}
	return ""
}
