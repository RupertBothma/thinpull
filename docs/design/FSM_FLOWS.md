# FSM State Machine Flows

**Document Type**: Design
**Status**: Approved
**Version**: 1.1
**Last Updated**: 2025-11-26

---

## Overview

This document defines the three FSMs that orchestrate container image management:
1. **Download FSM**: Retrieves images from S3 with idempotency
2. **Unpack FSM**: Extracts tarballs into devicemapper devices
3. **Activate FSM**: Creates snapshots for active images

Each FSM follows the pattern: **check idempotency → perform work → validate → persist state**

**See Also**: [Durable State Contracts](DURABLE_STATE_CONTRACTS.md) for detailed crash recovery behavior and state-to-database mapping for each FSM state

---

## Download FSM

### State Flow Diagram

```
START → check-exists → download → validate → store-metadata → COMPLETE
         ↓ (exists & valid)
         └──────────────────────────────────────────────────────→ COMPLETE (Handoff)
```

### Transitions

#### 1. check-exists

**Purpose**: Verify if image already downloaded to avoid redundant work

**Implementation**: `download/fsm.go:checkExists()`

**Logic**:
1. Query SQLite `images` table by `s3_key`
2. If exists and `download_status = 'completed'`:
   - Verify file exists at `local_path`
   - Verify file size matches `size_bytes`
   - Compute and verify checksum
   - If all valid → return `fsm.Handoff` (skip remaining transitions)
   - If invalid → proceed to download (re-download)
3. If not exists → proceed to download

**Error Handling**:
- Database errors → standard error (auto-retry, max 3 attempts)
- File system errors → standard error (auto-retry, max 2 attempts)

**Retry Strategy**: Exponential backoff, max 3 retries

---

#### 2. download

**Purpose**: Download tarball from S3 to local filesystem

**Implementation**: `download/fsm.go:downloadFromS3()`

**Logic**:
1. Validate S3 key (no path traversal, max length 1024)
2. Determine local path: `/var/lib/flyio/images/<image_id>.tar`
3. Stream download from S3 using `s3.Client.DownloadImage()`
4. Compute SHA256 checksum during download (single pass)
5. Enforce size limit: max 10GB per image
6. Store to temporary file, then atomic move to final location
7. On failure, cleanup temporary file

**Error Handling**:
- Network errors → standard error (FSM auto-retry)
- S3 access denied → `fsm.Abort` (unrecoverable)
- Invalid S3 key → `fsm.Abort` (validation failure)
- Size limit exceeded → `fsm.Abort` (security violation)
- Disk full → `fsm.Abort` (resource exhaustion)

**Retry Strategy**: Exponential backoff with jitter, max 5 retries  
**Timeout**: 5 minutes (configured via `fsm.WithTimeout`)

---

#### 3. validate

**Purpose**: Verify downloaded tarball integrity and safety

**Implementation**: `download/fsm.go:validateBlob()`

**Logic**:
1. Verify file exists and is non-empty
2. Recompute SHA256 checksum for verification
3. Validate tar structure (can be opened, valid format)
4. Security checks via `performSecurityChecks()`:
   - Scan for path traversal attempts (`..` in paths)
   - Check for absolute paths
   - Check for suspicious symlinks (absolute targets, escaping rootfs)
   - Verify no setuid/setgid binaries
   - Limit total file count (max 100,000 files)
   - Limit individual file sizes (max 1GB per file)
5. On validation failure, cleanup (remove file)

**Error Handling**:
- Corrupted tar → `fsm.Abort` + cleanup (unrecoverable)
- Malicious content detected → `fsm.Abort` + cleanup + log security violation
- File not found → standard error (retry)
- I/O errors → standard error (retry)

**Retry Strategy**: Fixed retry, max 2 attempts

---

#### 4. store-metadata

**Purpose**: Record successful download in SQLite

**Implementation**: `download/fsm.go:storeMetadata()`

**Logic**:
1. Begin transaction
2. Insert/update `images` table:
   - `image_id`: Unique identifier
   - `s3_key`: S3 object key
   - `local_path`: Local file path
   - `checksum`: SHA256 hash
   - `size_bytes`: File size
   - `download_status`: 'completed'
   - `downloaded_at`: Current timestamp
3. Commit transaction
4. Populate Response with image metadata for Unpack FSM

**Error Handling**:
- Unique constraint violation (concurrent download) → ignore, return success
- Database lock → standard error (retry with jitter, max 5 attempts)
- Other database errors → standard error (retry)

**Retry Strategy**: Exponential backoff with jitter, max 5 retries

---

### Request/Response Types

