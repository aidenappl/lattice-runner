package backup

import (
	"context"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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

	stat, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat file: %w", err)
	}

	key := d.remotePath(remotePath)
	_, err = d.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
		Body:   file,
	})
	if err != nil {
		return 0, fmt.Errorf("s3 upload: %w", err)
	}

	return stat.Size(), nil
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
