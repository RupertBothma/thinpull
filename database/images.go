package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// Download reservation errors used by higher layers (e.g. Download FSM) to
// reason about concurrent access to the same S3 object.
var (
	// ErrDownloadAlreadyCompleted indicates that another process has already
	// successfully downloaded this S3 object and recorded it with
	// download_status = 'completed'. Callers should typically treat this as a
	// cache hit and reuse the existing metadata / file on disk.
	ErrDownloadAlreadyCompleted = errors.New("download already completed")

	// ErrDownloadInProgress indicates that another process is currently
	// downloading the same S3 object (download_status = 'downloading' and not
	// considered stale). Callers should not start a competing download. They can
	// either fail fast or wait and retry later.
	ErrDownloadInProgress = errors.New("download in progress")
)

// CheckImageDownloaded checks if an image has already been downloaded.
// Returns the image if it exists and is completed, nil if not found or incomplete.
func (d *DB) CheckImageDownloaded(ctx context.Context, s3Key string) (*Image, error) {
	query := `
		SELECT id, image_id, s3_key, local_path, checksum, size_bytes,
		       download_status, activation_status, created_at,
		       download_started_at, downloaded_at,
		       activated_at, updated_at
		FROM images
		WHERE s3_key = ? AND download_status = ?
	`

	var img Image
	var startedAt, downloadedAt, activatedAt sql.NullTime

	err := d.db.QueryRowContext(ctx, query, s3Key, DownloadStatusCompleted).Scan(
		&img.ID, &img.ImageID, &img.S3Key, &img.LocalPath, &img.Checksum,
		&img.SizeBytes, &img.DownloadStatus, &img.ActivationStatus,
		&img.CreatedAt, &startedAt, &downloadedAt, &activatedAt, &img.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil // Not found
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query image: %w", err)
	}

	if startedAt.Valid {
		img.DownloadStartedAt = &startedAt.Time
	}
	if downloadedAt.Valid {
		img.DownloadedAt = &downloadedAt.Time
	}
	if activatedAt.Valid {
		img.ActivatedAt = &activatedAt.Time
	}

	return &img, nil
}

// StoreImageMetadata stores or updates image metadata after successful download.
func (d *DB) StoreImageMetadata(ctx context.Context, imageID, s3Key, localPath, checksum string, sizeBytes int64) error {
	query := `
		INSERT INTO images (image_id, s3_key, local_path, checksum, size_bytes, download_status, downloaded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(s3_key) DO UPDATE SET
			local_path = excluded.local_path,
			checksum = excluded.checksum,
			size_bytes = excluded.size_bytes,
			download_status = excluded.download_status,
			downloaded_at = excluded.downloaded_at,
			updated_at = CURRENT_TIMESTAMP
	`

	res, err := d.db.ExecContext(ctx, query, imageID, s3Key, localPath, checksum, sizeBytes, DownloadStatusCompleted, time.Now())
	if err != nil {
		return fmt.Errorf("failed to store image metadata: %w", err)
	}

	// Diagnostic logging to track DB writes
	rows, _ := res.RowsAffected()
	log.Printf("[DB-WRITE] StoreImageMetadata: rows=%d, s3_key=%s, image_id=%s, path=%s, db_file=%s",
		rows, s3Key, imageID, localPath, d.path)

	return nil
}

// downloadStaleThreshold defines how long a "downloading" row can remain
// before it is considered stale and eligible to be taken over by a new
// downloader. This provides a safety valve for crash recovery in cases where
// the original downloader never completed.
const downloadStaleThreshold = 1 * time.Hour

// GetImageByS3Key retrieves an image row by its S3 key.
func (d *DB) GetImageByS3Key(ctx context.Context, s3Key string) (*Image, error) {
	query := `
		SELECT id, image_id, s3_key, local_path, checksum, size_bytes,
		       download_status, activation_status, created_at,
		       download_started_at, downloaded_at,
		       activated_at, updated_at
		FROM images
		WHERE s3_key = ?
	`

	var img Image
	var startedAt, downloadedAt, activatedAt sql.NullTime

	err := d.db.QueryRowContext(ctx, query, s3Key).Scan(
		&img.ID, &img.ImageID, &img.S3Key, &img.LocalPath, &img.Checksum,
		&img.SizeBytes, &img.DownloadStatus, &img.ActivationStatus,
		&img.CreatedAt, &startedAt, &downloadedAt, &activatedAt, &img.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query image by s3 key: %w", err)
	}

	if startedAt.Valid {
		img.DownloadStartedAt = &startedAt.Time
	}
	if downloadedAt.Valid {
		img.DownloadedAt = &downloadedAt.Time
	}
	if activatedAt.Valid {
		img.ActivatedAt = &activatedAt.Time
	}

	return &img, nil
}

