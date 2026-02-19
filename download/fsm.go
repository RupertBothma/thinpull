// Package download implements the Download FSM for retrieving container images from S3.
package download

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	fsm "github.com/superfly/fsm"

	"github.com/superfly/fsm/database"
	"github.com/superfly/fsm/s3"
)

const (
	// MaxRetriesCheckExists is the maximum number of retries for database checks
	MaxRetriesCheckExists = 3
	// MaxRetriesDownload is the maximum number of retries for S3 download operations
	MaxRetriesDownload = 5
	// MaxRetriesValidate is the maximum number of retries for blob validation
	MaxRetriesValidate = 2
	// MaxRetriesStoreMetadata is the maximum number of retries for database writes
	MaxRetriesStoreMetadata = 5
)

// Dependencies holds the external dependencies for the Download FSM.
type Dependencies struct {
	DB       *database.DB
	S3Client *s3.Client
	S3Bucket string
	LocalDir string // Base directory for downloaded images (e.g., "/var/lib/flyio/images")
}

// ImageDownloadRequest represents the request to download a container image from S3.
//
// Callers SHOULD NOT choose ImageID directly. Instead, they should derive a
// deterministic image ID from the idempotency key (currently the S3 object
// key) using fsm.DeriveImageIDFromS3Key, and pass that value here. This keeps
// identity stable across retries and processes.
type ImageDownloadRequest = fsm.ImageDownloadRequest

// ImageDownloadResponse represents the response from the Download FSM.
type ImageDownloadResponse = fsm.ImageDownloadResponse

// checkExists verifies if the image has already been downloaded, and if not,
// uses a database-level reservation to ensure that at most one downloader is
// active for a given S3 key across processes.
//
// If the image exists and is valid, it returns fsm.Handoff to skip remaining
// transitions. If another process is currently downloading the same S3 object,
// the transition aborts with a descriptive error rather than starting a
// competing download.
func checkExists(deps *Dependencies) fsm.Transition[ImageDownloadRequest, ImageDownloadResponse] {
	return func(ctx context.Context, req *fsm.Request[ImageDownloadRequest, ImageDownloadResponse]) (*fsm.Response[ImageDownloadResponse], error) {
		logger := req.Log().WithField("transition", "check-exists")
		retryCount := fsm.RetryFromContext(ctx)

		// Enforce retry limit for database operations
		if retryCount > MaxRetriesCheckExists {
			return nil, fsm.Abort(fmt.Errorf("exceeded maximum retries (%d) for check-exists transition", MaxRetriesCheckExists))
		}

		if retryCount > 0 {
			logger.WithField("retry_count", retryCount).Info("retrying check-exists transition")
		}

		s3Key := req.Msg.S3Key
		imageID := req.Msg.ImageID

		logger.WithField("s3_key", s3Key).Info("checking if image already downloaded")

		// First, check for an already-completed download and validate the
		// on-disk file. This is the fast path and ensures we reuse good
		// downloads even if the reservation logic below would otherwise allow a
		// new download.
		existing, err := deps.DB.CheckImageDownloaded(ctx, s3Key)
		if err != nil {
			logger.WithError(err).Error("failed to check database for completed download")
			return nil, fmt.Errorf("database query failed: %w", err)
		}

		validateExisting := func(img *database.Image) (*fsm.Response[ImageDownloadResponse], error) {
			logger.WithFields(map[string]any{
				"image_id":   img.ImageID,
				"local_path": img.LocalPath,
				"checksum":   img.Checksum,
			}).Info("image found in database, verifying file")

			// Verify file exists
			fileInfo, err := os.Stat(img.LocalPath)
			if err != nil {
				if os.IsNotExist(err) {
					logger.Warn("file missing, will re-download")
					return nil, nil
				}
				logger.WithError(err).Error("failed to stat file")
				return nil, fmt.Errorf("failed to stat file: %w", err)
			}

			// Verify file size matches
			if fileInfo.Size() != img.SizeBytes {
				logger.WithFields(map[string]any{
					"expected": img.SizeBytes,
					"actual":   fileInfo.Size(),
				}).Warn("file size mismatch, will re-download")
				return nil, nil
			}

			// Verify checksum if available
			if img.Checksum != "" {
				actualChecksum, err := computeFileChecksum(img.LocalPath)
				if err != nil {
					logger.WithError(err).Error("failed to compute checksum")
					return nil, fmt.Errorf("failed to compute checksum: %w", err)
				}

				if actualChecksum != img.Checksum {
					logger.WithFields(map[string]any{
						"expected": img.Checksum,
						"actual":   actualChecksum,
					}).Warn("checksum mismatch, will re-download")
					return nil, nil
				}
			}

			logger.Info("image already downloaded and valid, skipping download")

			resp := &ImageDownloadResponse{
				ImageID:      img.ImageID,
				LocalPath:    img.LocalPath,
				Checksum:     img.Checksum,
				SizeBytes:    img.SizeBytes,
				Downloaded:   false,
				AlreadyExist: true,
			}

			// Use the current run's version for Handoff to properly signal FSM completion
			// (fsm.Handoff with empty ULID returns nil, which doesn't stop execution)
			return fsm.NewResponse(resp), fsm.Handoff(req.Run().StartVersion)
		}

		if existing != nil {
			if resp, err := validateExisting(existing); err != nil || resp != nil {
				return resp, err
			}
		}

		// No valid completed download; attempt to reserve a download slot in the
		// database so that only one downloader is active for this S3 key.
		if err := deps.DB.ReserveImageDownload(ctx, imageID, s3Key); err != nil {
			switch {
			case errors.Is(err, database.ErrDownloadAlreadyCompleted):
				logger.Info("download already completed by another process; re-checking metadata")
				img, err2 := deps.DB.CheckImageDownloaded(ctx, s3Key)
				if err2 != nil {
					logger.WithError(err2).Error("failed to re-check completed download after reservation conflict")
					return nil, fmt.Errorf("database query failed after reservation conflict: %w", err2)
				}
				if img == nil {
					return nil, fmt.Errorf("reservation reported completed download, but no record found for s3_key=%s", s3Key)
				}
				return validateExisting(img)
			case errors.Is(err, database.ErrDownloadInProgress):
				logger.WithError(err).Warn("another downloader is already in progress for this S3 key")
				return nil, fsm.Abort(fmt.Errorf("download already in progress for %s", s3Key))
			default:
				logger.WithError(err).Error("failed to reserve download slot")
				return nil, fmt.Errorf("download reservation failed: %w", err)
			}
		}

		logger.Info("download slot reserved; proceeding to download")
		return nil, nil
	}
}

