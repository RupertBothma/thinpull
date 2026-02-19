// Package s3 provides S3 client operations for downloading container images.
//
// This package wraps the AWS SDK v2 to provide streaming downloads from S3 with
// built-in validation, checksum computation, and size limits.
//
// # Features
//
//   - Streaming downloads (no buffering entire file in memory)
//   - Automatic SHA256 checksum computation during download
//   - Size limit enforcement (10GB max)
//   - S3 key validation (path traversal prevention)
//   - Atomic file writes (temp file + rename)
//
// # Authentication
//
// The client uses AWS SDK default credential chain:
//  1. Environment variables (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY)
//  2. Shared credentials file (~/.aws/credentials)
//  3. IAM role (if running on EC2)
//
// # Usage Example
//
//	// Create client
//	client, err := s3.New(ctx, s3.Config{
//		Region: "us-east-1",
//		Bucket: "flyio-container-images",
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	client.SetLogger(logger)
//
//	// Download image
//	result, err := client.DownloadImage(ctx, "my-bucket", "images/alpine.tar", "/tmp/alpine.tar")
//	if err != nil {
//		log.Fatal(err)
//	}
//	fmt.Printf("Downloaded %d bytes, checksum: %s\n", result.SizeBytes, result.Checksum)
//
// # Security
//
// The package validates S3 keys to prevent path traversal attacks:
//   - Rejects keys containing ".."
//   - Rejects keys with absolute paths
//   - Enforces maximum key length (1024 chars)
//
// Downloads are size-limited to 10GB to prevent resource exhaustion.
package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/sirupsen/logrus"
)

// ProgressFunc is called periodically during download with progress updates
type ProgressFunc func(downloaded, total int64, speed float64)

// Client wraps the S3 client with helper methods for image downloads.
type Client struct {
	s3Client     *s3.Client
	logger       *logrus.Logger
	progressFunc ProgressFunc
}

// Config holds S3 client configuration.
type Config struct {
	// Region is the AWS region (optional, defaults to us-east-1)
	Region string

	// Bucket is the default S3 bucket name
	Bucket string
}

// DefaultConfig returns a default S3 configuration.
func DefaultConfig() Config {
	return Config{
		Region: "us-east-1",
		Bucket: "flyio-container-images",
	}
}

// New creates a new S3 client.
func New(ctx context.Context, cfg Config) (*Client, error) {
	// Load AWS configuration
	opts := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
	}

	// If no credentials provided in env, use anonymous
	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		opts = append(opts, config.WithCredentialsProvider(aws.AnonymousCredentials{}))
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	return &Client{
		s3Client: s3.NewFromConfig(awsCfg),
		logger:   logrus.New(),
	}, nil
}

// SetLogger sets a custom logger for the client.
func (c *Client) SetLogger(logger *logrus.Logger) {
	c.logger = logger
}

// SetProgressFunc sets a callback function for progress updates during downloads.
// The callback receives bytes downloaded, total bytes, and current speed in bytes/sec.
func (c *Client) SetProgressFunc(fn ProgressFunc) {
	c.progressFunc = fn
}

// SuppressLogs disables all log output from the S3 client.
// This is useful when running in TUI mode where logs would interfere with the display.
func (c *Client) SuppressLogs() {
	c.logger.SetOutput(io.Discard)
}

// DownloadResult contains the result of a download operation.
type DownloadResult struct {
	// LocalPath is the path to the downloaded file
	LocalPath string

	// Checksum is the SHA256 hash of the downloaded file
	Checksum string

	// SizeBytes is the size of the downloaded file in bytes
	SizeBytes int64
}

