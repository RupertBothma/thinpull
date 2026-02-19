# Database API Reference

**Document Type**: API Reference
**Status**: Approved
**Version**: 1.1
**Last Updated**: 2025-11-24

---

## Overview

This document provides the complete API reference for the SQLite database layer, including schema definitions, CRUD operations, common queries, and transaction patterns.

**Implementation**: `database/` package

---

## Schema

### images Table

Tracks downloaded container images from S3.

```sql
CREATE TABLE IF NOT EXISTS images (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    image_id TEXT NOT NULL UNIQUE,
    s3_key TEXT NOT NULL UNIQUE,
    local_path TEXT NOT NULL,
    checksum TEXT,
    size_bytes INTEGER NOT NULL,
    download_status TEXT NOT NULL DEFAULT 'pending',
    activation_status TEXT DEFAULT 'inactive',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    downloaded_at DATETIME,
    activated_at DATETIME,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    
    CHECK (download_status IN ('pending', 'downloading', 'completed', 'failed')),
    CHECK (activation_status IN ('inactive', 'active', 'failed')),
    CHECK (size_bytes >= 0)
);

CREATE INDEX idx_images_s3_key ON images(s3_key);
CREATE INDEX idx_images_image_id ON images(image_id);
CREATE INDEX idx_images_download_status ON images(download_status);
CREATE INDEX idx_images_activation_status ON images(activation_status);
CREATE INDEX idx_images_downloaded_at ON images(downloaded_at);
```

**Fields**:
- `id`: Auto-increment primary key
- `image_id`: Unique identifier (UUID or hash)
- `s3_key`: S3 object key (for idempotency)
- `local_path`: Local filesystem path to tarball
- `checksum`: SHA256 hash
- `size_bytes`: File size in bytes
- `download_status`: pending | downloading | completed | failed
- `activation_status`: inactive | active | failed
- `created_at`, `downloaded_at`, `activated_at`, `updated_at`: Timestamps

### unpacked_images Table

Tracks images extracted into devicemapper devices.

```sql
CREATE TABLE IF NOT EXISTS unpacked_images (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    image_id TEXT NOT NULL UNIQUE,
    device_id TEXT NOT NULL UNIQUE,
    device_name TEXT NOT NULL UNIQUE,
    device_path TEXT NOT NULL,
    size_bytes INTEGER NOT NULL,
    file_count INTEGER NOT NULL,
    layout_verified BOOLEAN NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    unpacked_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    
    FOREIGN KEY (image_id) REFERENCES images(image_id) ON DELETE CASCADE,
    CHECK (size_bytes >= 0),
    CHECK (file_count >= 0),
    CHECK (layout_verified IN (0, 1))
);

CREATE INDEX idx_unpacked_images_image_id ON unpacked_images(image_id);
CREATE INDEX idx_unpacked_images_device_id ON unpacked_images(device_id);
CREATE INDEX idx_unpacked_images_device_name ON unpacked_images(device_name);
CREATE INDEX idx_unpacked_images_unpacked_at ON unpacked_images(unpacked_at);
```

**Fields**:
- `id`: Auto-increment primary key
- `image_id`: Links to images table (UNIQUE, CASCADE on delete)
- `device_id`: Devicemapper thin device ID (numeric)
- `device_name`: Human-readable device name
- `device_path`: Full path to device node (`/dev/mapper/...`)
- `size_bytes`: Total extracted size
- `file_count`: Number of files extracted
- `layout_verified`: Filesystem layout validated (0 or 1)
- `created_at`, `unpacked_at`, `updated_at`: Timestamps

### snapshots Table

Tracks active devicemapper snapshots.

```sql
CREATE TABLE IF NOT EXISTS snapshots (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    image_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL UNIQUE,
    snapshot_name TEXT NOT NULL UNIQUE,
    device_path TEXT NOT NULL,
    origin_device_id TEXT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deactivated_at DATETIME,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    
    FOREIGN KEY (image_id) REFERENCES images(image_id) ON DELETE CASCADE,
    FOREIGN KEY (origin_device_id) REFERENCES unpacked_images(device_id) ON DELETE RESTRICT,
    CHECK (active IN (0, 1))
);

CREATE INDEX idx_snapshots_image_id ON snapshots(image_id);
CREATE INDEX idx_snapshots_snapshot_id ON snapshots(snapshot_id);
CREATE INDEX idx_snapshots_snapshot_name ON snapshots(snapshot_name);
CREATE INDEX idx_snapshots_active ON snapshots(active);
CREATE INDEX idx_snapshots_origin_device_id ON snapshots(origin_device_id);
CREATE INDEX idx_snapshots_created_at ON snapshots(created_at);
```