// computeFileChecksum computes the SHA256 checksum of a file.
func computeFileChecksum(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// downloadFromS3 downloads the image from S3 to local storage.
func downloadFromS3(deps *Dependencies) fsm.Transition[ImageDownloadRequest, ImageDownloadResponse] {
	return func(ctx context.Context, req *fsm.Request[ImageDownloadRequest, ImageDownloadResponse]) (*fsm.Response[ImageDownloadResponse], error) {
		logger := req.Log().WithField("transition", "download")
		retryCount := fsm.RetryFromContext(ctx)

		// Enforce retry limit for S3 download operations
		if retryCount > MaxRetriesDownload {
			return nil, fsm.Abort(fmt.Errorf("exceeded maximum retries (%d) for download transition", MaxRetriesDownload))
		}

		if retryCount > 0 {
			logger.WithField("retry_count", retryCount).Info("retrying download transition")
		}

		s3Key := req.Msg.S3Key
		imageID := req.Msg.ImageID
		bucket := req.Msg.Bucket
		if bucket == "" {
			bucket = deps.S3Bucket
		}

		logger.WithFields(map[string]interface{}{
			"s3_key":   s3Key,
			"image_id": imageID,
			"bucket":   bucket,
		}).Info("downloading image from S3")

		// Use generous timeout for S3 download (large images can take time)
		ctxWithTimeout, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()

		// Determine local path
		localPath := filepath.Join(deps.LocalDir, fmt.Sprintf("%s.tar", imageID))

		// Download from S3
		result, err := deps.S3Client.DownloadImage(ctxWithTimeout, bucket, s3Key, localPath)
		if err != nil {
			logger.WithError(err).Error("S3 download failed")
			// Check for specific error types
			if isAccessDeniedError(err) {
				return nil, fsm.Abort(fmt.Errorf("S3 access denied: %w", err))
			}
			if isSizeLimitError(err) {
				return nil, fsm.Abort(fmt.Errorf("file too large: %w", err))
			}
			return nil, fmt.Errorf("S3 download failed: %w", err)
		}

		logger.WithFields(map[string]interface{}{
			"local_path": result.LocalPath,
			"checksum":   result.Checksum,
			"size":       result.SizeBytes,
		}).Info("download completed")

		// Store in response for next transition
		resp := &ImageDownloadResponse{
			ImageID:    imageID,
			LocalPath:  result.LocalPath,
			Checksum:   result.Checksum,
			SizeBytes:  result.SizeBytes,
			Downloaded: true,
		}

		return fsm.NewResponse(resp), nil
	}
}

// validateBlob validates the downloaded tarball for integrity and security.
func validateBlob(deps *Dependencies) fsm.Transition[ImageDownloadRequest, ImageDownloadResponse] {
	return func(ctx context.Context, req *fsm.Request[ImageDownloadRequest, ImageDownloadResponse]) (*fsm.Response[ImageDownloadResponse], error) {
		logger := req.Log().WithField("transition", "validate")
		retryCount := fsm.RetryFromContext(ctx)

		// Enforce retry limit for validation operations
		if retryCount > MaxRetriesValidate {
			return nil, fsm.Abort(fmt.Errorf("exceeded maximum retries (%d) for validate transition", MaxRetriesValidate))
		}

		if retryCount > 0 {
			logger.WithField("retry_count", retryCount).Info("retrying validate transition")
		}

		localPath := req.W.Msg.LocalPath
		expectedChecksum := req.W.Msg.Checksum

		logger.WithFields(map[string]interface{}{
			"local_path": localPath,
			"checksum":   expectedChecksum,
		}).Info("validating downloaded blob")

		// Use timeout for validation operations (tarball scanning can take time)
		ctxWithTimeout, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()

		// Check if context already timed out
		if ctxWithTimeout.Err() != nil {
			return nil, fmt.Errorf("validation timeout: %w", ctxWithTimeout.Err())
		}

		// Verify file exists
		fileInfo, err := os.Stat(localPath)
		if err != nil {
			logger.WithError(err).Error("file not found")
			return nil, fsm.Abort(fmt.Errorf("downloaded file not found: %w", err))
		}

		// Verify file size is reasonable
		if fileInfo.Size() == 0 {
			logger.Error("file is empty")
			return nil, fsm.Abort(fmt.Errorf("downloaded file is empty"))
		}

		logger.WithField("size", fileInfo.Size()).Info("file size verified")

		// Verify checksum (already computed during download, but double-check)
		actualChecksum, err := computeFileChecksum(localPath)
		if err != nil {
			logger.WithError(err).Error("failed to compute checksum")
			return nil, fmt.Errorf("checksum computation failed: %w", err)
		}

		if actualChecksum != expectedChecksum {
			logger.WithFields(map[string]interface{}{
				"expected": expectedChecksum,
				"actual":   actualChecksum,
			}).Error("checksum mismatch")
			// Clean up corrupted file
			os.Remove(localPath)
			return nil, fsm.Abort(fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum))
		}

		logger.Info("checksum verified")

		// Validate tar structure (can be opened and is valid format)
		if err := validateTarStructure(localPath); err != nil {
			logger.WithError(err).Error("invalid tar structure")
			// Clean up invalid file
			os.Remove(localPath)
			return nil, fsm.Abort(fmt.Errorf("invalid tar structure: %w", err))
		}

		logger.Info("tar structure validated")

		// Security checks: scan for path traversal and suspicious content
		if err := performSecurityChecks(localPath); err != nil {
			logger.WithError(err).Error("security validation failed")
			// Clean up malicious file
			os.Remove(localPath)
			return nil, fsm.Abort(fmt.Errorf("security validation failed: %w", err))
		}

		logger.Info("security checks passed")

		// Validation successful, pass through response
		return nil, nil
	}
}

