# Database Write Persistence Diagnostic Implementation

**Document Type**: Implementation Note  
**Status**: Complete  
**Date**: 2025-11-22

---

## Problem Statement

After running the `process-image` pipeline successfully (Download → Unpack → Activate FSMs all complete), the SQLite database at `/var/lib/flyio/images.db` appears empty when queried, despite logs showing successful completion of all phases.

## Root Cause Hypotheses

1. **Wrong DB file being inspected**: Process writes to one path, user queries a different path
2. **DB file deleted/recreated after run**: Cleanup script removes DB after successful pipeline run
3. **FSM transitions didn't execute**: DB-writing transitions skipped due to early handoff logic

## Diagnostic Instrumentation Added

### 1. Database Path Tracking

**File**: `database/database.go`

Added `path` field to `DB` struct to track which database file is being used:

```go
type DB struct {
    db   *sql.DB
    path string // Path to the database file (for diagnostic logging)
}
```

Added `Path()` method for external visibility:

```go
func (d *DB) Path() string {
    return d.path
}
```

Updated `New()` to populate the path field:

```go
d := &DB{
    db:   db,
    path: cfg.Path,
}
```

### 2. DB Write Logging

Added diagnostic logging to all four critical DB write methods:

#### `StoreImageMetadata` (database/images.go)

```go
res, err := d.db.ExecContext(ctx, query, imageID, s3Key, localPath, checksum, sizeBytes, DownloadStatusCompleted, time.Now())
if err != nil {
    return fmt.Errorf("failed to store image metadata: %w", err)
}

// Diagnostic logging to track DB writes
rows, _ := res.RowsAffected()
log.Printf("[DB-WRITE] StoreImageMetadata: rows=%d, s3_key=%s, image_id=%s, path=%s, db_file=%s",
    rows, s3Key, imageID, localPath, d.path)
```

#### `StoreUnpackedImage` (database/unpacked.go)

```go
res, err := d.db.ExecContext(ctx, query, imageID, deviceID, deviceName, devicePath, sizeBytes, fileCount, time.Now())
if err != nil {
    return fmt.Errorf("failed to store unpacked image: %w", err)
}

// Diagnostic logging to track DB writes
rows, _ := res.RowsAffected()
log.Printf("[DB-WRITE] StoreUnpackedImage: rows=%d, image_id=%s, device=%s, device_path=%s, db_file=%s",
    rows, imageID, deviceName, devicePath, d.path)
```

#### `StoreSnapshot` (database/snapshots.go)

```go
res, err := d.db.ExecContext(ctx, query, imageID, snapshotID, snapshotName, devicePath, originDeviceID, time.Now())
if err != nil {
    return fmt.Errorf("failed to store snapshot: %w", err)
}

// Diagnostic logging to track DB writes
rows, _ := res.RowsAffected()
log.Printf("[DB-WRITE] StoreSnapshot: rows=%d, snapshot=%s, image_id=%s, device_path=%s, db_file=%s",
    rows, snapshotName, imageID, devicePath, d.path)
```

#### `UpdateImageActivationStatus` (database/images.go)

```go
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
```

### 3. Diagnostic Test Script

**File**: `test-scripts/diagnose-db-persistence.sh`

Created automated diagnostic script that:

1. Clears any existing database (`rm -f /var/lib/flyio/images.db`)
2. Runs a single image through the pipeline with debug logging
3. Immediately checks database state (before any cleanup)
4. Extracts `[DB-WRITE]` log entries to verify writes occurred
5. Compares log entries with actual database record counts
6. Provides analysis and recommendations

## Usage Instructions

### Rebuild the Application

```bash
go build -o flyio-image-manager .
```

### Run Diagnostic Test

```bash
sudo ./test-scripts/diagnose-db-persistence.sh
```

### Expected Output

The script will show:

1. **Database file status**: Confirms file exists and shows size
2. **Record counts**: Shows counts from `images`, `unpacked_images`, `snapshots` tables
3. **DB-WRITE log entries**: Shows all database writes that occurred with:
   - Rows affected
   - Key identifiers (s3_key, image_id, snapshot name)
   - Database file path being written to
4. **Analysis**: Compares logs vs actual records and diagnoses the issue

### Interpreting Results

**SUCCESS Case**:
```
✓ Database file exists: /var/lib/flyio/images.db
Database record counts:
  Images:          1
  Unpacked Images: 1
  Snapshots:       1

✓ Found DB-WRITE log entries
DB-WRITE log entries:
  [DB-WRITE] StoreImageMetadata: rows=1, s3_key=images/python/5.tar, image_id=..., db_file=/var/lib/flyio/images.db
  [DB-WRITE] StoreUnpackedImage: rows=1, image_id=..., device=thin-12345, db_file=/var/lib/flyio/images.db
  [DB-WRITE] StoreSnapshot: rows=1, snapshot=snapshot-..., image_id=..., db_file=/var/lib/flyio/images.db
  [DB-WRITE] UpdateImageActivationStatus: rows=1, image_id=..., status=active, db_file=/var/lib/flyio/images.db

✓ SUCCESS: All expected records found in database
```

**Path Mismatch Case**:
```
⚠ DB-WRITE logs found but records missing from database
  Possible causes:
  - Database file path mismatch (check db_file= in DB-WRITE logs)
```
→ Check if `db_file=` in logs differs from the path you're querying

**Early Handoff Case**:
```
⚠ No DB-WRITE logs and no records in database
  Possible causes:
  - FSM transitions skipped due to early handoff
```
→ Check for "already downloaded/unpacked/activated" messages in logs

## Next Steps

After running the diagnostic script, share:

1. The output of `grep "DB-WRITE" /tmp/flyio-test/db-persistence-debug.log`
2. The database record counts shown by the script
3. Any error messages or warnings from the analysis section

This will definitively show whether writes are happening, to which file, and whether the DB is being modified after the fact.

## Files Modified

- `database/database.go`: Added `path` field and `Path()` method
- `database/images.go`: Added logging to `StoreImageMetadata` and `UpdateImageActivationStatus`
- `database/unpacked.go`: Added logging to `StoreUnpackedImage`
- `database/snapshots.go`: Added logging to `StoreSnapshot`
- `test-scripts/diagnose-db-persistence.sh`: New diagnostic test script

## References

- [Database API](../api/DATABASE.md) - Database schema and operations
- [System Architecture](../design/SYSTEM_ARCH.md) - Database role in system
- [FSM Flow Design](../design/FSM_FLOWS.md) - Database usage in FSMs

