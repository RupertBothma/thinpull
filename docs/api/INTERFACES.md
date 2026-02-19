# API Interfaces Reference

**Document Type**: API Reference  
**Status**: Approved  
**Version**: 1.0  
**Last Updated**: 2025-11-21

---

## Overview

This document provides the complete API reference for Request/Response types, CLI commands, and configuration interfaces used throughout the Fly.io Container Image Management System.

---

## FSM Request/Response Types

### Download FSM

#### ImageDownloadRequest

Request type for the Download FSM.

```go
type ImageDownloadRequest struct {
    S3Key   string `json:"s3_key"`   // S3 object key (e.g., "images/example.tar")
    ImageID string `json:"image_id"` // Unique image identifier
    Bucket  string `json:"bucket,omitempty"` // Optional S3 bucket override
}
```

**Fields**:
- `S3Key`: S3 object key to download (required)
- `ImageID`: Unique identifier for the image (required)
- `Bucket`: S3 bucket name (optional, defaults to configured bucket)

**Validation**:
- `S3Key`: Max 1024 characters, no path traversal, alphanumeric + `-_./`
- `ImageID`: Max 255 characters, alphanumeric + `-_`
- `Bucket`: Max 255 characters, valid S3 bucket name format

**Example**:
```go
req := &ImageDownloadRequest{
    S3Key:   "images/alpine-3.18.tar",
    ImageID: "alpine-3.18-001",
    Bucket:  "flyio-container-images",
}
```

#### ImageDownloadResponse

Response type for the Download FSM.

```go
type ImageDownloadResponse struct {
    ImageID      string    `json:"image_id"`
    LocalPath    string    `json:"local_path"`
    Checksum     string    `json:"checksum"`
    SizeBytes    int64     `json:"size_bytes"`
    Downloaded   bool      `json:"downloaded"`
    DownloadedAt time.Time `json:"downloaded_at,omitempty"`
}
```

**Fields**:
- `ImageID`: Image identifier (from request)
- `LocalPath`: Local filesystem path to downloaded tarball
- `Checksum`: SHA256 hash of the file
- `SizeBytes`: File size in bytes
- `Downloaded`: `true` if downloaded in this run, `false` if already existed
- `AlreadyExist`: `true` if skipped via idempotency (fsm.Handoff)

**Example**:
```go
resp := &ImageDownloadResponse{
    ImageID:      "alpine-3.18-001",
    LocalPath:    "/var/lib/flyio/images/alpine-3.18-001.tar",
    Checksum:     "abc123def456...",
    SizeBytes:    5242880,
    Downloaded:   true,
    AlreadyExist: false,
}
```

---

### Unpack FSM

#### ImageUnpackRequest

Request type for the Unpack FSM.

```go
type ImageUnpackRequest struct {
    ImageID    string `json:"image_id"`
    LocalPath  string `json:"local_path"`
    Checksum   string `json:"checksum"`
    PoolName   string `json:"pool_name,omitempty"`
    DeviceSize int64  `json:"device_size,omitempty"`
}
```

**Fields**:
- `ImageID`: Unique image identifier (from Download FSM)
- `LocalPath`: Local path to tarball (from Download FSM)
- `Checksum`: SHA256 hash for verification (from Download FSM)
- `PoolName`: Devicemapper pool name (optional, defaults to "pool")
- `DeviceSize`: Device size in bytes (optional, defaults to 10GB)

**Example**:
```go
req := &ImageUnpackRequest{
    ImageID:    "alpine-3.18-001",
    LocalPath:  "/var/lib/flyio/images/alpine-3.18-001.tar",
    Checksum:   "abc123def456...",
    PoolName:   "pool",
    DeviceSize: 10737418240, // 10GB
}
```

#### ImageUnpackResponse

Response type for the Unpack FSM.

