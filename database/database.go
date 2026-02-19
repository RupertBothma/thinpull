// Package database provides SQLite database operations for container image tracking.
//
// This package implements the persistence layer for the Fly.io Container Image Management
// System, tracking downloaded images, unpacked devicemapper devices, and active snapshots.
//
// The database uses SQLite with WAL (Write-Ahead Logging) mode for concurrent access and
// maintains referential integrity through foreign keys.
//
// # Usage Example
//
//	// Open database with default configuration
//	db, err := database.New(database.DefaultConfig())
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer db.Close()
//
//	// Check if image already downloaded
//	image, err := db.CheckImageDownloaded(ctx, "images/alpine-3.18.tar")
//	if err != nil {
//		log.Fatal(err)
//	}
//	if image != nil {
//		log.Printf("Image already downloaded: %s", image.ImageID)
//	}
//
//	// Store downloaded image metadata
//	err = db.StoreImageMetadata(ctx, imageID, s3Key, localPath, checksum, sizeBytes)
//	if err != nil {
//		log.Fatal(err)
//	}
//
// # Schema
//
// The database maintains three main tables:
//   - images: Downloaded container images from S3
//   - unpacked_images: Images extracted into devicemapper devices
//   - snapshots: Active devicemapper snapshots
//
// See schema.go for complete table definitions and indexes.
//
// # Concurrency
//
// The database is configured for safe concurrent access:
//   - WAL mode allows concurrent reads while writes are in progress
//   - Connection pool (10 max open, 5 max idle)
//   - 5-second busy timeout for lock contention
//   - Foreign key constraints ensure referential integrity
package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // SQLite driver
)

// DB wraps the SQL database with helper methods for image management.
type DB struct {
	db   *sql.DB
	path string // Path to the database file (for diagnostic logging)
}

// Config holds database configuration.
type Config struct {
	// Path to the SQLite database file
	Path string

	// MaxOpenConns is the maximum number of open connections
	MaxOpenConns int

	// MaxIdleConns is the maximum number of idle connections
	MaxIdleConns int

	// ConnMaxLifetime is the maximum lifetime of a connection
	ConnMaxLifetime time.Duration
}

// DefaultConfig returns a default database configuration.
func DefaultConfig() Config {
	return Config{
		Path:            "/var/lib/flyio/images.db",
		MaxOpenConns:    10,
		MaxIdleConns:    5,
		ConnMaxLifetime: 1 * time.Hour,
	}
}

// New creates a new database connection and initializes the schema.
//
// It configures SQLite with optimal settings for concurrent access and performance:
//   - WAL (Write-Ahead Logging) for concurrent reads during writes
//   - Foreign key constraints enabled
//   - NORMAL synchronous mode (balances durability and speed)
//   - 10MB cache, 5-second busy timeout
//   - Memory-mapped I/O (256MB)
//
// The function automatically creates tables if they don't exist and applies
// any pending schema migrations.
//
// Parameters:
//   - cfg: Database configuration (path, connection pool settings)
//
// Returns:
//   - *DB: Configured database connection
//   - error: Any error during connection, configuration, or schema initialization
//
// Example:
//
//	db, err := database.New(database.Config{
//		Path:            "/var/lib/flyio/images.db",
//		MaxOpenConns:    10,
//		MaxIdleConns:    5,
//		ConnMaxLifetime: time.Hour,
//	})
//	if err != nil {
//		return fmt.Errorf("database init failed: %w", err)
//	}
//	defer db.Close()
func New(cfg Config) (*DB, error) {
	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	// Set SQLite pragmas for optimal performance and concurrency
	pragmas := []string{
		"PRAGMA journal_mode = WAL",    // Write-Ahead Logging for better concurrency
		"PRAGMA foreign_keys = ON",     // Enable foreign key constraints
		"PRAGMA synchronous = NORMAL",  // Balance durability and performance
		"PRAGMA cache_size = -10000",   // 10MB cache
		"PRAGMA busy_timeout = 5000",   // 5 second timeout for locks
		"PRAGMA temp_store = MEMORY",   // Use memory for temp tables
		"PRAGMA mmap_size = 268435456", // 256MB memory-mapped I/O
	}

	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("failed to set pragma %q: %w", pragma, err)
		}
	}

	d := &DB{
		db:   db,
		path: cfg.Path,
	}

	// Initialize schema
	if err := d.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return d, nil
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.db.Close()
}

// Ping verifies the database connection is alive.
func (d *DB) Ping(ctx context.Context) error {
	return d.db.PingContext(ctx)
}

// Path returns the database file path.
func (d *DB) Path() string {
	return d.path
}

