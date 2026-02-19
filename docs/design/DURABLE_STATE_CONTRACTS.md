# Durable State Contracts

**Document Type**: Design  
**Status**: Approved  
**Version**: 1.0  
**Last Updated**: 2025-11-21

---

## Overview

This document defines the **durable state contracts** for the Download, Unpack, and Activate FSMs. Each FSM state encodes a clear recovery story: what data must be persisted, what the system should do when resuming from that state after a crash, and how database fields align with FSM states.

**Purpose**: Enable precise crash recovery by documenting the relationship between FSM states, persisted data, and recovery actions.

**Key Principle**: `Database State + FSM History = Complete Progress Description`

---

## State Contract Table Format

For each FSM, we document:

| State | Persisted Data | SQLite Fields | Recovery Action | Notes |
|-------|----------------|---------------|-----------------|-------|
| State name | What must be saved | DB columns that track this state | What to do on resume | Additional context |

**Recovery Action Types**:
- **Skip**: Work already complete, use Handoff to skip remaining transitions
- **Retry**: Repeat the transition (idempotent operation)
- **Resume**: Continue from where we left off
- **Cleanup**: Remove partial work and start fresh
- **Validate**: Check consistency and decide next action

---

## Download FSM State Contracts

**FSM Name**: `download-image`  
**Transitions**: check-exists → download → validate → store-metadata → complete

### State Contract Table

| State | Persisted Data | SQLite Fields | Recovery Action | Notes |
|-------|----------------|---------------|-----------------|-------|
| **START** | Request: s3_key, image_id, bucket | None yet | Check if download needed | Initial state, no persistence |
| **check-exists** | Request in FSM history | `images.download_status = 'completed'`<br>`images.s3_key`<br>`images.local_path`<br>`images.checksum`<br>`images.size_bytes` | If DB row exists with status='completed' AND file exists at local_path AND size/checksum match → **Skip** (Handoff)<br>Otherwise → **Retry** download | Idempotent check, safe to repeat |
| **download** | Request + Response: local_path, checksum (partial), size_bytes | Temp file in filesystem | If resuming during download → **Cleanup** temp file, **Retry** download from beginning<br>No database record during download | Downloads to temp file, atomic move on success |
| **validate** | Response: local_path, checksum, size_bytes | File on disk | **Retry** validation<br>If validation fails → **Cleanup** file (delete), Abort FSM | Checksum verification, tar structure check, security validation |
| **store-metadata** | Response: all fields populated | `images.download_status = 'completed'`<br>`images.downloaded_at = NOW()` | If DB already has record with status='completed' → **Skip** (idempotent)<br>Otherwise → **Retry** DB insert | Upsert operation, safe to repeat |
| **COMPLETE** | Response: final ImageDownloadResponse | Persistent in images table | FSM done, can be garbage collected | Terminal state |

### SQLite Field Mapping

```sql
-- images.download_status values aligned with FSM states:
'pending'     -- Never downloaded, or download failed (pre-START)
'downloading' -- UNUSED (we don't track in-progress downloads)
'completed'   -- store-metadata transition completed successfully
'failed'      -- Validation failed (fsm.Abort)
```

### Recovery Scenarios

**Scenario 1: Crash during `download` transition**
- **FSM State**: In `download` transition
- **Database State**: No row, or row with `download_status != 'completed'`
- **Filesystem State**: May have temp file
- **Recovery Action**: 
  1. Resume FSM → re-enters `download` transition
  2. Temp file cleanup (if exists)
  3. Restart download from S3
  4. Continue to validation

**Scenario 2: Crash after `download`, before `validate`**
- **FSM State**: Completed `download`, entering `validate`
- **Database State**: No completed record yet
- **Filesystem State**: Downloaded file exists at local_path
- **Recovery Action**:
  1. Resume FSM → enters `validate` transition
  2. Validate existing file (checksum, tar structure, security)
  3. If valid → continue to `store-metadata`
  4. If invalid → cleanup file, Abort FSM