```go
type ImageUnpackResponse struct {
    ImageID    string    `json:"image_id"`
    DeviceID   string    `json:"device_id"`
    DeviceName string    `json:"device_name"`
    DevicePath string    `json:"device_path"`
    SizeBytes  int64     `json:"size_bytes"`
    FileCount  int       `json:"file_count"`
    Unpacked   bool      `json:"unpacked"`
    UnpackedAt time.Time `json:"unpacked_at,omitempty"`
}
```

**Fields**:
- `ImageID`: Image identifier
- `DeviceID`: Devicemapper thin device ID (numeric)
- `DeviceName`: Human-readable device name
- `DevicePath`: Full path to device node (e.g., `/dev/mapper/thin-12345`)
- `SizeBytes`: Total extracted size in bytes
- `FileCount`: Number of files extracted
- `Unpacked`: `true` if unpacked in this run
- `AlreadyExist`: `true` if skipped via idempotency

**Example**:
```go
resp := &ImageUnpackResponse{
    ImageID:     "alpine-3.18-001",
    DeviceID:    "12345",
    DeviceName:  "thin-12345",
    DevicePath:  "/dev/mapper/thin-12345",
    SizeBytes:   5000000,
    FileCount:   1500,
    Unpacked:    true,
    AlreadyExist: false,
}
```

---

### Activate FSM

#### ImageActivateRequest

Request type for the Activate FSM.

```go
type ImageActivateRequest struct {
    ImageID      string `json:"image_id"`
    DeviceID     string `json:"device_id"`
    DeviceName   string `json:"device_name"`
    SnapshotName string `json:"snapshot_name,omitempty"`
    PoolName     string `json:"pool_name,omitempty"`
}
```

**Fields**:
- `ImageID`: Unique image identifier (from Unpack FSM)
- `DeviceID`: Origin device ID (from Unpack FSM)
- `DeviceName`: Origin device name (from Unpack FSM)
- `SnapshotName`: Snapshot name (optional, generated if not provided)
- `PoolName`: Devicemapper pool name (optional, defaults to "pool")

**Example**:
```go
req := &ImageActivateRequest{
    ImageID:      "alpine-3.18-001",
    DeviceID:     "12345",
    DeviceName:   "thin-12345",
    SnapshotName: "snapshot-alpine-3.18-001",
    PoolName:     "pool",
}
```

#### ImageActivateResponse

Response type for the Activate FSM.

```go
type ImageActivateResponse struct {
    ImageID      string `json:"image_id"`
    SnapshotID   string `json:"snapshot_id"`
    SnapshotName string `json:"snapshot_name"`
    DevicePath   string `json:"device_path"`
    Active       bool   `json:"active"`
    Activated    bool   `json:"activated"`
}
```

**Fields**:
- `ImageID`: Image identifier
- `SnapshotID`: Devicemapper snapshot ID (numeric)
- `SnapshotName`: Human-readable snapshot name
- `DevicePath`: Full path to snapshot device (e.g., `/dev/mapper/snapshot-alpine-3.18-001`)
- `Active`: `true` if snapshot is active
- `Activated`: `true` if activated in this run, `false` if already existed

**Example**:
```go
resp := &ImageActivateResponse{
    ImageID:      "alpine-3.18-001",
    SnapshotID:   "67890",
    SnapshotName: "snapshot-alpine-3.18-001",
    DevicePath:   "/dev/mapper/snapshot-alpine-3.18-001",
    Active:       true,
    Activated:    true,
}
```

---

## Configuration Types

### Config

Main application configuration.

```go
type Config struct {
    // S3 Configuration
    S3Bucket    string
    S3Region    string
    S3AccessKey string
    S3SecretKey string
    
    // Database Configuration
    DBPath      string
    
    // FSM Configuration
    FSMDBPath   string
    
    // DeviceMapper Configuration
    PoolName    string
    
    // Storage Configuration
    LocalDir    string
    
    // Queue Configuration
    DownloadQueueSize int
    UnpackQueueSize   int
    
    // Timeout Configuration
    DownloadTimeout time.Duration
    UnpackTimeout   time.Duration
}
```

