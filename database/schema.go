package database

// schemaMigrationsTable creates the schema_migrations table for tracking database versions.
const schemaMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    description TEXT
);
`

// initialSchema contains the initial database schema (version 1).
const initialSchema = `
-- images table: tracks downloaded container images from S3
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
    download_started_at DATETIME,
    downloaded_at DATETIME,
    activated_at DATETIME,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    
    CHECK (download_status IN ('pending', 'downloading', 'completed', 'failed')),
    CHECK (activation_status IN ('inactive', 'active', 'failed')),
    CHECK (size_bytes >= 0)
);

CREATE INDEX IF NOT EXISTS idx_images_s3_key ON images(s3_key);
CREATE INDEX IF NOT EXISTS idx_images_image_id ON images(image_id);
CREATE INDEX IF NOT EXISTS idx_images_download_status ON images(download_status);
CREATE INDEX IF NOT EXISTS idx_images_activation_status ON images(activation_status);
CREATE INDEX IF NOT EXISTS idx_images_downloaded_at ON images(downloaded_at);
CREATE INDEX IF NOT EXISTS idx_images_download_started_at ON images(download_started_at);

-- unpacked_images table: tracks images extracted into devicemapper devices
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

CREATE INDEX IF NOT EXISTS idx_unpacked_images_image_id ON unpacked_images(image_id);
CREATE INDEX IF NOT EXISTS idx_unpacked_images_device_id ON unpacked_images(device_id);
CREATE INDEX IF NOT EXISTS idx_unpacked_images_device_name ON unpacked_images(device_name);
CREATE INDEX IF NOT EXISTS idx_unpacked_images_unpacked_at ON unpacked_images(unpacked_at);

-- snapshots table: tracks active devicemapper snapshots
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

CREATE INDEX IF NOT EXISTS idx_snapshots_image_id ON snapshots(image_id);
CREATE INDEX IF NOT EXISTS idx_snapshots_snapshot_id ON snapshots(snapshot_id);
CREATE INDEX IF NOT EXISTS idx_snapshots_snapshot_name ON snapshots(snapshot_name);
CREATE INDEX IF NOT EXISTS idx_snapshots_active ON snapshots(active);
CREATE INDEX IF NOT EXISTS idx_snapshots_origin_device_id ON snapshots(origin_device_id);
CREATE INDEX IF NOT EXISTS idx_snapshots_created_at ON snapshots(created_at);
`

// imageLocksSchema adds the image_locks table for per-image concurrency control (version 2).
// This table prevents multiple Unpack FSMs from operating on the same image concurrently,
// which could cause devicemapper pool contention and kernel panics.
const imageLocksSchema = `
-- image_locks table: tracks exclusive locks for images being unpacked
CREATE TABLE IF NOT EXISTS image_locks (
    image_id TEXT PRIMARY KEY,
    locked_at INTEGER NOT NULL,
    locked_by TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_image_locks_locked_at ON image_locks(locked_at);
`
