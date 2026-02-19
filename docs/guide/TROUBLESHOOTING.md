# Troubleshooting Guide

**Document Type**: Guide
**Audience**: Operators, Developers
**Last Updated**: 2025-11-26

---

## Overview

This guide provides solutions to common issues encountered when running the Fly.io Container Image Management System.

---

## DeviceMapper Issues

### Issue: "Pool does not exist"

**Symptoms**:
```
Error: pool check failed: thin-pool "pool" does not exist.
This typically happens after a kernel panic or reboot.
```

**Cause**: DeviceMapper pool was destroyed by a kernel panic, system reboot, or was never created.

**Quick Solution**:

The system **automatically recreates the pool** when missing. Simply run any command:

```bash
# Pool will be auto-created if missing
sudo ./flyio-image-manager process-image --s3-key images/test.tar --db /var/lib/flyio/images.db

# Or explicitly create/recreate the pool
sudo ./flyio-image-manager setup-pool --db /var/lib/flyio/images.db
```

**See Also**: [Operations Guide - Pool Recovery](OPERATIONS.md#pool-recovery-after-kernel-panic) for manual recovery procedures and stale database cleanup.

---

### Issue: "Pool not found" (legacy error)

**Symptoms**:
```
Error: devicemapper pool 'pool' not found
```

**Cause**: DeviceMapper pool not created or not active

**Solution**:
```bash
# Check if pool exists
sudo dmsetup ls | grep pool

# If not found, create pool (see Quick Start Guide)
# CRITICAL: Use 256 (128KB blocks), NOT 2048 (1MB blocks - causes I/O slowdown)
sudo dmsetup create pool --table "0 $DATA_SIZE thin-pool /dev/loop1 /dev/loop0 256 65536 1 skip_block_zeroing"

# Verify pool is active
sudo dmsetup status pool
```

---

### Issue: "Pool is full"

**Symptoms**:
```
Error: devicemapper pool is full, cannot create device
```

**Cause**: Pool has reached capacity

**Solution**:
```bash
# Check pool usage
sudo dmsetup status pool | awk '{print "Used:", $6, "Total:", $7}'

# Option 1: Delete unused devices
sudo dmsetup remove thin-12345
sudo dmsetup message pool 0 "delete 12345"

# Option 2: Expand pool (advanced)
# See DeviceMapper documentation for pool expansion

# Option 3: Create new pool with larger size
# Backup data, recreate pool with larger data device
```

---

### Issue: "Orphaned Device Detected"

**Symptoms**:
```
Error: orphaned device thin-abc123 detected after failed creation; run 'flyio-image-manager gc --force' to clean up
FSM aborted with error: orphaned device exists without database record; manual cleanup required
```

**Cause**: Device was partially created but the operation failed partway through (e.g., mkfs timeout, activation failure). The device exists in devicemapper but has no corresponding database record.

**Why This Happens**: The system intentionally does NOT automatically clean up failed devices because cleanup operations (unmount, deactivate, delete) can trigger kernel panics when performed on devices that just failed. This is the "fail-dumb" pattern - we accept resource leakage to prevent system instability.

**Solution**:

1. **Verify the system is idle** (no active FSMs running):
   ```bash
   # Stop the daemon if running
   pkill -SIGTERM flyio-image-manager

   # Verify no FSMs are active
   ps aux | grep flyio-image-manager
   ```

2. **Preview what will be cleaned** (dry run):
   ```bash
   flyio-image-manager gc --dry-run
   ```

3. **Clean up orphaned devices**:
   ```bash
   flyio-image-manager gc --force
   ```

4. **If cleanup fails or hangs**, see [Emergency Recovery](#emergency-recovery) below.

**Prevention**: Monitor pool usage and system resources. Orphaned devices typically occur under high system load or when the pool is nearly full.

**See Also**: [Operations Guide - Orphaned Device Management](OPERATIONS.md#orphaned-device-management)

---

### Issue: "Device already exists"

**Symptoms**:
```
Error: device 'thin-12345' already exists
```

**Cause**: Device ID collision or leftover device from previous run

**Solution**:
```bash
# Check if device exists
sudo dmsetup info thin-12345

# If device exists and is not needed, remove it
sudo dmsetup remove thin-12345

# If device is in use, check what's using it
sudo lsof /dev/mapper/thin-12345

# The FSM will automatically retry with a new device ID
```

---

### Issue: "Cannot activate device"

**Symptoms**:
```
Error: failed to activate device 'thin-12345'
```

**Cause**: Device exists but is not active, or pool is suspended

**Solution**:
```bash
# Check pool status
sudo dmsetup status pool

# If pool is suspended, resume it
sudo dmsetup resume pool

# Try activating device manually
sudo dmsetup create thin-12345 --table "0 20971520 thin /dev/mapper/pool 12345"

# Check for errors in kernel log
sudo dmesg | tail -20
```

---

### Issue: "SSH command hangs (sudo dmsetup)"

**Symptoms**:
```
Command hangs when running sudo dmsetup via SSH
```

**Cause**:
1. `sudo` is waiting for a password (SSH does not allocate TTY by default)
2. `dmsetup` is blocked by the kernel (unresponsive storage)

**Solution**:
```bash
# 1. Force TTY allocation
# Use -t flag to force pseudo-terminal allocation to see password prompt
ssh -t user@host "sudo dmsetup status"

# 2. Isolate the issue
# Verify sudo works with trivial command
ssh -t user@host "sudo echo check"

# If sudo works but dmsetup hangs, check process state on remote host
ssh user@host "ps aux | grep dmsetup"
# If state is 'D' (uninterruptible sleep), storage pool is likely dead/locked
```

### Issue: "Kernel Deadlock (D state)"

**Symptoms**:
```
Processes in 'D' state (Uninterruptible Sleep)
dmsetup commands hang indefinitely and cannot be killed (kill -9 fails)
losetup shows (deleted) backing files
```

**Cause**:
The underlying backing files for loop devices were deleted while the device mapper pool was still active. The kernel is waiting for I/O operations that can never complete.

**Solution**:
This state is usually unrecoverable without a reboot.

```bash
# 1. Verify the issue
ssh user@host "ps aux | grep -E 'D.*dmsetup'"
ssh user@host "losetup -a | grep deleted"

# 2. Reboot the server
ssh user@host "sudo reboot"
```

---

## S3 Access Issues

### Issue: "Access denied"

**Symptoms**:
```
Error: S3 access denied for bucket 'flyio-container-images'
```

**Cause**: Invalid AWS credentials or insufficient permissions

**Solution**:
```bash
# Verify AWS credentials are set
echo $AWS_ACCESS_KEY_ID
echo $AWS_SECRET_ACCESS_KEY

# Test S3 access
aws s3 ls s3://flyio-container-images/images/

# If access denied, check IAM permissions
# Required permissions: s3:GetObject, s3:ListBucket

# Set credentials if missing
export AWS_ACCESS_KEY_ID="your-access-key"
export AWS_SECRET_ACCESS_KEY="your-secret-key"
export AWS_REGION="us-east-1"
```

---

### Issue: "Network timeout"

**Symptoms**:
```
Error: download timeout after 5 minutes
```

**Cause**: Slow network connection or large file

**Solution**:
```bash
# Increase download timeout
export DOWNLOAD_TIMEOUT=600  # 10 minutes

# Check network connectivity
ping -c 3 s3.amazonaws.com

# Test download speed
aws s3 cp s3://flyio-container-images/images/test.tar /tmp/test.tar

# The FSM will automatically retry failed downloads
```

---

### Issue: "Invalid S3 key"

**Symptoms**:
```
Error: S3 key validation failed: path traversal detected
```

**Cause**: S3 key contains invalid characters or path traversal

**Solution**:
```bash
# Verify S3 key format
# Valid: "images/alpine.tar"
# Invalid: "../images/alpine.tar", "/etc/passwd"

# List available images
aws s3 ls s3://flyio-container-images/images/

# Use correct S3 key
./flyio-image-manager process-image --s3-key "images/alpine.tar" --image-id "alpine-001"
```

---

## Database Issues

### Issue: "Database locked"

**Symptoms**:
```
Error: database is locked
```

**Cause**: Another process has exclusive lock on database

**Solution**:
```bash
# Check for other instances
ps aux | grep flyio-image-manager

# Kill other instances if safe
pkill flyio-image-manager

# Check for stale lock files
ls -la /var/lib/flyio/*.db-shm
ls -la /var/lib/flyio/*.db-wal

# Remove stale lock files (only if no other instances running)
rm /var/lib/flyio/images.db-shm
rm /var/lib/flyio/images.db-wal

# The FSM will automatically retry with exponential backoff
```

---

### Issue: "Database corruption"

**Symptoms**:
```
Error: database disk image is malformed
```

**Cause**: Unclean shutdown or disk failure

**Solution**:
```bash
# Check database integrity
sqlite3 /var/lib/flyio/images.db "PRAGMA integrity_check;"

# If corrupted, restore from backup
cp /var/lib/flyio/images.db.backup /var/lib/flyio/images.db

# If no backup, try recovery
sqlite3 /var/lib/flyio/images.db ".recover" | sqlite3 /var/lib/flyio/images-recovered.db

# Rebuild database from FSM event log (advanced)
# The FSM library maintains event log for recovery
```

---

### Issue: "Foreign key constraint failed"

**Symptoms**:
```
Error: FOREIGN KEY constraint failed
```

**Cause**: Attempting to delete image with active snapshots

**Solution**:
```bash
# Check for active snapshots
sqlite3 /var/lib/flyio/images.db "SELECT * FROM snapshots WHERE image_id = 'img-123' AND active = 1;"

# Deactivate snapshots first
./flyio-image-manager deactivate-snapshot --snapshot-id <snapshot-id>

# Then delete image
./flyio-image-manager delete-image --image-id img-123
```

---

## Extraction Issues

### Issue: "Path traversal detected"

**Symptoms**:
```
Error: path traversal detected: rootfs/../../etc/passwd
```

**Cause**: Malicious tar archive with path traversal

**Solution**:
```
# This is expected behavior - the system is protecting against malicious content
# The tar archive is rejected and the FSM is aborted

# Verify tar contents
tar -tzf /path/to/image.tar | grep '\.\.'

# If legitimate tar, ensure paths are relative and don't contain '..'
# Contact image provider if this is unexpected
```

---

### Issue: "File too large"

**Symptoms**:
```
Error: file too large: 2GB (max 1GB)
```

**Cause**: Tar contains file exceeding size limit

**Solution**:
```
# This is expected behavior - enforcing security limits

# Check file sizes in tar
tar -tvf /path/to/image.tar | sort -k5 -n | tail -10

# If legitimate large file, increase limit (requires code change)
# Edit extraction/extract.go: maxFileSize constant

# Or split large files in source image
```

---

### Issue: "Extraction timeout"

**Symptoms**:
```
Error: extraction timeout after 30 minutes
```

**Cause**: Very large tar or slow I/O

**Solution**:
```bash
# Increase extraction timeout
export UNPACK_TIMEOUT=3600  # 60 minutes

# Check I/O performance
sudo iostat -x 1 10

# Check for I/O bottlenecks
sudo iotop

# The FSM will automatically retry
```

---

## FSM Issues

### Issue: "FSM stuck in 'doing' state"

**Symptoms**:
```
FSM run has been in 'doing' state for hours
```

**Cause**: Transition hung or timeout not configured

**Solution**:
```bash
# Check FSM status
./flyio-image-manager status --run-id <run-id>

# Check FSM database
sqlite3 /var/lib/flyio/fsm/fsm-state.db "SELECT * FROM active WHERE run_id = '<run-id>';"

# Check for hung processes
ps aux | grep flyio-image-manager

# Cancel FSM (if safe)
./flyio-image-manager cancel --run-id <run-id>

# Check logs for errors
journalctl -u flyio-image-manager -f
```

---

### Issue: "FSM not resuming after restart"

**Symptoms**:
```
Active FSMs not resumed after application restart
```

**Cause**: Resume function not called or FSM database corrupted

**Solution**:
```bash
# Verify FSM database exists
ls -la /var/lib/flyio/fsm/fsm-state.db

# Check for active FSMs
sqlite3 /var/lib/flyio/fsm/fsm-state.db "SELECT COUNT(*) FROM active;"

# Ensure resume is called in main.go
# resumeDownloads(ctx)
# resumeUnpacks(ctx)
# resumeActivates(ctx)

# Check application logs for resume errors
journalctl -u flyio-image-manager | grep resume
```

---

## Performance Issues

### Issue: "Slow downloads"

**Symptoms**:
```
Downloads taking longer than expected
```

**Cause**: Network bandwidth, S3 throttling, or queue limits

**Solution**:
```bash
# Check network speed
speedtest-cli

# Check S3 transfer rate
aws s3 cp s3://flyio-container-images/images/large.tar /tmp/test.tar --debug

# Increase concurrent downloads (if bandwidth allows)
export DOWNLOAD_QUEUE_SIZE=10

# Check queue status
./flyio-image-manager queue-status --queue downloads
```

---

### Issue: "High memory usage"

**Symptoms**:
```
Application using excessive memory
```

**Cause**: Memory leak or large files buffered in memory

**Solution**:
```bash
# Check memory usage
ps aux | grep flyio-image-manager

# Profile memory usage
go tool pprof http://localhost:6060/debug/pprof/heap

# Verify streaming is used (not buffering entire files)
# Check s3/client.go: should use io.Copy, not ioutil.ReadAll

# Restart application to free memory
sudo systemctl restart flyio-image-manager
```

---

## Security Validation Failures

### Issue: "Symlink target escapes rootfs"

**Symptoms**:
```
Error: symlink target escapes base directory: rootfs/link -> /etc/passwd
```

**Cause**: Malicious symlink in tar archive

**Solution**:
```
# This is expected behavior - protecting against symlink attacks
# The tar archive is rejected and the FSM is aborted

# Verify symlinks in tar
tar -tvf /path/to/image.tar | grep '^l'

# If legitimate symlink, ensure target is relative and within rootfs
# Contact image provider if this is unexpected
```

---

## Emergency Recovery

### System Unresponsive / Kernel Panic

If the system becomes unresponsive or experiences a kernel panic after devicemapper operations:

**Immediate Action**:
1. **Reboot the system** - This is the safest option to clear D-state processes and reset the dm-thin stack
   ```bash
   # Graceful reboot (if system responds)
   sudo reboot

   # Force reboot (if system is hung)
   # Use IPMI, console, or physical reset button
   ```

2. **After reboot - Reset devicemapper**:
   ```bash
   cd /path/to/flyio-image-manager
   sudo ./test-scripts/reset-devicemapper.sh
   ```

   **WARNING**: This will destroy all unpacked images. You'll need to re-download and re-unpack.

3. **Clean up database**:
   ```bash
   sqlite3 /var/lib/flyio/images.db "DELETE FROM unpacked_images;"
   sqlite3 /var/lib/flyio/images.db "DELETE FROM snapshots;"
   ```

4. **Restart application**:
   ```bash
   flyio-image-manager daemon
   ```

**See Also**: [Operations Guide - Emergency Recovery](OPERATIONS.md#emergency-recovery)

### GC Command Hangs

If the `flyio-image-manager gc --force` command hangs:

1. **Check for D-state processes**:
   ```bash
   ps aux | awk '$8 ~ /D/ {print}'
   ```

2. **If D-state processes exist**:
   - **DO NOT** try to kill them (kill -9 won't work)
   - **DO NOT** run more cleanup commands
   - **Reboot the system** (only safe option)

3. **After reboot**:
   - Run GC again: `flyio-image-manager gc --force`
   - If still fails, use reset script: `./test-scripts/reset-devicemapper.sh`

---

## TUI Dashboard Issues

### Issue: "Enter key does not trigger image processing"

**Symptoms**:
- Pressing Enter on a selected image in the TUI dashboard does nothing
- Activity Log shows `viewMode=0` when Enter is pressed

**Cause**: The user is in Monitor view (viewMode=0) instead of Images view (viewMode=1). The Enter key only triggers image processing when in Images view.

**Solution**:
1. Press `2` to switch to the Images view
2. Verify the Activity Log shows: `"Switched to Images view (viewMode=1)"`
3. Navigate to an image with `j`/`k` or arrow keys
4. Press `Enter` to trigger processing

**Debug Information**:
When Enter is pressed, the Activity Log displays:
```
Enter pressed: viewMode=X, processingImage="", s3Browser=true, images=N
```
- `viewMode=0`: You're in Monitor view - press `2` first
- `viewMode=1`: You're in Images view - Enter should work
- `processingImage="xyz"`: An image is already being processed
- `images=0`: No images loaded - press `r` to refresh

---

### Issue: "Text shifting/misalignment in image list"

**Symptoms**:
- Text in the S3 Images list shifts when moving the selection cursor
- Columns misalign between selected and non-selected rows

**Cause**: Unicode cursor characters (like `▶`) may render with inconsistent widths across terminals.

**Solution**: This has been fixed by using ASCII `>` for the selection cursor with consistent padding.

---

### Issue: "S3 Images panel shows 'Loading...' indefinitely"

**Symptoms**:
- S3 Images panel shows "Loading images from S3..." but never completes
- No images appear after waiting

**Cause**: S3 client failed to connect or credentials are missing

**Solution**:
```bash
# Verify AWS credentials
echo $AWS_ACCESS_KEY_ID
echo $AWS_SECRET_ACCESS_KEY

# Test S3 access
aws s3 ls s3://flyio-container-images/images/

# Press 'r' in TUI to retry loading
```

---

### Issue: "Processing fails with 'ImageID not set'"

**Symptoms**:
```
Error: image ID is required
```

**Cause**: The ImageID was not derived from the S3 key when triggering from TUI.

**Solution**: This has been fixed. The TUI now automatically derives ImageID from S3 key using `fsm.DeriveImageIDFromS3Key(s3Key)`.

---

### TUI Keyboard Reference

| Key | Action |
|-----|--------|
| `1` | Switch to Monitor view |
| `2` | Switch to Images view |
| `j`/`↓` | Move selection down |
| `k`/`↑` | Move selection up |
| `g` | Jump to first item |
| `G` | Jump to last item |
| `Enter` | Process selected image (Images view only) |
| `r` | Refresh data |
| `Tab` | Cycle focus between panels |
| `q`/`Ctrl+C` | Quit |

---

## Getting Help

### Collect Diagnostic Information

```bash
# System information
uname -a
go version

# DeviceMapper status
sudo dmsetup ls
sudo dmsetup status pool

# Database status
sqlite3 /var/lib/flyio/images.db "SELECT COUNT(*) FROM images;"
sqlite3 /var/lib/flyio/fsm/fsm-state.db "SELECT COUNT(*) FROM active;"

# Application logs
journalctl -u flyio-image-manager --since "1 hour ago"

# Disk space
df -h /var/lib/flyio
```

### Report Issues

When reporting issues, include:
1. Error message (full stack trace)
2. Steps to reproduce
3. System information (OS, Go version)
4. Diagnostic information (see above)
5. Relevant logs

---

## References

- [Quick Start Guide](QUICKSTART.md) - Setup instructions
- [Development Guide](DEVELOPMENT.md) - Debugging techniques
- [System Architecture](../design/SYSTEM_ARCH.md) - Component interactions
- [Security Design](../design/SECURITY.md) - Security validations

