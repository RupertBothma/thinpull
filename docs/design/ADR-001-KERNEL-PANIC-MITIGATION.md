# ADR-001: Kernel Panic Mitigation Strategy

**Status:** Accepted
**Date:** 2025-11-26 (Updated)
**Deciders:** Project Maintainers
**Related:** [Security Design](SECURITY.md), [System Architecture](SYSTEM_ARCH.md)

---

## Context

During development and testing of the container image management system, we encountered **kernel panics** triggered by devicemapper cleanup operations. The panics manifested as:

- **D-state hangs** - processes stuck in uninterruptible sleep
- **System freezes** - requiring hard reboot
- **Kernel oops messages** - related to dm-thin module

### Root Cause Analysis

Investigation revealed that kernel panics were triggered by cleanup operations (`umount`, `dmsetup remove`, `dmsetup message ... delete`) performed on thin devices **immediately after** failed operations like:

- `mkfs.ext4` timeout or failure
- Device activation failure
- Mount operation failure

The Linux dm-thin (device-mapper thin provisioning) stack becomes **unstable** when cleanup operations are attempted on devices that just experienced failures, particularly when:

1. The dm-thin pool is under stress (high I/O, near capacity)
2. Operations timeout but kernel resources are still held
3. Cleanup is attempted before kernel has released internal locks

### Business Impact

- **Development blocked** - frequent kernel panics during testing
- **Production risk** - potential for cascading failures
- **Operational burden** - manual recovery required after each panic
- **Data loss risk** - unclean shutdowns could corrupt databases

---

## Decision

We adopt a **"fail-dumb" strategy** for devicemapper operations:

### Core Principle

**NEVER automatically clean up devicemapper resources on error paths in production code.**

### Implementation

1. **Remove automatic cleanup from error paths**
   - `CreateThinDevice` no longer calls `DeactivateDevice` or `deleteThinDevice` on mkfs failure
   - All FSM transitions accept resource leakage rather than risk cleanup

2. **Implement separate garbage collection system**
   - Standalone `gc` command for safe cleanup when system is idle
   - Dry-run mode for safety
   - Force flag requirement for actual cleanup
   - Timeout protection on all operations (10s per operation)
   - Skip mounted devices

3. **Enhanced observability**
   - Comprehensive before/after logging for all devicemapper operations
   - Structured fields: pool_name, device_id, device_name, command, duration_ms, exit_code, timed_out
   - Clear error messages directing users to GC command

4. **Documentation and procedures**
   - Operations Guide with emergency recovery procedures
   - Troubleshooting Guide with orphaned device detection
   - Clear policy documentation in code comments

---

## Consequences

### Positive

✅ **Eliminates kernel panics** - No cleanup on error paths means no D-state hangs

✅ **System stability** - Fail-safe approach prevents cascading failures

✅ **Observability** - Comprehensive logging enables root cause analysis

✅ **Operational clarity** - Clear procedures for handling orphaned devices

✅ **Debuggability** - Timeout tracking helps identify hung operations

### Negative

⚠️ **Resource leakage** - Failed device creation leaves orphaned devices

⚠️ **Operational overhead** - Requires periodic GC or manual cleanup

⚠️ **Disk space** - Orphaned devices consume pool space until cleaned

⚠️ **Complexity** - Separate GC system adds code and operational procedures

### Mitigations

- **Automated GC** - Can be scheduled via cron for periodic cleanup
- **Monitoring** - Pool usage alerts prevent space exhaustion
- **FSM detection** - Unpack FSM detects orphaned devices and provides clear error messages
- **Documentation** - Comprehensive guides reduce operational burden

---

## Alternatives Considered

### Alternative 1: Retry with Exponential Backoff

**Approach:** Retry cleanup operations with increasing delays (1s, 2s, 4s, 8s, etc.)

**Rejected because:**
- Still triggers kernel panics, just less frequently
- Delays don't solve the fundamental dm-thin instability
- Adds complexity without eliminating the problem
- Can make panics worse by repeatedly stressing the kernel

### Alternative 2: Async Cleanup Queue

**Approach:** Queue cleanup operations for later execution by background worker

**Rejected because:**
- Still performs cleanup, just deferred
- Doesn't solve the dm-thin instability issue
- Adds significant complexity (queue management, worker lifecycle)
- Background worker could still trigger panics

### Alternative 3: Kernel Upgrade

**Approach:** Upgrade to newer kernel with dm-thin fixes