**Fields**:
- `id`: Auto-increment primary key
- `image_id`: Links to images table (CASCADE on delete)
- `snapshot_id`: Devicemapper snapshot ID (numeric)
- `snapshot_name`: Human-readable snapshot name
- `device_path`: Full path to snapshot device node
- `origin_device_id`: Links to unpacked_images.device_id (RESTRICT on delete)
- `active`: Snapshot active status (0 or 1)
- `created_at`, `deactivated_at`, `updated_at`: Timestamps

### image_locks Table

Tracks exclusive locks on images being unpacked (added in version 2).

```sql
CREATE TABLE IF NOT EXISTS image_locks (
    image_id TEXT PRIMARY KEY,
    locked_at INTEGER NOT NULL,
    locked_by TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_image_locks_locked_at ON image_locks(locked_at);
```

**Fields**:
- `image_id`: Image being locked (PRIMARY KEY, links to images.image_id)
- `locked_at`: Unix timestamp when lock was acquired
- `locked_by`: Identifier of lock holder (e.g., "unpack-fsm", "process-123")

**Purpose**: Prevents concurrent unpack operations on the same image across multiple processes or FSMs. Ensures only one FSM can unpack a given image at a time.

**Lock Lifecycle**:
1. Acquired at start of Unpack FSM (`checkUnpacked` transition)
2. Released on Handoff (image already unpacked)
3. Released before `fsm.Abort` (validation failure, pool exhaustion, etc.)
4. Released after successful unpack (`updateDB` transition)

### schema_migrations Table

Tracks database schema versions.

```sql
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    description TEXT
);
```

**Migrations**:
- **Version 1**: Initial schema (images, unpacked_images, snapshots tables)
- **Version 2**: Add image_locks table for concurrency control (2025-11-24)

---

## Database Configuration

### SQLite Pragmas

```sql
PRAGMA journal_mode = WAL;              -- Write-Ahead Logging
PRAGMA foreign_keys = ON;               -- Enable foreign key constraints
PRAGMA synchronous = NORMAL;            -- Balance durability/performance
PRAGMA cache_size = -10000;             -- 10MB cache
PRAGMA busy_timeout = 5000;             -- 5 second timeout
PRAGMA temp_store = MEMORY;             -- Use memory for temp tables
PRAGMA mmap_size = 268435456;           -- 256MB memory-mapped I/O
```

### Connection Pool

```go
db.SetMaxOpenConns(10)                  // Max 10 concurrent connections
db.SetMaxIdleConns(5)                   // Keep 5 idle connections
db.SetConnMaxLifetime(1 * time.Hour)    // Recycle connections after 1 hour
```

---

## API Reference

### Database Connection

#### New(cfg Config) (*DB, error)

Creates a new database connection and initializes the schema.

```go
cfg := database.DefaultConfig()
cfg.Path = "/var/lib/flyio/images.db"

db, err := database.New(cfg)
if err != nil {
    log.Fatal(err)
}
defer db.Close()
```

**Parameters**:
- `cfg`: Database configuration

**Returns**:
- `*DB`: Database instance
- `error`: Error if connection or initialization fails

#### Close() error

Closes the database connection.

```go
err := db.Close()
```

#### Ping(ctx context.Context) error

Verifies the database connection is alive.

```go
err := db.Ping(ctx)
```

---

### Image Operations

#### CheckImageDownloaded(ctx context.Context, s3Key string) (*Image, error)

Checks if an image has already been downloaded.

```go
existing, err := db.CheckImageDownloaded(ctx, "images/example.tar")
if err != nil {
    return err
}
if existing != nil {
    // Image already downloaded
    fmt.Printf("Image exists: %s\n", existing.LocalPath)
}
```

**Parameters**:
- `ctx`: Context for cancellation
- `s3Key`: S3 object key

**Returns**:
- `*Image`: Image if found and completed, nil if not found
- `error`: Error if query fails

#### StoreImageMetadata(ctx, imageID, s3Key, localPath, checksum string, sizeBytes int64) error

Stores or updates image metadata after successful download.

```go
err := db.StoreImageMetadata(ctx, 
    "img-123", 
    "images/example.tar",
    "/var/lib/flyio/images/img-123.tar",
    "abc123...",
    1024000)
```

**Parameters**:
- `ctx`: Context
- `imageID`: Unique image identifier
- `s3Key`: S3 object key
- `localPath`: Local file path
- `checksum`: SHA256 hash
- `sizeBytes`: File size

**Returns**:
- `error`: Error if insert/update fails

#### GetImageByID(ctx context.Context, imageID string) (*Image, error)

Retrieves an image by its image_id.

```go
img, err := db.GetImageByID(ctx, "img-123")
```

#### UpdateImageActivationStatus(ctx context.Context, imageID, status string) error

Updates the activation status of an image.

```go
err := db.UpdateImageActivationStatus(ctx, "img-123", "active")
```

