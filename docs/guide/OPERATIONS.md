# Operations Guide

**Last Updated**: 2025-11-26

This guide provides operational procedures for managing the Fly.io image management system in production.

## Table of Contents

- [System Safeguards](#system-safeguards)
- [Orphaned Device Management](#orphaned-device-management)
- [Pool Recovery After Kernel Panic](#pool-recovery-after-kernel-panic)
- [Emergency Recovery](#emergency-recovery)
- [Monitoring and Diagnostics](#monitoring-and-diagnostics)
- [Preventive Maintenance](#preventive-maintenance)

---

## System Safeguards

The system includes multiple safeguard layers to prevent kernel panics and ensure stability when working with devicemapper thin-pools.

### Safeguard Components

| Component | Location | Purpose |
|-----------|----------|---------|
| **Pool Manager** | `devicemapper/pool.go` | Auto-creates pool, validates health, manages lifecycle |
| **Health Checker** | `safeguards/safeguards.go` | D-state detection, kernel log scanning, memory checks |
| **Operation Guard** | `safeguards/safeguards.go` | Serializes dm operations, pre-op health checks |
| **Manager Lock** | `cmd/.../main.go` | Prevents concurrent processes |

### Pool Manager

The Pool Manager handles thin-pool lifecycle:

```go
// Auto-creates pool if missing (called at startup)
poolManager.EnsurePoolExists(ctx)

// Validates pool is healthy before operations
poolManager.ValidatePoolHealth(ctx)

// Gets detailed pool status
status, _ := poolManager.GetPoolStatus(ctx)
// status.Exists, status.NeedsCheck, status.ReadOnly, status.ErrorState
```

**Key behaviors**:
- Automatically creates pool if missing (after reboot/panic)
- Detects corrupted pool states (`needs_check`, read-only)
- Creates pool files in database directory (`/var/lib/flyio/`)
- Default pool size: 2GB data + 1MB metadata

### Health Checker

The Health Checker validates system state before operations:

```go
healthChecker := safeguards.NewSystemHealthChecker(poolName, logger)
err := healthChecker.CheckAll(ctx)
```

**Checks performed**:
1. **D-state Detection** - Blocks if dm-thin processes are in uninterruptible sleep
2. **Pool Status** - Validates pool exists and is healthy
3. **Kernel Logs** - Scans dmesg for BUG, panic, OOM errors
4. **Memory Pressure** - Blocks if available memory < 5%
5. **I/O Wait** - Blocks if I/O wait > 50%

### Operation Guard

The Operation Guard serializes devicemapper operations:

```go
guard := safeguards.NewOperationGuard(safeguards.GuardConfig{
    MaxConcurrent:   1,           // Only ONE dm operation at a time
    Logger:          log,
    HealthCheckFunc: healthChecker.CheckAll,
})

// Wrap dm-heavy operations
err := guard.WithOperation(ctx, "unpack-image", func() error {
    return performUnpack()
})
```

**Key behaviors**:
- Semaphore-based concurrency control (default: 1)
- Runs health check before acquiring lock
- Times out if lock not acquired within timeout
- Logs operation start/end for debugging

### Manager Lock

Process-level lock prevents multiple flyio-image-manager instances:

```bash
# Lock file location
/var/lib/flyio/fsm/flyio-manager.lock

# Contains:
{
  "pid": 12345,
  "timestamp": 1732623600,
  "command": "process-image"
}
```

**Key behaviors**:
- Atomic lock acquisition (O_EXCL flag)
- Detects stale locks from dead processes
- Auto-removes stale locks older than 5 minutes
- Provides clear error message if another process is running

### Recommended Operational Practices

1. **After Reboot**: Run `setup-pool` or let first `process-image` auto-create pool
2. **After Kernel Panic**: Check kernel logs, run `setup-pool` to recreate pool
3. **Before Heavy Load**: Verify pool health with `dmsetup status pool`
4. **Monitoring**: Watch for D-state processes with `ps aux | awk '$8 ~ /D/'`
5. **Concurrent Access**: Never run multiple `flyio-image-manager` processes

---

## Orphaned Device Management

### What are Orphaned Devices?

**Orphaned devices** are devicemapper thin devices that were partially created but never completed due to failures during the device creation process. Common causes include:

- **mkfs.ext4 timeout** - Filesystem creation took too long or hung
- **Activation failure** - Device activation failed after creation
- **System stress** - High I/O load, memory pressure, or CPU contention
- **Kernel issues** - dm-thin stack bugs or D-state hangs

Orphaned devices consume space in the devicemapper thin pool but have no corresponding database records, making them invisible to the application.

### Why Don't We Clean Up Automatically?

**CRITICAL SAFETY DECISION**: The system intentionally does NOT automatically clean up failed devices because:

1. **Kernel Panic Risk**: Cleanup operations (unmount, deactivate, delete) on devices that just failed can trigger kernel-level D-state hangs and kernel panics
2. **System Stability**: It's safer to leak resources than to crash the entire system
3. **Controlled Cleanup**: Manual cleanup when the system is idle is much safer

This is documented in the code as the **"fail-dumb" pattern** - we accept resource leakage to prevent system instability.

### Detecting Orphaned Devices

#### Symptoms

- FSM aborts with error: `"orphaned device detected; run 'flyio-image-manager gc' to clean up"`
- `dmsetup ls` shows `thin-*` devices that don't appear in the database
- Pool usage increases without corresponding database records
- Repeated device creation failures

#### Verification

Check for orphaned devices manually:

```bash
# List all thin devices in devicemapper
dmsetup ls | grep thin-

# List all devices in database
sqlite3 /var/lib/flyio/images.db "SELECT device_name FROM unpacked_images;"

# Compare the two lists - devices in devicemapper but not in database are orphaned
```

### Garbage Collection Procedure

#### Step 1: Preview Orphaned Devices (Dry Run)

**ALWAYS run a dry run first** to see what would be cleaned:

```bash
flyio-image-manager gc --dry-run
```

This will:
- List all thin devices in devicemapper
- Compare with database records
- Identify orphaned devices
- Show what would be cleaned (without actually cleaning)

Example output:
```
INFO[0000] Running in DRY RUN mode - no changes will be made
INFO[0000] Step 1: Querying devicemapper for thin devices
INFO[0001] Found thin devices in devicemapper           count=5
INFO[0001] Step 2: Querying database for device records
INFO[0001] Found device records in database              count=3
INFO[0001] Step 3: Identifying orphaned devices
WARN[0001] Found orphaned device                         device_id=abc123 device_name=thin-abc123 mounted=false
WARN[0001] Found orphaned device                         device_id=def456 device_name=thin-def456 mounted=false
INFO[0001] DRY RUN: Skipping cleanup
INFO[0001] === Garbage Collection Summary ===
INFO[0001] Summary                                       cleaned=0 failed=0 orphaned=2 skipped=0 total_devices=5
INFO[0001] DRY RUN complete - no changes were made
INFO[0001] Run with --force to actually clean up orphaned devices
```

#### Step 2: Stop Active Operations

**CRITICAL**: Ensure no FSMs are currently running:

```bash
# If running in daemon mode, stop the daemon
pkill -SIGTERM flyio-image-manager

# Or use the shutdown command if available
flyio-image-manager shutdown

# Verify no processes are running
ps aux | grep flyio-image-manager
```

#### Step 3: Verify System is Idle

Check that the system is not under heavy load:

```bash
# Check CPU and I/O load
top
iostat -x 1 5

# Check for D-state processes (hung processes)
ps aux | awk '$8 ~ /D/ {print}'

# If you see D-state processes, DO NOT proceed - reboot first
```

#### Step 4: Run Garbage Collection

Once you've verified the system is idle and no FSMs are running:

```bash
# Clean up orphaned devices
flyio-image-manager gc --force

# For verbose logging (recommended)
flyio-image-manager gc --force --verbose
```

Example output:
```
WARN[0000] Running in FORCE mode - orphaned devices will be deleted
WARN[0000] IMPORTANT: Ensure no FSMs are currently running before proceeding
WARN[0000] IMPORTANT: This command should only be run when the system is idle
INFO[0000] Step 1: Querying devicemapper for thin devices
INFO[0001] Found thin devices in devicemapper           count=5
INFO[0001] Step 2: Querying database for device records
INFO[0001] Found device records in database              count=3
INFO[0001] Step 3: Identifying orphaned devices
WARN[0001] Found orphaned device                         device_id=abc123 device_name=thin-abc123 mounted=false
WARN[0001] Found orphaned device                         device_id=def456 device_name=thin-def456 mounted=false
INFO[0001] Step 4: Cleaning up orphaned devices
INFO[0001] Attempting to clean up orphaned device        device_id=abc123 device_name=thin-abc123
DEBUG[0001] Step 1: Attempting unmount                    device_id=abc123 device_name=thin-abc123
DEBUG[0002] Step 2: Attempting deactivate                 device_id=abc123 device_name=thin-abc123
DEBUG[0003] Step 3: Attempting delete from thin pool      device_id=abc123 device_name=thin-abc123
INFO[0004] Successfully cleaned up orphaned device       device_id=abc123 device_name=thin-abc123
INFO[0004] Attempting to clean up orphaned device        device_id=def456 device_name=thin-def456
DEBUG[0004] Step 1: Attempting unmount                    device_id=def456 device_name=thin-def456
DEBUG[0005] Step 2: Attempting deactivate                 device_id=def456 device_name=thin-def456
DEBUG[0006] Step 3: Attempting delete from thin pool      device_id=def456 device_name=thin-def456
INFO[0007] Successfully cleaned up orphaned device       device_id=def456 device_name=thin-def456
INFO[0007] === Garbage Collection Summary ===
INFO[0007] Summary                                       cleaned=2 failed=0 orphaned=2 skipped=0 total_devices=5
INFO[0007] Garbage collection complete
```

#### Step 5: Verify Cleanup

After cleanup, verify that orphaned devices were removed:

```bash
# Check devicemapper devices
dmsetup ls | grep thin-

# Check database records
sqlite3 /var/lib/flyio/images.db "SELECT device_name FROM unpacked_images;"

# Check pool status
dmsetup status pool
```

### Handling Cleanup Failures

If the GC command reports failures:

```
WARN[0010] Some devices could not be cleaned - manual intervention may be required
WARN[0010] Consider rebooting the system if devices are stuck in D-state
```

**Possible causes:**
- Device is stuck in D-state (uninterruptible sleep)
- Device is in use by another process
- Kernel dm-thin stack is hung

**Resolution:**
1. **Check for D-state processes**: `ps aux | awk '$8 ~ /D/ {print}'`
2. **If D-state processes exist**: System reboot is the only safe option
3. **After reboot**: Run GC again to clean up remaining orphaned devices

---

## Pool Recovery After Kernel Panic

When a kernel panic or system reboot occurs, the devicemapper thin-pool is lost because it uses loop devices backed by files. The system automatically detects this and can recover.

### Automatic Recovery

The system now **automatically recreates the pool** when it detects the pool is missing:

```bash
# Simply run process-image - pool will be auto-created
sudo ./flyio-image-manager process-image --s3-key images/test.tar

# Output shows automatic pool creation:
{"level":"info","msg":"safeguards initialized"}
{"component":"pool-manager","msg":"pool does not exist, attempting to create"}
{"component":"pool-manager","msg":"creating new thin pool","pool_name":"pool"}
{"component":"pool-manager","msg":"thin pool created successfully"}
# ... continues with image processing
```

### Manual Recovery (Using setup-pool)

If you prefer to explicitly recreate the pool:

```bash
# Use the setup-pool command
sudo ./flyio-image-manager setup-pool --db /var/lib/flyio/images.db
```

This command will:
1. Check if pool already exists
2. If missing, create pool files (`pool_meta`, `pool_data`) in the database directory
3. Set up loop devices
4. Create the thin-pool using dmsetup
5. Verify the pool is functional

### Manual Recovery (Legacy Method)

If you need fine-grained control or the automatic method fails:

#### Step 1: Clean Up Stale Loop Devices

```bash
# Check for existing loop devices
losetup -a

# Detach all loop devices (if safe)
sudo losetup -D
```

#### Step 2: Recreate the Thin-Pool

```bash
cd /var/lib/flyio

# Create backing files (adjust sizes as needed)
fallocate -l 1M pool_meta
fallocate -l 2G pool_data

# Create loop devices
METADATA_DEV="$(losetup -f --show pool_meta)"
DATA_DEV="$(losetup -f --show pool_data)"

# Create thin pool
dmsetup create --verifyudev pool --table "0 4194304 thin-pool ${METADATA_DEV} ${DATA_DEV} 2048 32768"

# Verify pool is active
dmsetup status pool
```

#### Step 3: Clean Up Stale Database Records (Optional)

If you want to start fresh, remove records for devices that no longer exist:

```bash
sqlite3 /var/lib/flyio/images.db "
  DELETE FROM snapshots;
  DELETE FROM unpacked_images;
  UPDATE images SET activation_status = 'inactive';
"
```

#### Step 4: Resume Operations

```bash
# Verify system health
sudo ./flyio-image-manager process-image --s3-key images/node/5.tar --quiet

# Or launch the monitor dashboard
sudo ./flyio-image-manager monitor
```

### Prevention

The system now performs comprehensive health checks before any devicemapper operations:

1. **Pool Existence Check** - Verifies thin-pool exists
2. **D-state Process Detection** - Checks for hung processes
3. **Kernel Log Scanning** - Scans dmesg for dm errors
4. **Memory Pressure Check** - Detects OOM conditions
5. **I/O Wait Check** - Detects storage bottlenecks

See [Usage Guide - Health Checks & Safeguards](USAGE.md#health-checks--safeguards) for details.

---

## Emergency Recovery

### Scenario 1: System Unresponsive / Kernel Panic

If the system becomes unresponsive or experiences a kernel panic:

#### Step 1: Reboot the System

**This is the safest option** to clear D-state processes and reset the dm-thin stack:

```bash
# Graceful reboot (if system responds)
sudo reboot

# Force reboot (if system is hung)
# Use IPMI, console, or physical reset button
```

#### Step 2: After Reboot - Reset Devicemapper

After reboot, the devicemapper state may be inconsistent. Use the reset script:

```bash
cd /path/to/flyio-image-manager
sudo ./test-scripts/reset-devicemapper.sh
```

This script will:
- Safely unmount all devices (with timeouts)
- Deactivate all thin devices (with timeouts)
- Remove the thin pool
- Recreate the pool from scratch

**WARNING**: This will destroy all unpacked images. You'll need to re-download and re-unpack.

#### Step 3: Clean Up Database

After resetting devicemapper, clean up the database:

```bash
sqlite3 /var/lib/flyio/images.db "DELETE FROM unpacked_images;"
sqlite3 /var/lib/flyio/images.db "DELETE FROM snapshots;"
```

#### Step 4: Restart Application

```bash
flyio-image-manager daemon
```

### Scenario 2: Pool Full

If the thin pool becomes full:

```bash
# Check pool status
dmsetup status pool

# Output format: start length target_type args
# Look for "needs_check" or "out_of_data_space" flags
```

**Resolution:**

1. **Run GC to free space**:
   ```bash
   flyio-image-manager gc --force
   ```

2. **If GC doesn't free enough space**, you'll need to:
   - Delete snapshots: `flyio-image-manager delete-snapshot <snapshot-id>`
   - Delete unpacked images (requires manual devicemapper cleanup)
   - Expand the pool (requires recreating with larger size)

### Scenario 3: Database Corruption

If the SQLite database becomes corrupted:

```bash
# Check database integrity
sqlite3 /var/lib/flyio/images.db "PRAGMA integrity_check;"

# If corrupted, restore from backup
cp /var/lib/flyio/images.db.backup /var/lib/flyio/images.db

# If no backup, rebuild from devicemapper state
# (This is complex - contact support)
```

---

## Monitoring and Diagnostics

### Key Metrics to Monitor

1. **Pool Usage**:
   ```bash
   dmsetup status pool
   # Monitor data usage percentage
   ```

2. **Orphaned Device Count**:
   ```bash
   flyio-image-manager gc --dry-run | grep "orphaned="
   ```

3. **FSM Failures**:
   ```bash
   # Check logs for FSM abort errors
   journalctl -u flyio-image-manager | grep "FSM aborted"
   ```

4. **D-State Processes**:
   ```bash
   # Check for hung processes
   ps aux | awk '$8 ~ /D/ {print}' | wc -l
   ```

### Log Analysis

Key log patterns to watch for:

- `"failed to create filesystem; leaving device active for manual/GC cleanup"` - Device creation failed, orphaned device created
- `"orphaned device detected"` - FSM detected orphaned device, requires GC
- `"timed_out": true` - Operation timed out, potential kernel issue
- `"deactivate failed or timed out"` - Cleanup operation failed, potential D-state hang

---

## Preventive Maintenance

### Regular Maintenance Tasks

1. **Weekly GC Run** (during low-traffic periods):
   ```bash
   flyio-image-manager gc --dry-run  # Preview
   flyio-image-manager gc --force    # Clean up
   ```

2. **Pool Status Check**:
   ```bash
   dmsetup status pool
   # Ensure usage is below 80%
   ```

3. **Database Backup**:
   ```bash
   sqlite3 /var/lib/flyio/images.db ".backup /var/lib/flyio/images.db.backup"
   ```

### Best Practices

1. **Avoid concurrent operations** - Run one FSM at a time when possible
2. **Monitor system resources** - Ensure adequate CPU, memory, and I/O capacity
3. **Run GC during idle periods** - Don't run GC during peak load
4. **Keep pool size adequate** - Maintain at least 20% free space
5. **Regular reboots** - Consider scheduled reboots to clear any accumulated kernel state issues

### Capacity Planning

Monitor pool usage trends:

```bash
# Check current usage
dmsetup status pool | awk '{print $6, $7}'

# Calculate usage percentage
# (used_sectors / total_sectors) * 100
```

If usage consistently exceeds 80%, consider:
- Increasing pool size (requires recreation)
- More aggressive GC schedule
- Deleting old snapshots
- Archiving/removing old images