**Rejected because:**
- Not always feasible in production environments
- Dm-thin bugs exist across multiple kernel versions
- Doesn't address fundamental design issue (cleanup after failure)
- Would still need fallback strategy for older kernels

### Alternative 4: Use Different Storage Backend

**Approach:** Replace devicemapper with overlayfs, btrfs, or zfs

**Rejected because:**
- Out of scope for the current implementation
- Devicemapper thin provisioning is a requirement
- Would require complete redesign
- Doesn't address the general principle of cleanup-after-failure

---

## Implementation Details

### Code Changes

**File:** `devicemapper/dm.go`

- **Lines 61-82:** Added package-level "Cleanup Policy (CRITICAL)" documentation
- **Lines 299-314:** Removed automatic cleanup from `CreateThinDevice` mkfs error path
- **9 functions:** Added comprehensive before/after logging with structured fields
- **Cleanup functions:** Added WARNING documentation about kernel panic risks

**File:** `unpack/fsm.go`

- **Lines 248-278:** Added orphaned device detection after `CreateThinDevice` failure
- Enhanced error messages to direct users to GC command

**File:** `cmd/flyio-image-manager/gc.go` (NEW)

- **368 lines:** Complete GC command implementation
- Safety checks: dry-run, force flag, timeout protection, mounted device detection
- 3-step cleanup: unmount → deactivate → delete (each with 10s timeout)

**File:** `cmd/flyio-image-manager/main.go`

- Added GC command integration
- Added `parseGCFlags()` function
- Updated help text

### Testing Strategy

**Manual Testing:**
1. Trigger device creation failures (pool full, mkfs timeout)
2. Verify no kernel panics occur
3. Verify orphaned devices are detected
4. Run GC command in dry-run mode
5. Run GC command with --force
6. Verify devices are cleaned up successfully

**Monitoring:**
- Watch for "orphaned device" log messages
- Monitor pool usage metrics
- Track GC command execution and results

### Rollback Plan

If the fail-dumb strategy proves problematic:

1. **Immediate:** Revert to previous cleanup behavior (accept kernel panic risk)
2. **Short-term:** Implement Alternative 2 (async cleanup queue)
3. **Long-term:** Investigate Alternative 3 (kernel upgrade) or Alternative 4 (different storage backend)

---

## References

