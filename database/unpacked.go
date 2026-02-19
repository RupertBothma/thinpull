package database

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// CheckImageUnpacked checks if an image has already been unpacked.
// Returns the unpacked image if it exists and is verified, nil if not found.
func (d *DB) CheckImageUnpacked(ctx context.Context, imageID string) (*UnpackedImage, error) {
	query := `
		SELECT id, image_id, device_id, device_name, device_path, size_bytes,
		       file_count, layout_verified, created_at, unpacked_at, updated_at
		FROM unpacked_images
		WHERE image_id = ? AND layout_verified = 1
	`

	var img UnpackedImage
	err := d.db.QueryRowContext(ctx, query, imageID).Scan(
		&img.ID, &img.ImageID, &img.DeviceID, &img.DeviceName, &img.DevicePath,
		&img.SizeBytes, &img.FileCount, &img.LayoutVerified,
		&img.CreatedAt, &img.UnpackedAt, &img.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil // Not found
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query unpacked image: %w", err)
	}

	return &img, nil
}

// StoreUnpackedImage stores or updates unpacked image metadata.
func (d *DB) StoreUnpackedImage(ctx context.Context, imageID, deviceID, deviceName, devicePath string, sizeBytes int64, fileCount int) error {
	query := `
		INSERT INTO unpacked_images (image_id, device_id, device_name, device_path, size_bytes, file_count, layout_verified, unpacked_at)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(image_id) DO UPDATE SET
			device_id = excluded.device_id,
			device_name = excluded.device_name,
			device_path = excluded.device_path,
			size_bytes = excluded.size_bytes,
			file_count = excluded.file_count,
			layout_verified = 1,
			unpacked_at = excluded.unpacked_at,
			updated_at = CURRENT_TIMESTAMP
	`

	res, err := d.db.ExecContext(ctx, query, imageID, deviceID, deviceName, devicePath, sizeBytes, fileCount, time.Now())
	if err != nil {
		return fmt.Errorf("failed to store unpacked image: %w", err)
	}

	// Diagnostic logging to track DB writes
	rows, _ := res.RowsAffected()
	log.Printf("[DB-WRITE] StoreUnpackedImage: rows=%d, image_id=%s, device=%s, device_path=%s, db_file=%s",
		rows, imageID, deviceName, devicePath, d.path)

	return nil
}

// GetUnpackedImageByID retrieves an unpacked image by its image_id.
func (d *DB) GetUnpackedImageByID(ctx context.Context, imageID string) (*UnpackedImage, error) {
	query := `
		SELECT id, image_id, device_id, device_name, device_path, size_bytes,
		       file_count, layout_verified, created_at, unpacked_at, updated_at
		FROM unpacked_images
		WHERE image_id = ?
	`

	var img UnpackedImage
	err := d.db.QueryRowContext(ctx, query, imageID).Scan(
		&img.ID, &img.ImageID, &img.DeviceID, &img.DeviceName, &img.DevicePath,
		&img.SizeBytes, &img.FileCount, &img.LayoutVerified,
		&img.CreatedAt, &img.UnpackedAt, &img.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query unpacked image: %w", err)
	}

	return &img, nil
}

// GetUnpackedImageByDeviceID retrieves an unpacked image by its device_id.
func (d *DB) GetUnpackedImageByDeviceID(ctx context.Context, deviceID string) (*UnpackedImage, error) {
	query := `
		SELECT id, image_id, device_id, device_name, device_path, size_bytes,
		       file_count, layout_verified, created_at, unpacked_at, updated_at
		FROM unpacked_images
		WHERE device_id = ?
	`

	var img UnpackedImage
	err := d.db.QueryRowContext(ctx, query, deviceID).Scan(
		&img.ID, &img.ImageID, &img.DeviceID, &img.DeviceName, &img.DevicePath,
		&img.SizeBytes, &img.FileCount, &img.LayoutVerified,
		&img.CreatedAt, &img.UnpackedAt, &img.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query unpacked image: %w", err)
	}

	return &img, nil
}

// DeleteUnpackedImage deletes an unpacked image record.
// This should be used when cleaning up after a failed unpack operation.
func (d *DB) DeleteUnpackedImage(ctx context.Context, imageID string) error {
	query := `DELETE FROM unpacked_images WHERE image_id = ?`

	result, err := d.db.ExecContext(ctx, query, imageID)
	if err != nil {
		return fmt.Errorf("failed to delete unpacked image: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("unpacked image not found: %s", imageID)
	}

	return nil
}

// ListUnpackedImages lists all unpacked images.
func (d *DB) ListUnpackedImages(ctx context.Context) ([]*UnpackedImage, error) {
	query := `
		SELECT id, image_id, device_id, device_name, device_path, size_bytes,
		       file_count, layout_verified, created_at, unpacked_at, updated_at
		FROM unpacked_images
		ORDER BY unpacked_at DESC
	`

	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list unpacked images: %w", err)
	}
	defer rows.Close()

	var images []*UnpackedImage
	for rows.Next() {
		var img UnpackedImage
		err := rows.Scan(
			&img.ID, &img.ImageID, &img.DeviceID, &img.DeviceName, &img.DevicePath,
			&img.SizeBytes, &img.FileCount, &img.LayoutVerified,
			&img.CreatedAt, &img.UnpackedAt, &img.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan unpacked image: %w", err)
		}

		images = append(images, &img)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating unpacked images: %w", err)
	}

	return images, nil
}