```go
type ImageDownloadRequest struct {
    S3Key   string // S3 object key (e.g., "images/example.tar")
    ImageID string // Unique image identifier
    Bucket  string // S3 bucket (optional, defaults to configured bucket)
}

type ImageDownloadResponse struct {
    ImageID      string
    LocalPath    string
    Checksum     string
    SizeBytes    int64
    Downloaded   bool // true if downloaded, false if already existed
    AlreadyExist bool // true if skipped via idempotency
}
```

### FSM Registration

```go
fsm.Register[ImageDownloadRequest, ImageDownloadResponse](manager, "download-image").
    Start("check-exists", checkExists(deps)).
    To("download", downloadFromS3(deps), fsm.WithTimeout(5*time.Minute)).
    To("validate", validateBlob(deps)).
    To("store-metadata", storeMetadata(deps)).
    End("complete").
    Build(ctx)
```

### Queue Configuration

- **Queue name**: `"downloads"`
- **Concurrency limit**: 5 (max 5 concurrent S3 downloads)
- **Usage**: `fsm.WithQueue("downloads")` when starting FSM

---

## Unpack FSM

### State Flow Diagram

```
START → check-unpacked → create-device → extract-layers → verify-layout → update-db → COMPLETE
         ↓ (unpacked & valid)
         └────────────────────────────────────────────────────────────────────────→ COMPLETE (Handoff)
```

### Transitions

#### 1. check-unpacked

**Purpose**: Verify if image already unpacked to avoid redundant work

**Logic**:
1. Query SQLite `unpacked_images` table by `image_id`
2. If exists and `layout_verified = 1`:
   - Verify devicemapper device exists: `dmsetup info <device_name>`
   - Verify device is active and accessible
   - If all valid → return `fsm.Handoff` (skip remaining transitions)
   - If device missing/corrupted → delete DB entry, proceed to create-device
3. If not exists → proceed to create-device

**Error Handling**:
- Database errors → standard error (retry, max 3 attempts)
- Devicemapper query errors → standard error (retry, max 2 attempts)
- Device corrupted → log warning, proceed to recreate

**Retry Strategy**: Exponential backoff, max 3 retries

---

#### 2. create-device

**Purpose**: Create thin device in devicemapper pool

**Logic**:
1. Generate unique device ID: numeric timestamp-based
2. Determine device size (default: 10GB = 20971520 sectors)
3. Create thin device: `dmsetup message pool 0 "create_thin <device_id>"`
4. Activate device: `dmsetup create <device_name> --table "..."`
5. Create ext4 filesystem: `mkfs.ext4 /dev/mapper/<device_name>`
6. Store device info in Response

**Error Handling**:
- Pool full → `fsm.Abort` (resource exhaustion)
- Device ID collision → generate new ID, retry (max 3 attempts)
- Transient devicemapper errors → standard error (retry)
- Filesystem creation errors → cleanup device, standard error (retry)

**Retry Strategy**: Fixed retry, max 3 attempts

---

#### 3. extract-layers

**Purpose**: Extract tarball to mounted device with security checks

**Logic**:
1. Mount device: `mount /dev/mapper/<device_name> /mnt/flyio/<device_name>`
2. Extract tarball using `extraction.Extractor.Extract()`:
   - Validate each tar entry (path, symlink, permissions)
   - Enforce limits (1GB per file, 10GB total, 100k files)
   - Track progress (files extracted, bytes written)
3. Sync filesystem: `sync`
4. Unmount device: `umount /mnt/flyio/<device_name>`
5. Store extraction stats in Response

**Error Handling**:
- Corrupted tar → cleanup (unmount, deactivate, delete), `fsm.Abort`
- Malicious content → cleanup, `fsm.Abort` + log security violation
- Disk full → cleanup, `fsm.Abort`
- Timeout exceeded (30 min) → cleanup, `fsm.Abort`
- I/O errors → standard error (retry)

**Retry Strategy**: Fixed retry, max 2 attempts  
**Timeout**: 30 minutes (configured via `fsm.WithTimeout`)

---

#### 4. verify-layout

**Purpose**: Validate canonical filesystem layout and enforce additional
security invariants on the unpacked rootfs.

**Logic**:
1. Use existing mount from extract-layers (no extra mount/unmount here).
2. Check for required directories under the mounted device:
   - `/rootfs/` (required)
   - `/rootfs/etc/` (expected)
   - `/rootfs/usr/` (expected)
   - `/rootfs/var/` (expected)
3. Verify permissions on critical directories (no world-writable
   `rootfs/etc`, `rootfs/usr`).
4. Treat any structural or permission violations as unrecoverable for this
   image.
5. Leave device mounted for update-db to perform final unmount and DB write.

**Error Handling**:
- Invalid layout → cleanup (unmount, deactivate, delete), `fsm.Abort`
- Security violations → cleanup, `fsm.Abort` + log
- I/O errors → standard error (retry)