**Scenario 3: Crash during `store-metadata`**
- **FSM State**: In `store-metadata` transition
- **Database State**: May or may not have record
- **Filesystem State**: Valid file exists
- **Recovery Action**:
  1. Resume FSM → re-enters `store-metadata`
  2. Upsert database record (idempotent)
  3. Complete FSM

**Scenario 4: Idempotent re-request after completion**
- **FSM State**: Fresh FSM run, starts at `check-exists`
- **Database State**: `download_status = 'completed'`, file verified
- **Recovery Action**:
  1. `check-exists` finds existing record
  2. Verifies file exists and checksum matches
  3. Returns Handoff with existing response
  4. Skips download, validate, store-metadata transitions

---

## Unpack FSM State Contracts

**FSM Name**: `unpack-image`  
**Transitions**: check-unpacked → create-device → extract-layers → verify-layout → update-db → complete

### State Contract Table

| State | Persisted Data | SQLite Fields | Recovery Action | Notes |
|-------|----------------|---------------|-----------------|-------|
| **START** | Request: image_id, local_path, checksum, pool_name | None yet | Check if already unpacked | Initial state |
| **check-unpacked** | Request in FSM history | `unpacked_images.image_id`<br>`unpacked_images.device_id`<br>`unpacked_images.device_name`<br>`unpacked_images.layout_verified = true` | Query DB for unpacked_images record AND verify device exists in devicemapper<br>If both exist → **Skip** (Handoff)<br>If DB exists but no device → **Cleanup** stale DB row, **Retry** unpack<br>Otherwise → **Retry** create-device | Validates both DB and devicemapper consistency |
| **create-device** | Response: device_id, device_name, device_path | Devicemapper thin device created<br>Device formatted with ext4<br>Device mounted at temp mount point | If device already exists → **Skip** to extract-layers<br>If partially created → **Cleanup** (deactivate + delete), **Retry** create<br>On failure → **Cleanup** device | Deterministic device_id = hash(image_id) |
| **extract-layers** | Response: device info + partial extraction | Files being written to mounted device<br>Partial tar extraction in progress | **Cannot resume partial extraction**<br>Must **Cleanup** (unmount, deactivate, delete device) and **Retry** from create-device | 30-minute timeout, extraction is atomic operation |
| **verify-layout** | Response: device info + extraction complete | All files extracted to device<br>Device still mounted | **Retry** layout verification<br>If layout invalid → **Cleanup** device (unmount, deactivate, delete), Abort FSM | Validates rootfs/, etc/, usr/, var/ structure |
| **update-db** | Response: all fields + layout_verified | `unpacked_images.layout_verified = true`<br>`unpacked_images.unpacked_at = NOW()` | If DB already has record → **Skip** (idempotent)<br>Otherwise → **Retry** DB insert<br>Device left mounted for next FSM | Device unmounted after DB update |
| **COMPLETE** | Response: final ImageUnpackResponse | Persistent in unpacked_images table<br>Device active and available | FSM done, device ready for activation | Terminal state |

### SQLite Field Mapping

```sql
-- unpacked_images table tracks unpack state:
layout_verified  -- Boolean: verify-layout completed successfully
                 -- NULL/FALSE = unpacking in progress or failed
                 -- TRUE = unpack complete, ready for activation

-- No status enum needed; presence of row + layout_verified indicates state
```

### Deterministic Device Identity

```go
// Stable device ID derived from image_id
deviceID = hash(image_id)[0:8]  // First 8 chars of hash
deviceName = "thin-" + deviceID  // Matches devicemapper naming convention

// This ensures:
// - Same image_id always gets same device_id
// - Idempotency: can check if device already exists
// - No collisions (hash-based)
```

### Recovery Scenarios

**Scenario 1: Crash during `create-device` transition**
- **FSM State**: In `create-device`, device may be partially created
- **Database State**: No unpacked_images row
- **Devicemapper State**: Device may exist but not fully initialized
- **Recovery Action**:
  1. Resume FSM → re-enters `create-device`
  2. Check if device exists using deterministic device_id
  3. If exists but not properly initialized → cleanup (deactivate + delete)
  4. Create device from scratch
  5. Continue to `extract-layers`