// initSchema creates the database schema if it doesn't exist.
func (d *DB) initSchema() error {
	// Create schema_migrations table first
	if _, err := d.db.Exec(schemaMigrationsTable); err != nil {
		return fmt.Errorf("failed to create schema_migrations table: %w", err)
	}

	// Run migrations
	migrations := []migration{
		{version: 1, description: "Initial schema", sql: initialSchema},
		{version: 2, description: "Add image_locks table", sql: imageLocksSchema},
	}

	for _, m := range migrations {
		if err := d.runMigration(m); err != nil {
			return fmt.Errorf("migration %d failed: %w", m.version, err)
		}
	}

	return nil
}

type migration struct {
	version     int
	description string
	sql         string
}

func (d *DB) runMigration(m migration) error {
	// Check if migration already applied
	var exists bool
	err := d.db.QueryRow("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = ?)", m.version).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check migration status: %w", err)
	}

	if exists {
		return nil // Migration already applied
	}

	// Run migration in a transaction
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Execute migration SQL
	if _, err := tx.Exec(m.sql); err != nil {
		return fmt.Errorf("failed to execute migration SQL: %w", err)
	}

	// Record migration
	if _, err := tx.Exec("INSERT INTO schema_migrations (version, description) VALUES (?, ?)", m.version, m.description); err != nil {
		return fmt.Errorf("failed to record migration: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit migration: %w", err)
	}

	return nil
}

// AcquireImageLock attempts to acquire an exclusive lock for the given image.
// This prevents multiple Unpack FSMs from operating on the same image concurrently,
// which could cause devicemapper pool contention and kernel panics.
//
// The lock is implemented using SQLite's UNIQUE constraint on image_id.
// If another FSM already holds the lock, this returns an error.
//
// Parameters:
//   - ctx: Context for cancellation
//   - imageID: The image identifier to lock
//   - lockedBy: Identifier of the lock holder (e.g., "unpack-fsm")
//
// Returns:
//   - error: nil on success, error if lock already held or database error
//
// Example:
//
//	err := db.AcquireImageLock(ctx, "alpine-3.18", "unpack-fsm")
//	if err != nil {
//		// Image is already being unpacked by another FSM
//		return fsm.Handoff(...)
//	}
//	defer db.ReleaseImageLock(ctx, "alpine-3.18")
func (d *DB) AcquireImageLock(ctx context.Context, imageID, lockedBy string) error {
	query := `INSERT INTO image_locks (image_id, locked_at, locked_by) VALUES (?, ?, ?)`
	_, err := d.db.ExecContext(ctx, query, imageID, time.Now().Unix(), lockedBy)
	if err != nil {
		// Check if this is a UNIQUE constraint violation (lock already held)
		if strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "constraint failed") {
			// Try to get info about who holds the lock
			var holder string
			var lockedAt int64
			queryLock := `SELECT locked_by, locked_at FROM image_locks WHERE image_id = ?`
			if scanErr := d.db.QueryRowContext(ctx, queryLock, imageID).Scan(&holder, &lockedAt); scanErr == nil {
				lockTime := time.Unix(lockedAt, 0)
				return fmt.Errorf("image %s is already locked by %s (acquired at %s)", imageID, holder, lockTime.Format(time.RFC3339))
			}
			return fmt.Errorf("image %s is already locked by another process", imageID)
		}
		return fmt.Errorf("failed to acquire image lock: %w", err)
	}
	return nil
}

// ReleaseImageLock releases the lock for the given image.
// This is idempotent - it does not error if the lock doesn't exist.
//
// Should be called after successful unpack completion or when the FSM aborts.
//
// Parameters:
//   - ctx: Context for cancellation
//   - imageID: The image identifier to unlock
//
// Returns:
//   - error: nil on success (including if lock doesn't exist), error on database failure
//
// Example:
//
//	defer db.ReleaseImageLock(ctx, "alpine-3.18")
func (d *DB) ReleaseImageLock(ctx context.Context, imageID string) error {
	query := `DELETE FROM image_locks WHERE image_id = ?`
	_, err := d.db.ExecContext(ctx, query, imageID)
	if err != nil {
		return fmt.Errorf("failed to release image lock: %w", err)
	}
	// Note: We don't check rows affected - this is intentionally idempotent
	return nil
}

// IsImageLocked checks if the given image is currently locked.
//
// Parameters:
//   - ctx: Context for cancellation
//   - imageID: The image identifier to check
//
// Returns:
//   - bool: true if locked, false if not locked
//   - error: nil on success, error on database failure
//
// Example:
//
//	locked, err := db.IsImageLocked(ctx, "alpine-3.18")
//	if err != nil {
//		return err
//	}
//	if locked {
//		log.Info("image is currently being unpacked")
//	}
func (d *DB) IsImageLocked(ctx context.Context, imageID string) (bool, error) {
	var count int
	query := `SELECT COUNT(*) FROM image_locks WHERE image_id = ?`
	err := d.db.QueryRowContext(ctx, query, imageID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("failed to check image lock: %w", err)
	}
	return count > 0, nil
}