- [Fly.io Engineering Philosophy](../note/FLY_CONTEXT.md) - "Fail dumb, not smart"
- [Operations Guide](../guide/OPERATIONS.md) - Orphaned device management procedures
- [Troubleshooting Guide](../guide/TROUBLESHOOTING.md) - Emergency recovery procedures
- [Linux dm-thin documentation](https://www.kernel.org/doc/Documentation/device-mapper/thin-provisioning.txt)

---

## Lessons Learned

1. **Cleanup-after-failure is dangerous** - Attempting to clean up resources that just failed operations can trigger system instability

2. **Fail-safe over fail-smart** - Accepting resource leakage is better than risking kernel panics

3. **Observability is critical** - Comprehensive logging enabled root cause analysis and informed the decision

4. **Separate concerns** - Cleanup should be a separate, controlled operation, not automatic on error paths

5. **Document the "why"** - Clear policy documentation prevents future developers from reintroducing dangerous patterns

---

## Future Considerations

1. **Automated GC scheduling** - Integrate GC into daemon mode with configurable schedule

2. **Pool usage monitoring** - Add Prometheus metrics for orphaned device count and pool usage

3. **Kernel version detection** - Detect kernel version and adjust strategy if newer kernels have fixes

4. **Alternative storage backends** - Evaluate overlayfs or other backends for future versions

5. **Upstream kernel fixes** - Monitor dm-thin development and contribute bug reports/patches

---

## Priority 3 Enhancements (2025-11-26)

### Context

After Priority 1 (fail-dumb pattern) and Priority 2 (concurrency control) fixes, a kernel panic occurred during production testing. Post-mortem analysis revealed that the system needed proactive health checks to detect and prevent operations when the system is in an unhealthy state.

### Decision

Implement **comprehensive pre-flight health checks** before any devicemapper operations:

**Health Check 1: Pool Existence Verification**
- Verifies thin-pool exists before any operations
- Detects `needs_check` corruption flag
- Provides clear recovery instructions when pool is missing

**Health Check 2: D-state Process Detection (Enhanced)**
- Expanded pattern to catch more dm-related D-state processes:
  - `dm-thin` workers
  - `flush-*` threads
  - `jbd2/dm-*` journal threads
  - `kworker` threads with dm context

**Health Check 3: Kernel Log Scanning**
- Scans `dmesg` for recent devicemapper errors:
  - `device-mapper.*thin` errors
  - `needs_check` flags
  - I/O errors on dm devices
  - Pool/metadata errors

**Health Check 4: Memory Pressure Detection**
- Detects OOM conditions that can cause dm hangs:
  - Available memory < 5%
  - Swap usage > 80%

**Health Check 5: I/O Wait Detection**
- Detects storage bottlenecks:
  - I/O wait > 50% blocks operations

**Health Check 6: Timeout Protection**
- All health check commands have 10-second timeout
- Prevents health checks themselves from hanging

### Implementation Details

**File:** `cmd/flyio-image-manager/main.go`

- **`checkPoolExists()`** (lines 576-599): New function to verify pool exists
- **`checkDStateProcesses()`**: Enhanced pattern matching for D-state detection
- **`checkKernelLogs()`**: New function to scan dmesg for dm errors
- **`checkMemoryPressure()`**: New function to detect OOM conditions
- **`checkIOWait()`**: New function to detect storage bottlenecks
- **`runFSMPipeline()`**: Updated to call `checkPoolExists()` before processing

### Consequences

**Positive**:
- ✅ Detects missing pool before operations (clear error message)
- ✅ Prevents operations when system is unhealthy
- ✅ Provides actionable recovery instructions
- ✅ Timeout protection prevents health checks from hanging

**Negative**:
- ⚠️ Additional latency (~100ms) for health checks before each operation
- ⚠️ May block operations during transient system stress

### Recovery Procedure

When pool is missing after kernel panic:

```bash
cd /var/lib/flyio

# Clean up stale loop devices
sudo losetup -D

# Recreate backing files
fallocate -l 1M pool_meta
fallocate -l 2G pool_data

# Create loop devices
METADATA_DEV="$(losetup -f --show pool_meta)"
DATA_DEV="$(losetup -f --show pool_data)"

# Create thin pool
dmsetup create --verifyudev pool --table "0 4194304 thin-pool ${METADATA_DEV} ${DATA_DEV} 256 65536"

# Verify
dmsetup status pool
```

### References

- [Troubleshooting Guide - Pool Does Not Exist](../guide/TROUBLESHOOTING.md#issue-pool-does-not-exist)
- [Usage Guide - Health Checks & Safeguards](../guide/USAGE.md#health-checks--safeguards)

---

## Priority 2 Enhancements (2025-11-24)

### Context

Priority 1 fixes (fail-dumb pattern, GC command, comprehensive logging) successfully eliminated kernel panics from cleanup operations. However, testing revealed additional concurrency issues:

1. **Multi-process conflicts**: Multiple flyio-image-manager processes running simultaneously caused devicemapper pool hangs
2. **Same-image conflicts**: Multiple FSMs attempting to unpack the same image concurrently caused pool instability
3. **Intra-process races**: Concurrent devicemapper operations within a single process caused unpredictable behavior

### Decision

Implement **three-layer concurrency control** to prevent all forms of concurrent devicemapper operations:

**Layer 1: Process Lock File**
- Manager lock file at `<FSMDBPath>/flyio-manager.lock`
- Contains PID, timestamp, and command for diagnostics
- Acquired at startup by `process-image` and `daemon` commands
- GC command checks for lock file and requires `--ignore-lock` flag to override

**Layer 2: Per-Image Database Locking**
- New `image_locks` table with UNIQUE constraint on `image_id`
- Acquired at start of Unpack FSM (`checkUnpacked` transition)
- Released on Handoff (already unpacked), before `fsm.Abort` (errors), and after success (`updateDB`)
- Prevents multiple FSMs from unpacking the same image concurrently

**Layer 3: DeviceMapper Mutex Serialization**
- `sync.Mutex` in `devicemapper.Client` struct
- Wraps all state-mutating operations: `CreateThinDevice`, `CreateSnapshot`, `ActivateDevice`, `DeactivateDevice`, `DeleteDevice`
- Serializes devicemapper operations within a single process

**Additional Changes**:
- Reduced `UnpackQueueSize` from 2 to 1 to serialize devicemapper-heavy work
- Fixed GC delete command syntax: `fmt.Sprintf("delete %s", deviceID)`
- Created `DatabaseManager` interface for test mocking

### Implementation Details

**Files Modified**:

1. **`cmd/flyio-image-manager/main.go`** (lines 85-130):
   - Added `lockFileInfo` struct
   - Implemented `acquireManagerLock()` and `releaseManagerLock()`
   - Modified `runProcessImage()` and `runDaemon()` to acquire/release lock

2. **`cmd/flyio-image-manager/gc.go`** (lines 20-23, 30-32, 60-70):
   - Added `gcIgnoreLock` flag
   - Added lock file check before GC operations
   - Logs warning if `--ignore-lock` is used

3. **`database/schema.go`** (lines 85-95):
   - Added `imageLocksSchema` constant for version 2 migration
   - Created `image_locks` table with `image_id` PRIMARY KEY

4. **`database/database.go`** (lines 56, 290-330):
   - Added migration version 2
   - Implemented `AcquireImageLock()`, `ReleaseImageLock()`, `IsImageLocked()`

5. **`unpack/fsm.go`** (lines 21-32, 47, 155, 276-279, 298-301, 320-323, 404-407, 463-466, 586-592):
   - Created `DatabaseManager` interface
   - Updated `Dependencies.DB` to use interface
   - Added lock acquisition in `checkUnpacked()`
   - Added lock release before all `fsm.Abort` returns
   - Added lock release in `updateDB()` after success

6. **`unpack/fsm_test.go`** (lines 17-45):
   - Created `fakeDB` mock implementing `DatabaseManager` interface
   - Updated all tests to use `&fakeDB{}`

7. **`devicemapper/dm.go`** (lines 85-103, 168-176, etc.):
   - Added `sync.Mutex` field to `Client` struct
   - Wrapped all state-mutating operations with `mu.Lock()/Unlock()`

### Consequences

**Positive**:
- ✅ Eliminates multi-process devicemapper conflicts
- ✅ Prevents concurrent unpack of same image
- ✅ Serializes devicemapper operations within process
- ✅ Maintains backward compatibility (database migration)
- ✅ All tests pass (`go test ./...`)

**Negative**:
- ⚠️ Reduced concurrency: `UnpackQueueSize=1` (serializes unpack operations)
- ⚠️ Added complexity: Lock management in FSM transitions
- ⚠️ Lock cleanup required in all error paths

**Mitigations**:
- Idempotent lock operations (release doesn't fail if lock doesn't exist)
- Comprehensive lock cleanup in all FSM exit paths
- Clear error messages when lock conflicts occur
- `DatabaseManager` interface enables clean test mocking

### Alternatives Considered

**Alternative 1: Advisory Locks (flock)**
- **Approach**: Use `flock()` on lock file for atomic locking
- **Rejected**: Doesn't solve per-image locking, only process-level
- **Trade-off**: Simpler implementation but incomplete solution

**Alternative 2: Distributed Locks (Redis, etcd)**
- **Approach**: Use distributed lock service for coordination
- **Rejected**: Overkill for single-host system, adds external dependency
- **Trade-off**: Would enable multi-host deployment but unnecessary complexity

**Alternative 3: Queue-Based Serialization**
- **Approach**: Single-threaded queue for all devicemapper operations
- **Rejected**: Doesn't prevent multi-process conflicts, reduces concurrency too much
- **Trade-off**: Simpler than multi-layer approach but less flexible

**Alternative 4: No Concurrency Control**
- **Approach**: Accept kernel panics as rare edge case
- **Rejected**: Unacceptable production risk, violates fail-safe principle
- **Trade-off**: Simpler code but unstable system

### Testing

**Manual Testing**:
1. ✅ Run multiple `process-image` commands simultaneously (lock conflict detected)
2. ✅ Run same image twice concurrently (per-image lock prevents conflict)
3. ✅ Run GC while FSMs active (lock file check prevents GC)
4. ✅ Run GC with `--ignore-lock` (warning logged, GC proceeds)
5. ✅ All unit tests pass (`go test ./...`)

**Automated Testing**:
- `fakeDB` mock enables FSM tests without real database
- All existing tests continue to pass
- No new test files created (per project requirements)

### References

- [System Architecture - Concurrency Control](SYSTEM_ARCH.md#concurrency-control)
- [Database API - Image Lock Operations](../api/DATABASE.md#image-lock-operations)
- [Security Design - Lock File Security](SECURITY.md#layer-3-lock-file-security)
- [Kernel Panic Root Cause Analysis](../note/KERNEL_PANIC_ROOT_CAUSE.md)