// ReserveImageDownload attempts to atomically reserve the right to download
// the given S3 object. It enforces that at most one downloader is active for a
// given s3Key at any time across processes.
//
// Behaviour:
//   - If no row exists, or the existing row is in a terminal/non-active
//     state ("pending", "failed") or has a stale "downloading" status, this
//     will insert/update the row to "downloading" and return nil.
//   - If a row exists with "completed", returns ErrDownloadAlreadyCompleted.
//   - If a row exists with a non-stale "downloading", returns
//     ErrDownloadInProgress.
func (d *DB) ReserveImageDownload(ctx context.Context, imageID, s3Key string) error {
	now := time.Now()
	staleBefore := now.Add(-downloadStaleThreshold)

	query := `
		INSERT INTO images (image_id, s3_key, local_path, checksum, size_bytes, download_status, download_started_at)
		VALUES (?, ?, '', '', 0, ?, ?)
		ON CONFLICT(s3_key) DO UPDATE SET
			image_id = excluded.image_id,
			download_status = excluded.download_status,
			download_started_at = excluded.download_started_at,
			downloaded_at = NULL,
			updated_at = CURRENT_TIMESTAMP
		WHERE images.download_status IN ('pending','failed')
		   OR (images.download_status = 'downloading'
		       AND (images.download_started_at IS NULL OR images.download_started_at < ?));
	`

	res, err := d.db.ExecContext(ctx, query, imageID, s3Key, DownloadStatusDownloading, now, staleBefore)
	if err != nil {
		// Surface constraint errors clearly for debugging.
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "constraint") {
			return fmt.Errorf("reserve image download constraint error for s3_key %s: %w", s3Key, err)
		}
		return fmt.Errorf("reserve image download failed for s3_key %s: %w", s3Key, err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("reserve image download: failed to get rows affected: %w", err)
	}
	if rows > 0 {
		// We successfully claimed or refreshed the reservation.
		return nil
	}

	// No rows changed means the WHERE clause on the UPSERT prevented us from
	// taking over the record. Inspect the current state to decide why.
	img, err := d.GetImageByS3Key(ctx, s3Key)
	if err != nil {
		return err
	}
	if img == nil {
		return fmt.Errorf("reservation failed but image row missing for s3_key %s", s3Key)
	}

	switch img.DownloadStatus {
	case DownloadStatusCompleted:
		return ErrDownloadAlreadyCompleted
	case DownloadStatusDownloading:
		// Treat as in-progress if it is not stale according to our threshold.
		if img.DownloadStartedAt != nil && img.DownloadStartedAt.After(staleBefore) {
			return ErrDownloadInProgress
		}
		// If it is stale but we still failed to reserve, signal a generic error.
		return fmt.Errorf("stale downloading record for s3_key %s could not be taken over", s3Key)
	case DownloadStatusPending, DownloadStatusFailed:
		// These should have been eligible for takeover via the WHERE clause.
		return fmt.Errorf("unexpected reservation conflict for s3_key %s with status %s", s3Key, img.DownloadStatus)
	default:
		return fmt.Errorf("unknown download status %s for s3_key %s", img.DownloadStatus, s3Key)
	}
}

// GetImageByID retrieves an image by its image_id.
func (d *DB) GetImageByID(ctx context.Context, imageID string) (*Image, error) {
	query := `
		SELECT id, image_id, s3_key, local_path, checksum, size_bytes,
		       download_status, activation_status, created_at,
		       download_started_at, downloaded_at,
		       activated_at, updated_at
		FROM images
		WHERE image_id = ?
	`

	var img Image
	var startedAt, downloadedAt, activatedAt sql.NullTime

	err := d.db.QueryRowContext(ctx, query, imageID).Scan(
		&img.ID, &img.ImageID, &img.S3Key, &img.LocalPath, &img.Checksum,
		&img.SizeBytes, &img.DownloadStatus, &img.ActivationStatus,
		&img.CreatedAt, &startedAt, &downloadedAt, &activatedAt, &img.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query image: %w", err)
	}

	if downloadedAt.Valid {
		img.DownloadedAt = &downloadedAt.Time
	}
	if activatedAt.Valid {
		img.ActivatedAt = &activatedAt.Time
	}

	return &img, nil
}

// UpdateImageActivationStatus updates the activation status of an image.
func (d *DB) UpdateImageActivationStatus(ctx context.Context, imageID, status string) error {
	query := `
		UPDATE images
		SET activation_status = ?,
		    activated_at = CASE WHEN ? = 'active' THEN CURRENT_TIMESTAMP ELSE activated_at END,
		    updated_at = CURRENT_TIMESTAMP
		WHERE image_id = ?
	`

	result, err := d.db.ExecContext(ctx, query, status, status, imageID)
	if err != nil {
		return fmt.Errorf("failed to update activation status: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("image not found: %s", imageID)
	}

	// Diagnostic logging to track DB writes
	log.Printf("[DB-WRITE] UpdateImageActivationStatus: rows=%d, image_id=%s, status=%s, db_file=%s",
		rows, imageID, status, d.path)

	return nil
}

// ListImages lists all images with optional status filter.
func (d *DB) ListImages(ctx context.Context, downloadStatus string) ([]*Image, error) {
	query := `
		SELECT id, image_id, s3_key, local_path, checksum, size_bytes, 
		       download_status, activation_status, created_at, downloaded_at, 
		       activated_at, updated_at
		FROM images
	`

	args := []interface{}{}
	if downloadStatus != "" {
		query += " WHERE download_status = ?"
		args = append(args, downloadStatus)
	}

	query += " ORDER BY downloaded_at DESC"

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to list images: %w", err)
	}
	defer rows.Close()

	var images []*Image
	for rows.Next() {
		var img Image
		var downloadedAt, activatedAt sql.NullTime

		err := rows.Scan(
			&img.ID, &img.ImageID, &img.S3Key, &img.LocalPath, &img.Checksum,
			&img.SizeBytes, &img.DownloadStatus, &img.ActivationStatus,
			&img.CreatedAt, &downloadedAt, &activatedAt, &img.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan image: %w", err)
		}

		if downloadedAt.Valid {
			img.DownloadedAt = &downloadedAt.Time
		}
		if activatedAt.Valid {
			img.ActivatedAt = &activatedAt.Time
		}

		images = append(images, &img)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating images: %w", err)
	}

	return images, nil
}
