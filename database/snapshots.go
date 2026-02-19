package database

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// CheckSnapshotExists checks if a snapshot already exists for an image.
// Returns the snapshot if it exists and is active, nil if not found.
func (d *DB) CheckSnapshotExists(ctx context.Context, imageID, snapshotName string) (*Snapshot, error) {
	query := `
		SELECT id, image_id, snapshot_id, snapshot_name, device_path, origin_device_id,
		       active, created_at, deactivated_at, updated_at
		FROM snapshots
		WHERE image_id = ? AND snapshot_name = ? AND active = 1
	`

	var snap Snapshot
	var deactivatedAt sql.NullTime

	err := d.db.QueryRowContext(ctx, query, imageID, snapshotName).Scan(
		&snap.ID, &snap.ImageID, &snap.SnapshotID, &snap.SnapshotName,
		&snap.DevicePath, &snap.OriginDeviceID, &snap.Active,
		&snap.CreatedAt, &deactivatedAt, &snap.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil // Not found
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query snapshot: %w", err)
	}

	if deactivatedAt.Valid {
		snap.DeactivatedAt = &deactivatedAt.Time
	}

	return &snap, nil
}

// StoreSnapshot stores or updates snapshot metadata.
func (d *DB) StoreSnapshot(ctx context.Context, imageID, snapshotID, snapshotName, devicePath, originDeviceID string) error {
	query := `
		INSERT INTO snapshots (image_id, snapshot_id, snapshot_name, device_path, origin_device_id, active, created_at)
		VALUES (?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(snapshot_name) DO UPDATE SET
			active = 1,
			updated_at = CURRENT_TIMESTAMP
	`

	res, err := d.db.ExecContext(ctx, query, imageID, snapshotID, snapshotName, devicePath, originDeviceID, time.Now())
	if err != nil {
		return fmt.Errorf("failed to store snapshot: %w", err)
	}

	// Diagnostic logging to track DB writes
	rows, _ := res.RowsAffected()
	log.Printf("[DB-WRITE] StoreSnapshot: rows=%d, snapshot=%s, image_id=%s, device_path=%s, db_file=%s",
		rows, snapshotName, imageID, devicePath, d.path)

	return nil
}

// GetSnapshotByID retrieves a snapshot by its snapshot_id.
func (d *DB) GetSnapshotByID(ctx context.Context, snapshotID string) (*Snapshot, error) {
	query := `
		SELECT id, image_id, snapshot_id, snapshot_name, device_path, origin_device_id,
		       active, created_at, deactivated_at, updated_at
		FROM snapshots
		WHERE snapshot_id = ?
	`

	var snap Snapshot
	var deactivatedAt sql.NullTime

	err := d.db.QueryRowContext(ctx, query, snapshotID).Scan(
		&snap.ID, &snap.ImageID, &snap.SnapshotID, &snap.SnapshotName,
		&snap.DevicePath, &snap.OriginDeviceID, &snap.Active,
		&snap.CreatedAt, &deactivatedAt, &snap.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query snapshot: %w", err)
	}

	if deactivatedAt.Valid {
		snap.DeactivatedAt = &deactivatedAt.Time
	}

	return &snap, nil
}

// GetSnapshotsByImageID retrieves all snapshots for an image.
func (d *DB) GetSnapshotsByImageID(ctx context.Context, imageID string) ([]*Snapshot, error) {
	query := `
		SELECT id, image_id, snapshot_id, snapshot_name, device_path, origin_device_id,
		       active, created_at, deactivated_at, updated_at
		FROM snapshots
		WHERE image_id = ?
		ORDER BY created_at DESC
	`

	rows, err := d.db.QueryContext(ctx, query, imageID)
	if err != nil {
		return nil, fmt.Errorf("failed to query snapshots: %w", err)
	}
	defer rows.Close()

	var snapshots []*Snapshot
	for rows.Next() {
		var snap Snapshot
		var deactivatedAt sql.NullTime

		err := rows.Scan(
			&snap.ID, &snap.ImageID, &snap.SnapshotID, &snap.SnapshotName,
			&snap.DevicePath, &snap.OriginDeviceID, &snap.Active,
			&snap.CreatedAt, &deactivatedAt, &snap.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan snapshot: %w", err)
		}

		if deactivatedAt.Valid {
			snap.DeactivatedAt = &deactivatedAt.Time
		}

		snapshots = append(snapshots, &snap)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating snapshots: %w", err)
	}

	return snapshots, nil
}

// DeactivateSnapshot marks a snapshot as inactive.
func (d *DB) DeactivateSnapshot(ctx context.Context, snapshotID string) error {
	query := `
		UPDATE snapshots
		SET active = 0,
		    deactivated_at = CURRENT_TIMESTAMP,
		    updated_at = CURRENT_TIMESTAMP
		WHERE snapshot_id = ?
	`

	result, err := d.db.ExecContext(ctx, query, snapshotID)
	if err != nil {
		return fmt.Errorf("failed to deactivate snapshot: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("snapshot not found: %s", snapshotID)
	}

	return nil
}

// DeleteSnapshot deletes a snapshot record.
// This should be used when cleaning up after a failed activation.
func (d *DB) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	query := `DELETE FROM snapshots WHERE snapshot_id = ?`

	result, err := d.db.ExecContext(ctx, query, snapshotID)
	if err != nil {
		return fmt.Errorf("failed to delete snapshot: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("snapshot not found: %s", snapshotID)
	}

	return nil
}

// ListActiveSnapshots lists all active snapshots.
func (d *DB) ListActiveSnapshots(ctx context.Context) ([]*Snapshot, error) {
	query := `
		SELECT id, image_id, snapshot_id, snapshot_name, device_path, origin_device_id,
		       active, created_at, deactivated_at, updated_at
		FROM snapshots
		WHERE active = 1
		ORDER BY created_at DESC
	`

	rows, err := d.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list active snapshots: %w", err)
	}
	defer rows.Close()

	var snapshots []*Snapshot
	for rows.Next() {
		var snap Snapshot
		var deactivatedAt sql.NullTime

		err := rows.Scan(
			&snap.ID, &snap.ImageID, &snap.SnapshotID, &snap.SnapshotName,
			&snap.DevicePath, &snap.OriginDeviceID, &snap.Active,
			&snap.CreatedAt, &deactivatedAt, &snap.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan snapshot: %w", err)
		}

		if deactivatedAt.Valid {
			snap.DeactivatedAt = &deactivatedAt.Time
		}

		snapshots = append(snapshots, &snap)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating snapshots: %w", err)
	}

	return snapshots, nil
}