// DownloadImage downloads an image from S3 to a local file with streaming.
//
// The function downloads the S3 object in a streaming fashion (no full buffering),
// computes SHA256 checksum on-the-fly, and enforces size limits. The download is
// atomic: it writes to a temp file first, then renames on success.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - bucket: S3 bucket name
//   - key: S3 object key (e.g., "images/alpine-3.18.tar")
//   - destPath: Local filesystem path for downloaded file
//
// Returns:
//   - *DownloadResult: Contains local path, checksum, and size
//   - error: Any error during validation, download, or checksum computation
//
// Errors:
//   - Validation errors for invalid S3 keys
//   - AWS errors (NoSuchKey, AccessDenied, etc.)
//   - Size limit exceeded (>10GB)
//   - Filesystem errors
//
// The function validates the S3 key to prevent path traversal attacks and
// enforces a 10GB size limit to prevent resource exhaustion.
//
// Example:
//
//	result, err := client.DownloadImage(ctx,
//		"flyio-container-images",
//		"images/alpine-3.18.tar",
//		"/var/lib/flyio/images/alpine.tar",
//	)
//	if err != nil {
//		var nsk *s3types.NoSuchKey
//		if errors.As(err, &nsk) {
//			return fmt.Errorf("image not found in S3: %w", err)
//		}
//		return err
//	}
//	log.Printf("Downloaded: %s (%d bytes, checksum: %s)",
//		result.LocalPath, result.SizeBytes, result.Checksum)
//
// progressReader wraps an io.Reader and logs periodic download progress.
// It is single-threaded (used with io.Copy) and not concurrency-safe by design.
type progressReader struct {
	r            io.Reader
	logger       logrus.FieldLogger
	progressFunc ProgressFunc
	total        int64
	read         int64
	started      time.Time
	lastLog      time.Time
	interval     time.Duration
}

func newProgressReader(r io.Reader, logger logrus.FieldLogger, progressFunc ProgressFunc, total int64, interval time.Duration) *progressReader {
	return &progressReader{r: r, logger: logger, progressFunc: progressFunc, total: total, started: time.Now(), interval: interval}
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.read += int64(n)
		now := time.Now()
		if p.lastLog.IsZero() || now.Sub(p.lastLog) >= p.interval {
			p.log(now)
			p.lastLog = now
		}
	}
	return n, err
}

func (p *progressReader) log(now time.Time) {
	percent := float64(0)
	if p.total > 0 {
		percent = (float64(p.read) / float64(p.total)) * 100
	}
	elapsed := now.Sub(p.started).Seconds()
	var rate float64
	if elapsed > 0 {
		rate = float64(p.read) / elapsed
	}
	eta := "unknown"
	if p.total > 0 && rate > 0 {
		remaining := float64(p.total-p.read) / rate
		eta = time.Duration(remaining * float64(time.Second)).Truncate(time.Second).String()
	}
	p.logger.WithFields(logrus.Fields{
		"downloaded": humanBytes(p.read),
		"total":      humanBytes(p.total),
		"percent":    fmt.Sprintf("%.1f", percent),
		"avg_rate":   humanBytes(int64(rate)) + "/s",
		"eta":        eta,
	}).Info("s3 download progress")

	// Call progress callback if set
	if p.progressFunc != nil {
		p.progressFunc(p.read, p.total, rate)
	}
}

func humanBytes(b int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case b >= GB:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func (c *Client) DownloadImage(ctx context.Context, bucket, key, destPath string) (*DownloadResult, error) {
	// Validate S3 key
	if err := validateS3Key(key); err != nil {
		return nil, fmt.Errorf("invalid S3 key: %w", err)
	}

	logger := c.logger.WithFields(logrus.Fields{
		"bucket": bucket,
		"key":    key,
		"dest":   destPath,
	})

	logger.Info("starting S3 download")

	// Get object metadata first to check size
	headResp, err := c.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get object metadata: %w", err)
	}

	// Enforce size limit (10GB max)
	const maxSize = 10 * 1024 * 1024 * 1024 // 10GB
	if headResp.ContentLength != nil && *headResp.ContentLength > maxSize {
		return nil, fmt.Errorf("file too large: %d bytes (max %d)", *headResp.ContentLength, maxSize)
	}

	// Log expected content length
	var totalSize int64
	if headResp.ContentLength != nil {
		totalSize = *headResp.ContentLength
		logger.WithField("content_length", humanBytes(totalSize)).Info("s3 object metadata fetched")
	}

	// Create temporary file for download
	tmpPath := destPath + ".tmp"
	tmpFile, err := os.Create(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer func() {
		tmpFile.Close()
		// Clean up temp file if we didn't move it
		if _, err := os.Stat(tmpPath); err == nil {
			os.Remove(tmpPath)
		}
	}()

	// Download object with streaming
	getResp, err := c.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get object: %w", err)
	}
	defer getResp.Body.Close()

	// Stream to file while computing checksum, with progress logging
	hash := sha256.New()
	multiWriter := io.MultiWriter(tmpFile, hash)

	// Wrap body with progress reader (log every 5s)
	pr := newProgressReader(getResp.Body, logger, c.progressFunc, totalSize, 5*time.Second)

	written, err := io.Copy(multiWriter, pr)
	if err != nil {
		return nil, fmt.Errorf("failed to download file: %w", err)
	}

	// Final progress log at completion
	logger.WithFields(logrus.Fields{
		"downloaded": humanBytes(written),
		"total":      humanBytes(totalSize),
	}).Info("s3 download completed")

	// Final progress callback
	if c.progressFunc != nil {
		c.progressFunc(written, totalSize, 0)
	}

	// Sync to disk
	if err := tmpFile.Sync(); err != nil {
		return nil, fmt.Errorf("failed to sync file: %w", err)
	}

	// Close temp file before moving
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp file: %w", err)
	}

	// Ensure destination directory exists
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create destination directory: %w", err)
	}

	// Move temp file to final destination
	if err := os.Rename(tmpPath, destPath); err != nil {
		return nil, fmt.Errorf("failed to move file to destination: %w", err)
	}

	checksum := hex.EncodeToString(hash.Sum(nil))

	logger.WithFields(logrus.Fields{
		"size":     written,
		"checksum": checksum,
	}).Info("download completed")

	return &DownloadResult{
		LocalPath: destPath,
		Checksum:  checksum,
		SizeBytes: written,
	}, nil
}