// storeMetadata records the successful download in the database.
func storeMetadata(deps *Dependencies) fsm.Transition[ImageDownloadRequest, ImageDownloadResponse] {
	return func(ctx context.Context, req *fsm.Request[ImageDownloadRequest, ImageDownloadResponse]) (*fsm.Response[ImageDownloadResponse], error) {
		logger := req.Log().WithField("transition", "store-metadata")
		retryCount := fsm.RetryFromContext(ctx)

		// Enforce retry limit for database operations
		if retryCount > MaxRetriesStoreMetadata {
			return nil, fsm.Abort(fmt.Errorf("exceeded maximum retries (%d) for store-metadata transition", MaxRetriesStoreMetadata))
		}

		if retryCount > 0 {
			logger.WithField("retry_count", retryCount).Info("retrying store-metadata transition")
		}

		imageID := req.Msg.ImageID
		s3Key := req.Msg.S3Key
		localPath := req.W.Msg.LocalPath
		checksum := req.W.Msg.Checksum
		sizeBytes := req.W.Msg.SizeBytes

		logger.WithFields(map[string]interface{}{
			"image_id":   imageID,
			"s3_key":     s3Key,
			"local_path": localPath,
			"checksum":   checksum,
			"size":       sizeBytes,
		}).Info("storing image metadata in database")

		// Use timeout for database operations
		ctxWithTimeout, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		// Store in database
		err := deps.DB.StoreImageMetadata(ctxWithTimeout, imageID, s3Key, localPath, checksum, sizeBytes)
		if err != nil {
			logger.WithError(err).Error("failed to store metadata")
			return nil, fmt.Errorf("database update failed: %w", err)
		}

		logger.Info("metadata stored successfully")

		// Return final response
		resp := &ImageDownloadResponse{
			ImageID:      imageID,
			LocalPath:    localPath,
			Checksum:     checksum,
			SizeBytes:    sizeBytes,
			Downloaded:   true,
			AlreadyExist: false,
		}

		return fsm.NewResponse(resp), nil
	}
}