**Default Values**:
```go
func DefaultConfig() Config {
    return Config{
        S3Bucket:          "flyio-container-images",
        S3Region:          "us-east-1",
        DBPath:            "/var/lib/flyio/images.db",
        FSMDBPath:         "/var/lib/flyio/fsm",
        PoolName:          "pool",
        LocalDir:          "/var/lib/flyio/images",
        DownloadQueueSize: 5,
        UnpackQueueSize:   2,
        DownloadTimeout:   5 * time.Minute,
        UnpackTimeout:     30 * time.Minute,
    }
}
```

---

## CLI Commands

### process-image

Process a container image through the complete pipeline (download → unpack → activate).

**Usage**:
```bash
flyio-image-manager process-image --s3-key <key> --image-id <id> [options]
```

**Flags**:
- `--s3-key`: S3 object key (required)
- `--image-id`: Unique image identifier (required)
- `--bucket`: S3 bucket name (optional, default: from config)
- `--pool`: Devicemapper pool name (optional, default: "pool")
- `--device-size`: Device size in bytes (optional, default: 10GB)

**Example**:
```bash
sudo flyio-image-manager process-image \
  --s3-key "images/alpine-3.18.tar" \
  --image-id "alpine-3.18-001"
```

### list-images

List downloaded images.

**Usage**:
```bash
flyio-image-manager list-images [--status <status>]
```

**Flags**:
- `--status`: Filter by download status (optional: pending, downloading, completed, failed)

**Example**:
```bash
flyio-image-manager list-images --status completed
```

### list-snapshots

List active snapshots.

**Usage**:
```bash
flyio-image-manager list-snapshots [--active-only]
```

**Flags**:
- `--active-only`: Show only active snapshots (default: true)

**Example**:
```bash
flyio-image-manager list-snapshots
```

### status

Check FSM run status.

**Usage**:
```bash
flyio-image-manager status --run-id <id>
```

**Flags**:
- `--run-id`: FSM run ID (ULID format)

**Example**:
```bash
flyio-image-manager status --run-id 01HQZX9YFQR8TNKQZ7VXYZ1234
```

### verify-system

Verify system requirements and configuration.

**Usage**:
```bash
flyio-image-manager verify-system
```

**Checks**:
- Go version
- DeviceMapper pool status
- Database connectivity
- S3 access
- Disk space

**Example**:
```bash
flyio-image-manager verify-system
```

---

## Environment Variables

### S3 Configuration

- `AWS_ACCESS_KEY_ID`: AWS access key
- `AWS_SECRET_ACCESS_KEY`: AWS secret key
- `AWS_REGION`: AWS region (default: us-east-1)
- `S3_BUCKET`: S3 bucket name (default: flyio-container-images)

### Database Configuration

- `DB_PATH`: SQLite database path (default: /var/lib/flyio/images.db)
- `FSM_DB_PATH`: FSM database path (default: /var/lib/flyio/fsm)

### DeviceMapper Configuration

- `POOL_NAME`: Devicemapper pool name (default: pool)

### Storage Configuration

- `LOCAL_DIR`: Local storage directory (default: /var/lib/flyio/images)

### Queue Configuration

- `DOWNLOAD_QUEUE_SIZE`: Max concurrent downloads (default: 5)
- `UNPACK_QUEUE_SIZE`: Max concurrent unpacking (default: 2)

### Timeout Configuration

- `DOWNLOAD_TIMEOUT`: Download timeout in seconds (default: 300)
- `UNPACK_TIMEOUT`: Unpack timeout in seconds (default: 1800)

---

## References

- [Database Schema](DATABASE.md) - Database models and queries
- [FSM Library](FSM_LIBRARY.md) - FSM framework API
- [FSM Flow Design](../design/FSM_FLOWS.md) - State machine flows
- [Quick Start Guide](../guide/QUICKSTART.md) - CLI usage examples

