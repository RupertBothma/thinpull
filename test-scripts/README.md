# Operational Setup Scripts

Scripts for setting up and managing the Linux devicemapper environment. These are **operational scripts** for running the system, not test suites.

> **NOTE**: These scripts set up infrastructure, not test suites.
> They prepare the Linux environment required to run the application.

## Requirements

- Linux operating system (kernel 3.10+)
- Root/sudo access
- DeviceMapper support (`dmsetup`, `losetup`)
- Go 1.21+
- AWS credentials configured

## Essential Scripts

### Environment Setup

| Script | Purpose |
|--------|---------|
| `check-environment.sh` | Verify system meets all prerequisites |
| `setup-test-environment.sh` | Create devicemapper pool and directories |
| `cleanup-test-environment.sh` | Safely remove pool and cleanup (interactive) |
| `reset-devicemapper.sh` | Quick reset: remove and recreate pool |

### Utilities

| Script | Purpose |
|--------|---------|
| `create-dynamic-pool.sh` | Create pool with sizes based on S3 bucket analysis |
| `analyze-s3-bucket.sh` | Analyze S3 bucket to determine pool sizing |
| `check-aws-permissions.sh` | Verify AWS credentials and S3 access |
| `test-images.conf` | Configuration: known test images and their properties |

## DeviceMapper Configuration

All scripts use the configuration specified in the main README:

```bash
# Pool Configuration (from README.md)
# - Metadata: 4MB (0.2% of data size)
# - Data: 2GB (sufficient for 3-4 large images)
# - Block size: 256 sectors (128KB) - CRITICAL for performance
# - Low water mark: 65536 sectors (32MB)

fallocate -l 4M pool_meta
fallocate -l 2G pool_data
METADATA_DEV="$(losetup -f --show pool_meta)"
DATA_DEV="$(losetup -f --show pool_data)"
dmsetup create --verifyudev pool --table "0 4194304 thin-pool ${METADATA_DEV} ${DATA_DEV} 256 65536"
```

> **WARNING**: Do NOT use 2048 sectors (1MB blocks) - causes severe I/O degradation!

## Quick Start

```bash
# 1. Verify environment
sudo ./check-environment.sh

# 2. Set up devicemapper pool
sudo ./setup-test-environment.sh

# 3. Run the application (see main README)
sudo ../flyio-image-manager process-image --image-id test-001

# 4. When done, cleanup
sudo ./cleanup-test-environment.sh
```

## macOS Users

DeviceMapper requires Linux. Options:
1. Remote Linux server via SSH
2. EC2 instance (Amazon Linux 2 or Ubuntu)
3. Multipass VM: `multipass launch --name flyio-test ubuntu`

## Safety Notes

These scripts follow the "fail-dumb" pattern documented in `docs/design/ADR-001-KERNEL-PANIC-MITIGATION.md`:

- **No automatic cleanup on error paths** - prevents kernel panics
- **Timeout protection** on all dmsetup operations
- **D-state detection** before cleanup operations
- **Interactive confirmation** before destructive operations

## Related Documentation

- [Quick Start Guide](../docs/guide/QUICKSTART.md) - Full setup walkthrough
- [Development Guide](../docs/guide/DEVELOPMENT.md) - Build and run instructions
- [Kernel Panic Mitigation](../docs/design/ADR-001-KERNEL-PANIC-MITIGATION.md) - Safety patterns