// validateS3Key validates an S3 key for security.
func validateS3Key(key string) error {
	// Check for empty key
	if key == "" {
		return fmt.Errorf("S3 key cannot be empty")
	}

	// Check length (max 1024 characters)
	if len(key) > 1024 {
		return fmt.Errorf("S3 key too long: %d characters (max 1024)", len(key))
	}

	// Check for path traversal attempts
	if strings.Contains(key, "..") {
		return fmt.Errorf("S3 key contains path traversal: %s", key)
	}

	// Check for absolute paths (should be relative)
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("S3 key should not start with /: %s", key)
	}

	// Check for null bytes
	if strings.Contains(key, "\x00") {
		return fmt.Errorf("S3 key contains null byte")
	}

	return nil
}

// ListImages lists all images in the S3 bucket with a given prefix.
func (c *Client) ListImages(ctx context.Context, bucket, prefix string) ([]string, error) {
	logger := c.logger.WithFields(logrus.Fields{
		"bucket": bucket,
		"prefix": prefix,
	})

	logger.Info("listing S3 objects")

	var keys []string
	paginator := s3.NewListObjectsV2Paginator(c.s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}

		for _, obj := range page.Contents {
			if obj.Key != nil {
				keys = append(keys, *obj.Key)
			}
		}
	}

	logger.WithField("count", len(keys)).Info("listed S3 objects")

	return keys, nil
}

// ObjectExists checks if an object exists in S3.
func (c *Client) ObjectExists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := c.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})

	if err != nil {
		// Check if it's a "not found" error
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "404") {
			return false, nil
		}
		return false, fmt.Errorf("failed to check object existence: %w", err)
	}

	return true, nil
}

// GetObjectSize returns the size of an object in S3.
func (c *Client) GetObjectSize(ctx context.Context, bucket, key string) (int64, error) {
	resp, err := c.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return 0, fmt.Errorf("failed to get object size: %w", err)
	}

	if resp.ContentLength == nil {
		return 0, fmt.Errorf("object has no content length")
	}

	return *resp.ContentLength, nil
}

// S3Object represents an S3 object with metadata.
type S3Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// ListImagesDetailed lists all images in the S3 bucket with detailed metadata.
func (c *Client) ListImagesDetailed(ctx context.Context, bucket, prefix string) ([]S3Object, error) {
	logger := c.logger.WithFields(logrus.Fields{
		"bucket": bucket,
		"prefix": prefix,
	})

	logger.Info("listing S3 objects with metadata")

	var objects []S3Object
	paginator := s3.NewListObjectsV2Paginator(c.s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}

		for _, obj := range page.Contents {
			if obj.Key != nil {
				s3obj := S3Object{
					Key: *obj.Key,
				}
				if obj.Size != nil {
					s3obj.Size = *obj.Size
				}
				if obj.LastModified != nil {
					s3obj.LastModified = *obj.LastModified
				}
				objects = append(objects, s3obj)
			}
		}
	}

	logger.WithField("count", len(objects)).Info("listed S3 objects with metadata")

	return objects, nil
}