#### ListImages(ctx context.Context, downloadStatus string) ([]*Image, error)

Lists all images with optional status filter.

```go
// List all completed images
images, err := db.ListImages(ctx, "completed")

// List all images
images, err := db.ListImages(ctx, "")
```

---

### Unpacked Image Operations

#### CheckImageUnpacked(ctx context.Context, imageID string) (*UnpackedImage, error)

Checks if an image has already been unpacked.

```go
existing, err := db.CheckImageUnpacked(ctx, "img-123")
if err != nil {
    return err
}
if existing != nil && existing.LayoutVerified {
    // Image already unpacked and verified
}
```

#### StoreUnpackedImage(ctx, imageID, deviceID, deviceName, devicePath string, sizeBytes int64, fileCount int) error

Stores or updates unpacked image metadata.

```go
err := db.StoreUnpackedImage(ctx,
    "img-123",
    "12345",
    "thin-12345",
    "/dev/mapper/thin-12345",
    5000000,
    1500)
```

#### GetUnpackedImageByID(ctx context.Context, imageID string) (*UnpackedImage, error)

Retrieves an unpacked image by its image_id.

#### GetUnpackedImageByDeviceID(ctx context.Context, deviceID string) (*UnpackedImage, error)

Retrieves an unpacked image by its device_id.

#### DeleteUnpackedImage(ctx context.Context, imageID string) error

Deletes an unpacked image record (cleanup after failure).

#### ListUnpackedImages(ctx context.Context) ([]*UnpackedImage, error)

Lists all unpacked images.

---

### Snapshot Operations

#### CheckSnapshotExists(ctx context.Context, imageID, snapshotName string) (*Snapshot, error)

Checks if a snapshot already exists for an image.

```go
existing, err := db.CheckSnapshotExists(ctx, "img-123", "snapshot-img-123")
if err != nil {
    return err
}
if existing != nil && existing.Active {
    // Snapshot already exists and is active
}
```

#### StoreSnapshot(ctx, imageID, snapshotID, snapshotName, devicePath, originDeviceID string) error

Stores or updates snapshot metadata.

```go
err := db.StoreSnapshot(ctx,
    "img-123",
    "67890",
    "snapshot-img-123",
    "/dev/mapper/snapshot-img-123",
    "12345")
```

#### GetSnapshotByID(ctx context.Context, snapshotID string) (*Snapshot, error)

Retrieves a snapshot by its snapshot_id.

#### GetSnapshotsByImageID(ctx context.Context, imageID string) ([]*Snapshot, error)

Retrieves all snapshots for an image.

#### DeactivateSnapshot(ctx context.Context, snapshotID string) error

Marks a snapshot as inactive.

```go
err := db.DeactivateSnapshot(ctx, "67890")
```

#### DeleteSnapshot(ctx context.Context, snapshotID string) error

Deletes a snapshot record (cleanup after failure).

#### ListActiveSnapshots(ctx context.Context) ([]*Snapshot, error)

Lists all active snapshots.

```go
snapshots, err := db.ListActiveSnapshots(ctx)
for _, snap := range snapshots {
    fmt.Printf("Snapshot: %s -> %s\n", snap.SnapshotName, snap.DevicePath)
}
```

---

### Image Lock Operations

#### AcquireImageLock(ctx context.Context, imageID, lockedBy string) error

Acquires an exclusive lock on an image to prevent concurrent unpack operations.

```go
// Acquire lock at start of Unpack FSM
err := db.AcquireImageLock(ctx, "img-123", "unpack-fsm")
if err != nil {
    // Lock already held by another FSM
    if strings.Contains(err.Error(), "UNIQUE constraint failed") {
        logger.Warn("image is already being unpacked by another FSM")
        return fsm.Handoff(...)
    }
    return err
}
logger.Info("acquired image lock")
```

**Parameters**:
- `ctx`: Context for cancellation
- `imageID`: Image to lock
- `lockedBy`: Identifier of lock holder (e.g., "unpack-fsm", "process-123")

**Returns**:
- `error`: Error if lock acquisition fails (UNIQUE constraint violation if already locked)

**Behavior**:
- Inserts lock record with current timestamp
- Returns descriptive error if lock already held (includes lock holder info)
- Uses UNIQUE constraint on `image_id` for atomic lock acquisition

#### ReleaseImageLock(ctx context.Context, imageID string) error

Releases an exclusive lock on an image.

```go
// Release lock after successful unpack
err := db.ReleaseImageLock(ctx, "img-123")
if err != nil {
    // Log but don't fail - unpack work is already complete
    logger.WithError(err).Error("failed to release image lock")
}
```

**Parameters**:
- `ctx`: Context for cancellation
- `imageID`: Image to unlock