**Retry Strategy**: Fixed retry, max 2 attempts

---

#### 5. update-db

**Purpose**: Record unpacked image in database

**Logic**:
1. Deactivate device: `dmsetup remove <device_name>` (device stays in pool)
2. Begin transaction
3. Insert into `unpacked_images` table:
   - `image_id`: From request
   - `device_id`: Devicemapper device ID
   - `device_name`: Device name
   - `device_path`: `/dev/mapper/<device_name>`
   - `layout_verified`: true
   - `size_bytes`: Total extracted size
   - `file_count`: Number of files
   - `unpacked_at`: Current timestamp
4. Commit transaction
5. Populate Response with device info for Activate FSM

**Error Handling**:
- Deactivate errors → log warning, continue (device may already be inactive)
- Database errors → standard error (retry, max 5 attempts)
- Unique constraint violation → ignore, return success

**Retry Strategy**: Exponential backoff, max 5 retries

---

### Request/Response Types

```go
type ImageUnpackRequest struct {
    ImageID    string
    LocalPath  string // From Download FSM
    Checksum   string // For verification
    PoolName   string // Optional, defaults to configured pool
    DeviceSize int64  // Optional, defaults to 10GB
}

type ImageUnpackResponse struct {
    ImageID     string
    DeviceID    string
    DeviceName  string
    DevicePath  string
    SizeBytes   int64
    FileCount   int
    Unpacked    bool // true if unpacked, false if already existed
}
```

### FSM Registration

```go
fsm.Register[ImageUnpackRequest, ImageUnpackResponse](manager, "unpack-image").
    Start("check-unpacked", checkUnpacked(deps)).
    To("create-device", createDevice(deps)).
    To("extract-layers", extractLayers(deps), fsm.WithTimeout(30*time.Minute)).
    To("verify-layout", verifyLayout(deps)).
    To("update-db", updateDB(deps)).
    End("complete").
    Build(ctx)
```

### Queue Configuration

- **Queue name**: `"unpacking"`
- **Concurrency limit**: 2 (I/O intensive, limit concurrent operations)
- **Usage**: `fsm.WithQueue("unpacking")` when starting FSM

---

## Activate FSM

### State Flow Diagram

```
START → check-snapshot → create-snapshot → register → COMPLETE
         ↓ (snapshot exists & valid)
         └──────────────────────────────────────────→ COMPLETE (Handoff)
```

### Transitions

#### 1. check-snapshot

**Purpose**: Verify if snapshot already exists for this image

**Logic**:
1. Query SQLite `snapshots` table by `image_id` and `snapshot_name`
2. If exists and `active = 1`:
   - Verify snapshot device exists: `dmsetup info <snapshot_name>`
   - Verify snapshot is active
   - Verify origin device matches expected `device_id`
   - If all valid → return `fsm.Handoff` (skip remaining transitions)
   - If snapshot missing/invalid → delete DB entry, proceed to create-snapshot
3. If not exists → proceed to create-snapshot

**Error Handling**:
- Database errors → standard error (retry, max 3 attempts)
- Devicemapper query errors → standard error (retry, max 2 attempts)
- Snapshot corrupted → log warning, proceed to recreate

**Retry Strategy**: Exponential backoff, max 3 retries

---

#### 2. create-snapshot

**Purpose**: Create devicemapper snapshot from unpacked image

**Logic**:
1. Generate unique snapshot ID: numeric timestamp-based
2. Verify origin device exists and is inactive
3. Create snapshot: `dmsetup message pool 0 "create_snap <snapshot_id> <origin_device_id>"`
4. Activate snapshot: `dmsetup create <snapshot_name> --table "..."`
5. Verify snapshot creation succeeded
6. Store snapshot device path in Response: `/dev/mapper/<snapshot_name>`

**Error Handling**:
- Origin device not found → `fsm.Abort` (data missing)
- Pool full → `fsm.Abort` (resource exhaustion)
- Snapshot ID collision → generate new ID, retry (max 3 attempts)
- Transient devicemapper errors → standard error (retry)

**Retry Strategy**: Fixed retry, max 3 attempts

---

#### 3. register

**Purpose**: Record snapshot in database and mark image as active

**Logic**:
1. Begin transaction
2. Insert into `snapshots` table:
   - `image_id`: From request
   - `snapshot_id`: Devicemapper snapshot ID
   - `snapshot_name`: Snapshot device name
   - `device_path`: `/dev/mapper/<snapshot_name>`
   - `origin_device_id`: Origin device ID
   - `active`: true
   - `created_at`: Current timestamp
3. Update `images` table:
   - Set `activation_status = 'active'`
   - Set `activated_at = NOW()`
4. Commit transaction
5. Populate Response with snapshot info