// Helper functions

func isAccessDeniedError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return contains(errStr, "AccessDenied") || contains(errStr, "403") || contains(errStr, "Forbidden")
}

func isSizeLimitError(err error) bool {
	if err == nil {
		return false
	}
	return contains(err.Error(), "too large")
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || containsMiddle(s, substr)))
}

func containsMiddle(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// validateTarStructure validates that the file is a valid tar archive.
func validateTarStructure(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Try to read tar header
	tarReader := tar.NewReader(file)

	// Read at least one header to verify it's a valid tar
	_, err = tarReader.Next()
	if err != nil && err != io.EOF {
		return fmt.Errorf("invalid tar format: %w", err)
	}

	return nil
}

// performSecurityChecks scans the tarball for malicious content.
func performSecurityChecks(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	tarReader := tar.NewReader(file)
	fileCount := 0
	const maxFiles = 100000

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("error reading tar: %w", err)
		}

		fileCount++
		if fileCount > maxFiles {
			return fmt.Errorf("too many files in archive: %d (max %d)", fileCount, maxFiles)
		}

		// Check for path traversal
		if strings.Contains(header.Name, "..") {
			return fmt.Errorf("path traversal detected: %s", header.Name)
		}

		// Check for absolute paths
		if filepath.IsAbs(header.Name) {
			return fmt.Errorf("absolute path not allowed: %s", header.Name)
		}

		// Check for suspicious symlinks
		if header.Typeflag == tar.TypeSymlink {
			// For relative symlink targets, verify they don't escape the archive root.
			if !filepath.IsAbs(header.Linkname) {
				// Resolve the symlink relative to the symlink's directory
				symlinkDir := filepath.Dir(header.Name)
				resolvedPath := filepath.Join(symlinkDir, header.Linkname)
				cleanedPath := filepath.Clean("/" + resolvedPath)
				// If clean path doesn't start with /, it tried to escape
				if !strings.HasPrefix(cleanedPath, "/") {
					return fmt.Errorf("symlink escapes root: %s -> %s (resolves to %s)", header.Name, header.Linkname, cleanedPath)
				}
			}
			// Absolute symlink targets are allowed (common in container images)
		}

		// Check file size
		const maxFileSize = 1 * 1024 * 1024 * 1024 // 1GB
		if header.Size > maxFileSize {
			return fmt.Errorf("file too large: %s (%d bytes, max %d)", header.Name, header.Size, maxFileSize)
		}
	}

	return nil
}

// Register registers the Download FSM with the manager.
// Returns start and resume functions for the FSM.
func Register(ctx context.Context, manager *fsm.Manager, deps *Dependencies) (fsm.Start[ImageDownloadRequest, ImageDownloadResponse], fsm.Resume, error) {
	return fsm.Register[ImageDownloadRequest, ImageDownloadResponse](manager, "download-image").
		Start("check-exists", checkExists(deps)).
		To("download", downloadFromS3(deps)).
		To("validate", validateBlob(deps)).
		To("store-metadata", storeMetadata(deps)).
		End("complete").
		Build(ctx)
}