**Returns**:
- `error`: Error if delete fails (nil if lock doesn't exist - idempotent)

**Behavior**:
- Deletes lock record from `image_locks` table
- Idempotent: Does not fail if lock doesn't exist
- Should be called in all FSM exit paths (success, abort, handoff)

#### IsImageLocked(ctx context.Context, imageID string) (bool, error)

Checks if an image is currently locked.

```go
locked, err := db.IsImageLocked(ctx, "img-123")
if err != nil {
    return err
}
if locked {
    logger.Info("image is locked by another FSM")
}
```

**Parameters**:
- `ctx`: Context for cancellation
- `imageID`: Image to check

**Returns**:
- `bool`: True if locked, false otherwise
- `error`: Error if query fails

**Usage**: Diagnostic queries, monitoring, debugging lock contention.

---

## Common Queries

### Idempotency Checks

```sql
-- Check if image downloaded
SELECT id, image_id, local_path, checksum, download_status
FROM images
WHERE s3_key = ? AND download_status = 'completed';

-- Check if image unpacked
SELECT u.id, u.image_id, u.device_id, u.device_name, u.device_path
FROM unpacked_images u
INNER JOIN images i ON u.image_id = i.image_id
WHERE i.s3_key = ? AND u.layout_verified = 1;

-- Check if snapshot exists
SELECT s.id, s.snapshot_id, s.snapshot_name, s.device_path
FROM snapshots s
INNER JOIN images i ON s.image_id = i.image_id
WHERE i.s3_key = ? AND s.active = 1;
```

### Status Queries

```sql
-- Get complete image status
SELECT
    i.image_id,
    i.s3_key,
    i.download_status,
    i.activation_status,
    u.device_name,
    s.snapshot_name,
    s.active AS snapshot_active
FROM images i
LEFT JOIN unpacked_images u ON i.image_id = u.image_id
LEFT JOIN snapshots s ON i.image_id = s.image_id
WHERE i.s3_key = ?;
```

### Lock Management Queries

```sql
-- Check if image is locked
SELECT image_id, locked_at, locked_by
FROM image_locks
WHERE image_id = ?;

-- List all active locks
SELECT image_id, locked_at, locked_by
FROM image_locks
ORDER BY locked_at DESC;

-- Find stale locks (older than 1 hour)
SELECT image_id, locked_at, locked_by
FROM image_locks
WHERE locked_at < unixepoch('now') - 3600;

-- Clean up stale locks (manual maintenance)
DELETE FROM image_locks
WHERE locked_at < unixepoch('now') - 3600;
```

---

## Transaction Patterns

### Download FSM - store-metadata

```sql
BEGIN TRANSACTION;

INSERT INTO images (image_id, s3_key, local_path, checksum, size_bytes, download_status, downloaded_at)
VALUES (?, ?, ?, ?, ?, 'completed', CURRENT_TIMESTAMP)
ON CONFLICT(s3_key) DO UPDATE SET
    local_path = excluded.local_path,
    checksum = excluded.checksum,
    size_bytes = excluded.size_bytes,
    download_status = 'completed',
    downloaded_at = CURRENT_TIMESTAMP,
    updated_at = CURRENT_TIMESTAMP;

COMMIT;
```

### Unpack FSM - update-db

```sql
BEGIN TRANSACTION;

INSERT INTO unpacked_images (image_id, device_id, device_name, device_path, size_bytes, file_count, layout_verified, unpacked_at)
VALUES (?, ?, ?, ?, ?, ?, 1, CURRENT_TIMESTAMP)
ON CONFLICT(image_id) DO UPDATE SET
    device_id = excluded.device_id,
    device_name = excluded.device_name,
    device_path = excluded.device_path,
    size_bytes = excluded.size_bytes,
    file_count = excluded.file_count,
    layout_verified = 1,
    unpacked_at = CURRENT_TIMESTAMP,
    updated_at = CURRENT_TIMESTAMP;

COMMIT;
```

### Activate FSM - register

```sql
BEGIN TRANSACTION;

-- Insert snapshot
INSERT INTO snapshots (image_id, snapshot_id, snapshot_name, device_path, origin_device_id, active, created_at)
VALUES (?, ?, ?, ?, ?, 1, CURRENT_TIMESTAMP)
ON CONFLICT(snapshot_name) DO UPDATE SET
    active = 1,
    updated_at = CURRENT_TIMESTAMP;

-- Update image activation status
UPDATE images
SET activation_status = 'active',
    activated_at = CURRENT_TIMESTAMP,
    updated_at = CURRENT_TIMESTAMP
WHERE image_id = ?;

COMMIT;
```

---

## References

- [System Architecture](../design/SYSTEM_ARCH.md) - Database role in system
- [FSM Flow Design](../design/FSM_FLOWS.md) - Database usage in FSMs
- [Requirements](../spec/REQUIREMENTS.md) - Database requirements

