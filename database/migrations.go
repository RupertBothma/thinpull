package database

import "fmt"

// Migration 2: Add trace correlation fields for observability
const migration002TraceCorrelation = `
-- Add trace_id and span_id to images table for request correlation
ALTER TABLE images ADD COLUMN trace_id TEXT;
ALTER TABLE images ADD COLUMN span_id TEXT;

-- Add trace_id to unpacked_images table
ALTER TABLE unpacked_images ADD COLUMN trace_id TEXT;

-- Add trace_id to snapshots table
ALTER TABLE snapshots ADD COLUMN trace_id TEXT;

-- Add indexes for trace lookups
CREATE INDEX IF NOT EXISTS idx_images_trace_id ON images(trace_id);
CREATE INDEX IF NOT EXISTS idx_unpacked_images_trace_id ON unpacked_images(trace_id);
CREATE INDEX IF NOT EXISTS idx_snapshots_trace_id ON snapshots(trace_id);
`

// migrations contains all database migrations in order
var migrations = []struct {
	version     int
	description string
	sql         string
}{
	{
		version:     1,
		description: "Initial schema with images, unpacked_images, and snapshots tables",
		sql:         initialSchema,
	},
	{
		version:     2,
		description: "Add trace_id fields for OpenTelemetry correlation",
		sql:         migration002TraceCorrelation,
	},
}

// ApplyMigrations applies all pending database migrations
func (d *DB) ApplyMigrations() error {
	// Get current version
	currentVersion := 0
	row := d.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations")
	if err := row.Scan(&currentVersion); err != nil {
		// If table doesn't exist, start from 0
		currentVersion = 0
	}

	// Apply pending migrations
	for _, migration := range migrations {
		if migration.version <= currentVersion {
			continue
		}

		// Execute migration
		if _, err := d.db.Exec(migration.sql); err != nil {
			return fmt.Errorf("failed to apply migration %d: %w", migration.version, err)
		}

		// Record migration
		if _, err := d.db.Exec(
			"INSERT INTO schema_migrations (version, description) VALUES (?, ?)",
			migration.version,
			migration.description,
		); err != nil {
			return fmt.Errorf("failed to record migration %d: %w", migration.version, err)
		}
	}

	return nil
}