**Scenario 2: Crash during `extract-layers` transition**
- **FSM State**: In `extract-layers`, extraction partially complete
- **Database State**: No unpacked_images row (or layout_verified = false)
- **Devicemapper State**: Device exists, mounted, partial files extracted
- **Recovery Action**:
  1. Resume FSM → re-enters `extract-layers`
  2. **Cannot resume partial extraction** (tar is sequential)
  3. Cleanup: unmount device, deactivate, delete device
  4. Go back to `create-device` transition
  5. Re-create device and restart extraction from beginning

**Scenario 3: Crash after `extract-layers`, before `verify-layout`**
- **FSM State**: Completed extraction, entering verification
- **Database State**: No unpacked_images row yet (or layout_verified = false)
- **Devicemapper State**: Device exists, mounted, all files extracted
- **Recovery Action**:
  1. Resume FSM → enters `verify-layout`
  2. Verify rootfs structure on existing device
  3. If valid → continue to `update-db`
  4. If invalid → cleanup device, Abort FSM

**Scenario 4: Crash during `update-db`**
- **FSM State**: In `update-db`, layout verified
- **Database State**: May or may not have record
- **Devicemapper State**: Device exists with verified layout
- **Recovery Action**:
  1. Resume FSM → re-enters `update-db`
  2. Upsert database record (idempotent)
  3. Unmount device
  4. Complete FSM

**Scenario 5: Idempotent re-request after completion**
- **FSM State**: Fresh FSM run, starts at `check-unpacked`
- **Database State**: unpacked_images row exists with layout_verified = true
- **Devicemapper State**: Device exists and active
- **Recovery Action**:
  1. `check-unpacked` queries DB and verifies device exists
  2. Returns Handoff with existing device info
  3. Skips all unpacking work

---

## Activate FSM State Contracts

**FSM Name**: `activate-image`  
**Transitions**: check-snapshot → create-snapshot → register → complete

### State Contract Table

| State | Persisted Data | SQLite Fields | Recovery Action | Notes |
|-------|----------------|---------------|-----------------|-------|
| **START** | Request: image_id, device_id, device_name, snapshot_name | None yet | Check if already activated | Initial state |
| **check-snapshot** | Request in FSM history | `snapshots.image_id`<br>`snapshots.snapshot_id`<br>`snapshots.snapshot_name`<br>`snapshots.active = true`<br>`images.activation_status = 'active'` | Query DB for snapshots record AND verify snapshot device exists in devicemapper<br>If both exist → **Skip** (Handoff)<br>If DB exists but no device → **Cleanup** stale DB row, **Retry** activation<br>Otherwise → **Retry** create-snapshot | Fast operation (<1s), validates both DB and devicemapper |
| **create-snapshot** | Response: snapshot_id, snapshot_name, device_path | Devicemapper snapshot device created<br>Copy-on-write snapshot from origin device | If snapshot already exists → **Skip** to register<br>If partially created → typically atomic in devicemapper<br>On failure (pool full, origin missing) → Abort FSM | Deterministic snapshot_id = origin_device_id + "-snap" |
| **register** | Response: all snapshot info | `snapshots.active = true`<br>`snapshots.created_at = NOW()`<br>`images.activation_status = 'active'`<br>`images.activated_at = NOW()` | If DB already has records → **Skip** (idempotent)<br>Otherwise → **Retry** DB inserts<br>Updates both snapshots and images tables | Two-table update, transactional |
| **COMPLETE** | Response: final ImageActivateResponse | Persistent in snapshots table<br>images.activation_status updated | FSM done, snapshot ready for use | Terminal state |

### SQLite Field Mapping

```sql
-- snapshots table tracks activation state:
active           -- Boolean: snapshot is active and available
                 -- TRUE = registered and ready
                 -- FALSE = deactivated or failed

-- images.activation_status aligned with FSM:
'inactive'       -- Never activated (pre-START)
'active'         -- register transition completed successfully
'failed'         -- Activation failed (pool full, etc.)
```

### Deterministic Snapshot Identity

