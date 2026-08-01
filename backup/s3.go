package backup

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type s3Destination struct {
	client     *s3.Client
	bucket     string
	pathPrefix string
}

func newS3Destination(config map[string]any) (*s3Destination, error) {
	bucket := getString(config, "bucket")
	region := getString(config, "region")
	accessKey := getString(config, "access_key_id")
	secretKey := getString(config, "secret_access_key")
	endpoint := getString(config, "endpoint")
	pathPrefix := getString(config, "path_prefix")

	if bucket == "" || region == "" || accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("s3: bucket, region, access_key_id, and secret_access_key are required")
	}

	opts := s3.Options{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
	}
	if endpoint != "" {
		opts.BaseEndpoint = aws.String(endpoint)
		opts.UsePathStyle = true // Required for MinIO and other S3-compatible
	}

	client := s3.New(opts)

	return &s3Destination{
		client:     client,
		bucket:     bucket,
		pathPrefix: pathPrefix,
	}, nil
}

func (d *s3Destination) remotePath(path string) string {
	if d.pathPrefix != "" {
		return d.pathPrefix + "/" + path
	}
	return path
}

func (d *s3Destination) Upload(ctx context.Context, localPath, remotePath string) (int64, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return 0, fmt.Errorf("open file: %w", err)
	}
	defer file.Close()

	return d.UploadStream(ctx, file, remotePath)
}

// UploadStream writes an object from a reader of unknown length, using multipart
// upload.
//
// The single PutObject this replaced imposed a hard 5GiB ceiling on any snapshot
// and required the whole dump on local disk first. The uploader also aborts the
// multipart upload when the reader returns an error, which is the half that
// matters: a dump that dies mid-stream must leave *no* object rather than a
// complete-looking truncated one.
//
// Full-object CRC32C is requested because S3 can only linearise CRC-family
// checksums across parts — SHA-256 is composite-only on multipart, so it cannot
// be server-verified for the whole object.
func (d *s3Destination) UploadStream(ctx context.Context, r io.Reader, remotePath string) (int64, error) {
	key := d.remotePath(remotePath)

	counter := &countingReader{r: r}
	uploader := manager.NewUploader(d.client, func(u *manager.Uploader) {
		u.PartSize = 16 * 1024 * 1024
		u.Concurrency = 3
		// Leave no orphan parts behind on failure. They are billed until a
		// lifecycle rule reaps them, and nothing in the UI would ever show them.
		u.LeavePartsOnError = false
	})

	_, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:            aws.String(d.bucket),
		Key:               aws.String(key),
		Body:              counter,
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32c,
	})
	if err != nil {
		return 0, fmt.Errorf("s3 upload: %w", err)
	}

	return counter.n, nil
}

// countingReader records how many bytes actually reached the destination, so a
// snapshot's recorded size is measured rather than assumed.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (d *s3Destination) Download(ctx context.Context, remotePath, localPath string) error {
	key := d.remotePath(remotePath)
	result, err := d.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 download: %w", err)
	}
	defer result.Body.Close()

	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer file.Close()

	if _, err := file.ReadFrom(result.Body); err != nil {
		os.Remove(localPath) // Clean up partial file
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}

func (d *s3Destination) Test(ctx context.Context) error {
	_, err := d.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(d.bucket),
	})
	if err != nil {
		return fmt.Errorf("s3 test: %w", err)
	}
	return nil
}

func (d *s3Destination) Delete(ctx context.Context, remotePath string) error {
	key := d.remotePath(remotePath)
	_, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3 delete: %w", err)
	}
	return nil
}
