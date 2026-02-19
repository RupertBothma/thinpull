# Integration Testing Guide

**Document Type**: Guide  
**Status**: Ready for Execution  
**Version**: 1.1  
**Last Updated**: 2025-11-25

---

> **NOTE**: These are manual validation procedures for development and debugging purposes.
> They supplement (but do not replace) automated testing.

## Overview

Manual integration testing procedures for the Container Image Manager. These validate the complete pipeline, crash recovery, idempotency, concurrent operations, and error handling.

**Prerequisites**: Linux system with devicemapper support and root access  
**Estimated Time**: 2-3 hours

**Note**: These tests require Linux. macOS users should use a Linux VM, EC2 instance, or remote Linux server.

---

## Quick Start

```bash
# 1. Environment check
sudo ./test-scripts/check-environment.sh

# 2. Setup test environment  
sudo ./test-scripts/setup-test-environment.sh

# 3. Process an image (manual validation)
sudo ./flyio-image-manager process-image --s3-key images/node/5.tar

# 4. Verify results
sudo dmsetup ls | grep -E "thin-|snap-"
sqlite3 /var/lib/flyio/images.db "SELECT * FROM images;"

# 5. Cleanup when done
sudo ./test-scripts/cleanup-test-environment.sh
```

---

## Available Scripts

Operational scripts are located in the `test-scripts/` directory. See [`test-scripts/README.md`](../../test-scripts/README.md) for full documentation.

| Script | Purpose |
|--------|---------|
| `check-environment.sh` | Verify system meets all prerequisites |
| `setup-test-environment.sh` | Create devicemapper pool and directories |
| `cleanup-test-environment.sh` | Safely remove pool and cleanup |
| `reset-devicemapper.sh` | Quick reset: remove and recreate pool |

### DeviceMapper Configuration

All scripts use the configuration from the main README:

```bash
# Pool Configuration
# - Metadata: 4MB (0.2% of data size)
# - Data: 2GB (sufficient for 3-4 large images)
# - Block size: 256 sectors (128KB) - CRITICAL for performance
# - Low water mark: 65536 sectors (32MB)

dmsetup create --verifyudev pool --table "0 4194304 thin-pool $METADATA_DEV $DATA_DEV 256 65536"
```

> **WARNING**: Do NOT use 2048 sectors (1MB blocks) - causes severe I/O degradation!

---

## Manual Testing Procedures

### Test 1: Basic End-to-End

```bash
# Setup
sudo ./test-scripts/setup-test-environment.sh

# Process image
sudo ./flyio-image-manager process-image --s3-key "images/test.tar"

# Verify database
sqlite3 /var/lib/flyio/images.db "SELECT * FROM images;"

# Verify device
sudo dmsetup ls | grep thin

# Mount and inspect
SNAP=$(sqlite3 /var/lib/flyio/images.db "SELECT device_path FROM snapshots LIMIT 1;")
sudo mkdir -p /mnt/test
sudo mount "$SNAP" /mnt/test
ls /mnt/test
sudo umount /mnt/test
```

### Test 2: Crash Recovery

```bash
# Start processing
sudo ./flyio-image-manager process-image --s3-key "images/large.tar" &
PID=$!

# Kill after 5 seconds
sleep 5
sudo kill -9 $PID

# Resume
sudo ./flyio-image-manager process-image --s3-key "images/large.tar"

# Should complete successfully
```

### Test 3: Concurrent Operations

```bash
# Start 3 concurrent
sudo ./flyio-image-manager process-image --s3-key "images/test1.tar" &
sudo ./flyio-image-manager process-image --s3-key "images/test2.tar" &
sudo ./flyio-image-manager process-image --s3-key "images/test3.tar" &

# Wait for all
wait

# Verify
sqlite3 /var/lib/flyio/images.db "SELECT COUNT(*) FROM images;"
# Should be 3
```

---

## Expected Results

**Successful Test Suite**:
```
=== SUMMARY ===
Run: 3 | Passed: 3 | Failed: 0
✓ ALL PASSED
```

**Performance Benchmarks**:
- Download (100MB): 5-15s
- Unpack (100MB): 10-20s  
- Activate: <1s
- Idempotency: <1s

---

## Troubleshooting

**Issue**: Device not found  
**Solution**: Check pool status with `dmsetup status pool`

**Issue**: S3 access denied  
**Solution**: Set AWS credentials: `export AWS_ACCESS_KEY_ID=...`

**Issue**: Mount failed  
**Solution**: Check device active: `dmsetup info <device>`

---

## Next Steps

After successful integration testing:

1. **Update Archon Task**: Mark integration testing as DONE
2. **Document Results**: Create test report with metrics
3. **Code Review**: Run `go fmt`, `go vet`, add godoc comments
4. **Production Deployment**: System is validated and ready

---

## References

- [Usage Guide](USAGE.md) - Complete CLI documentation
- [Troubleshooting](TROUBLESHOOTING.md) - Error resolution
- [Durable State Contracts](../design/DURABLE_STATE_CONTRACTS.md) - Crash recovery