```go
// Stable snapshot ID derived from origin device
snapshotID = origin_device_id + "-snap"  // e.g., "12345-snap"
snapshotName = "snap-" + image_id        // Human-readable name

// Devicemapper creates device named: "thin-{snapshotID}"
// e.g., "thin-12345-snap"

// This ensures:
// - Same origin device always gets same snapshot ID
// - Idempotency: can check if snapshot already exists
// - Clear relationship between origin and snapshot
```

### Recovery Scenarios

**Scenario 1: Crash during `create-snapshot` transition**
- **FSM State**: In `create-snapshot`, snapshot being created
- **Database State**: No snapshots row
- **Devicemapper State**: Snapshot may be partially created (rare)
- **Recovery Action**:
  1. Resume FSM → re-enters `create-snapshot`
  2. Check if snapshot already exists using deterministic snapshot_id
  3. If exists → skip to `register`
  4. If not exists → create snapshot
  5. Continue to `register`
- **Note**: Devicemapper snapshot creation is typically atomic

**Scenario 2: Crash during `register` transition**
- **FSM State**: In `register`, snapshot created
- **Database State**: May have partial records (snapshots row but not images update)
- **Devicemapper State**: Snapshot exists and active
- **Recovery Action**:
  1. Resume FSM → re-enters `register`
  2. Upsert snapshots record (idempotent)
  3. Update images.activation_status (idempotent)
  4. Complete FSM

**Scenario 3: Idempotent re-request after completion**
- **FSM State**: Fresh FSM run, starts at `check-snapshot`
- **Database State**: snapshots row exists with active = true, images.activation_status = 'active'
- **Devicemapper State**: Snapshot device exists
- **Recovery Action**:
  1. `check-snapshot` queries DB and verifies snapshot device exists
  2. Returns Handoff with existing snapshot info
  3. Skips snapshot creation and registration

**Scenario 4: Snapshot device missing but DB record exists**
- **FSM State**: Fresh FSM run at `check-snapshot`
- **Database State**: snapshots row exists with active = true
- **Devicemapper State**: Snapshot device does NOT exist (inconsistency)
- **Recovery Action**:
  1. `check-snapshot` detects inconsistency
  2. Cleanup: deactivate DB record (set active = false)
  3. Return nil to continue FSM
  4. Re-create snapshot from origin device
  5. Re-register in database

---

## Cross-FSM State Alignment

### Database State Consistency Rules

```sql
-- Rule 1: Downloaded images must exist before unpacking
SELECT * FROM images WHERE download_status = 'completed'
-- These images can be unpacked

-- Rule 2: Unpacked images must be downloaded first
SELECT * FROM unpacked_images u
JOIN images i ON u.image_id = i.image_id
WHERE i.download_status = 'completed'
-- All unpacked_images should have corresponding downloaded image

-- Rule 3: Activated images must be unpacked first  
SELECT * FROM snapshots s
JOIN unpacked_images u ON s.image_id = u.image_id
WHERE u.layout_verified = true
-- All snapshots should have corresponding unpacked image

-- Rule 4: Activation status tracks snapshot state
UPDATE images SET activation_status = 'active'
WHERE image_id IN (
    SELECT image_id FROM snapshots WHERE active = true
)
```

### FSM Chaining Contract

```
Download FSM → Unpack FSM → Activate FSM

Output of Download (ImageDownloadResponse) provides:
- image_id → Required by Unpack
- local_path → Required by Unpack
- checksum → Used for verification in Unpack

Output of Unpack (ImageUnpackResponse) provides:
- device_id → Required by Activate (origin device)
- device_name → Required by Activate
- image_id → Links all FSMs together

Output of Activate (ImageActivateResponse) provides:
- snapshot_id → Final device to mount/use
- device_path → Path to snapshot device
```

---

## Recovery Testing Scenarios

### Test Case 1: Crash During Download
```bash
# Setup: Start download, kill process mid-download
1. Start FSM: process-image --s3-key "test.tar"
2. Kill daemon: kill -9 <pid> (while download in progress)
3. Restart daemon: process-image --s3-key "test.tar"

# Expected Behavior:
- FSM resumes from START
- check-exists: finds no completed download in DB
- download: restarts from beginning (temp file cleaned up)
- Completes successfully
```