**Error Handling**:
- Database errors → standard error (retry, max 5 attempts)
- Unique constraint violation (concurrent activation) → verify snapshot exists, return success
- Transaction deadlock → standard error (retry with jitter)

**Retry Strategy**: Exponential backoff with jitter, max 5 retries

---

### Request/Response Types

```go
type ImageActivateRequest struct {
    ImageID      string
    DeviceID     string // From Unpack FSM
    DeviceName   string
    SnapshotName string // Optional, generated if not provided
    PoolName     string // Optional, defaults to configured pool
}

type ImageActivateResponse struct {
    ImageID      string
    SnapshotID   string
    SnapshotName string
    DevicePath   string
    Active       bool
    Activated    bool // true if activated, false if already existed
}
```

### FSM Registration

```go
fsm.Register[ImageActivateRequest, ImageActivateResponse](manager, "activate-image").
    Start("check-snapshot", checkSnapshot(deps)).
    To("create-snapshot", createSnapshot(deps)).
    To("register", registerSnapshot(deps)).
    End("complete").
    Build(ctx)
```

### Queue Configuration

- **No queue** (fast operation, no I/O bottleneck)
- Runs immediately when started

---

## FSM Chaining

### Sequential Execution

The three FSMs are chained together using `fsm.WithRunAfter`:

```go
// 1. Start Download FSM
downloadRunID, err := startDownload(ctx, imageID, &fsm.Request[...]{
    Msg: &ImageDownloadRequest{...},
}, fsm.WithQueue("downloads"))

// 2. Start Unpack FSM after Download completes
unpackRunID, err := startUnpack(ctx, imageID, &fsm.Request[...]{
    Msg: &ImageUnpackRequest{
        ImageID:   imageID,
        LocalPath: downloadResp.LocalPath,
        Checksum:  downloadResp.Checksum,
    },
}, fsm.WithQueue("unpacking"), fsm.WithRunAfter(downloadRunID))

// 3. Start Activate FSM after Unpack completes
activateRunID, err := startActivate(ctx, imageID, &fsm.Request[...]{
    Msg: &ImageActivateRequest{
        ImageID:    imageID,
        DeviceID:   unpackResp.DeviceID,
        DeviceName: unpackResp.DeviceName,
    },
}, fsm.WithRunAfter(unpackRunID))
```

### Parent-Child Relationships

Optionally, use `fsm.WithParent` to track the entire pipeline:

```go
// Create parent FSM for tracking
parentRunID := ulid.Make()

// All child FSMs reference the parent
fsm.WithParent(parentRunID)
```

---

## Error Handling Summary

### Error Types

| Error Type | Behavior | Use Cases |
|------------|----------|-----------|
| **Standard Error** | Auto-retry with exponential backoff | Network errors, transient I/O errors, database locks |
| **fsm.Abort** | Stop immediately, no retries, mark as aborted | Validation failures, security violations, resource exhaustion |
| **fsm.Unrecoverable** | Stop immediately, mark as failed | Permanent errors, data corruption |
| **fsm.Handoff** | Skip remaining transitions, mark as complete | Idempotency (already processed) |

### Retry Strategies

| Transition | Strategy | Max Retries | Timeout |
|------------|----------|-------------|---------|
| check-exists | Exponential backoff | 3 | - |
| download | Exponential backoff + jitter | 5 | 5 min |
| validate | Fixed retry | 2 | - |
| store-metadata | Exponential backoff + jitter | 5 | - |
| check-unpacked | Exponential backoff | 3 | - |
| create-device | Fixed retry | 3 | - |
| extract-layers | Fixed retry | 2 | 30 min |
| verify-layout | Fixed retry | 2 | - |
| update-db | Exponential backoff | 5 | - |
| check-snapshot | Exponential backoff | 3 | - |
| create-snapshot | Fixed retry | 3 | - |
| register | Exponential backoff + jitter | 5 | - |

### Cleanup on Failure

Each FSM implements cleanup logic:

**Download FSM**:
- Remove partial downloads from temporary location
- Delete temporary files

**Unpack FSM**:
- Unmount device if mounted
- Deactivate and delete devicemapper device
- Remove mount point directory
- Delete database entry if partially created

**Activate FSM**:
- Deactivate and delete snapshot device
- Delete database entry if partially created

---

## References

- [Durable State Contracts](DURABLE_STATE_CONTRACTS.md) - State-to-database mapping and crash recovery
- [System Architecture](SYSTEM_ARCH.md) - High-level architecture
- [Security Design](SECURITY.md) - Security strategy and validations
- [Database Schema](../api/DATABASE.md) - Data model and queries
- [API Interfaces](../api/INTERFACES.md) - Request/Response type details
- [Requirements](../spec/REQUIREMENTS.md) - Functional requirements