### Test Case 2: Crash After Download, Before Validation
```bash
# Setup: Download completes, crash before validation
1. Start FSM with instrumentation to pause before validate
2. Kill daemon after download transition completes
3. Restart daemon

# Expected Behavior:
- FSM resumes, enters validate transition
- Validates existing downloaded file
- If valid: continues to store-metadata
- If corrupted: deletes file and aborts
```

### Test Case 3: Crash During Extraction
```bash
# Setup: Extraction in progress, device partially populated
1. Start unpack FSM
2. Kill daemon while extract-layers is running
3. Restart daemon

# Expected Behavior:
- FSM resumes, re-enters extract-layers
- Detects partial extraction
- Cleanup: unmount, deactivate, delete device
- Returns to create-device
- Re-creates clean device
- Restarts extraction from beginning
```

### Test Case 4: Idempotency - Re-run Completed Pipeline
```bash
# Setup: Complete pipeline, then re-run
1. Run: process-image --s3-key "test.tar" (completes successfully)
2. Run: process-image --s3-key "test.tar" (same image)

# Expected Behavior:
- Download FSM: check-exists returns Handoff (skips all work)
- Unpack FSM: check-unpacked returns Handoff (skips all work)
- Activate FSM: check-snapshot returns Handoff (skips all work)
- Total time: <1 second (only DB queries)
- No S3 download, no tar extraction, no devicemapper operations
```

### Test Case 5: Database Inconsistency Recovery
```bash
# Setup: Manual DB corruption - record exists but device missing
1. Run complete pipeline (creates DB records and devices)
2. Manually delete devicemapper device: dmsetup remove thin-12345
3. Re-run: process-image --s3-key "test.tar"

# Expected Behavior:
- Unpack FSM check-unpacked: detects DB record but no device
- Cleanup: deletes stale DB row
- Continues with fresh unpack
- Re-creates device from scratch
- System self-heals from inconsistency
```

---

## Implementation Verification Checklist

### Download FSM
- [x] check-exists queries DB and verifies file existence/integrity
- [x] download uses temp file with atomic move on success
- [x] validate removes corrupted files on failure
- [x] store-metadata is idempotent (upsert)
- [x] Handoff used when image already downloaded
- [x] Cleanup removes temp files on failure

### Unpack FSM
- [x] check-unpacked validates both DB and devicemapper state
- [x] create-device uses deterministic device_id from image_id
- [x] extract-layers cannot resume partial work (cleanup and retry)
- [x] verify-layout validates filesystem structure
- [x] update-db records layout_verified = true
- [x] Cleanup unmounts, deactivates, deletes device on failure
- [x] Handoff used when image already unpacked

### Activate FSM
- [x] check-snapshot validates both DB and devicemapper state
- [x] create-snapshot uses deterministic snapshot_id from device_id
- [x] register updates both snapshots and images tables
- [x] Cleanup deactivates stale DB records when device missing
- [x] Handoff used when snapshot already activated
- [x] activation_status field tracks snapshot state

### Cross-FSM Integration
- [x] FSM outputs correctly chain to next FSM inputs
- [x] Database foreign key relationships enforce consistency
- [x] Deterministic IDs enable idempotency across all FSMs
- [x] Resume() called for all FSMs on daemon startup
- [x] Graceful shutdown persists FSM state before exit

---

## References

- [System Architecture](SYSTEM_ARCH.md) - High-level FSM orchestration
- [FSM Flow Design](FSM_FLOWS.md) - Detailed transition logic
- [Database Schema](../api/DATABASE.md) - SQLite schema and indexes
- [FSM Library API](../api/FSM_LIBRARY.md) - FSM framework reference

---

## Glossary

- **Durable State**: State that survives process crashes (persisted in BoltDB or SQLite)
- **Handoff**: FSM library mechanism to skip remaining transitions (fsm.Handoff)
- **Idempotency**: Ability to retry operations safely without side effects
- **Recovery Action**: What the system does when resuming from a specific state after crash
- **State Contract**: Agreement between FSM state, persisted data, and recovery behavior
- **Deterministic ID**: ID derived from input data that is always the same for same input
