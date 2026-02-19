# Quick Start Guide

**Document Type**: Guide
**Audience**: Developers, Operators
**Prerequisites**: Linux system with root access, AWS credentials
**Estimated Time**: 5 minutes (automated) / 15 minutes (manual)
**Last Updated**: 2025-11-26

---

## TL;DR - Get Running in 5 Minutes

Copy and paste these commands on any Linux system with sudo access:

```bash
# 1. Clone and enter repository
git clone https://github.com/RupertBothma/flyio.git
cd flyio

# 2. Configure AWS credentials (if not already set)
export AWS_ACCESS_KEY_ID="your-access-key"
export AWS_SECRET_ACCESS_KEY="your-secret-key"
export AWS_REGION="us-east-1"

# 3. Run automated setup (installs ALL dependencies, creates pool, builds app)
sudo -E ./scripts/setup.sh --force

# 4. Process an image (launches TUI with animated progress bars)
sudo ./flyio-image-manager process-image --s3-key images/node/5.tar

# 5. When done, clean up safely
sudo ./scripts/cleanup.sh --all
```

**What you'll see**: A beautiful terminal UI showing download → unpack → activate progress with animated bars. The whole process takes 1-2 minutes per image.

**Optional**: Launch the monitoring dashboard with `sudo ./flyio-image-manager monitor`

---

## Prerequisites

### Required (You Must Have)

| Requirement | How to Check | Notes |
|-------------|--------------|-------|
| **Linux system** | `uname -s` → "Linux" | Ubuntu 20.04+, Debian 11+, or similar |
| **Root/sudo access** | `sudo whoami` → "root" | Needed for DeviceMapper operations |
| **AWS credentials** | `aws sts get-caller-identity` | For S3 bucket access |
| **10GB+ disk space** | `df -h /var` | For pool data and images |

### Automatically Installed by Setup Script

The setup script (`scripts/setup.sh`) automatically installs these if missing:
- Go 1.21+ (golang-go)
- DeviceMapper tools (lvm2, thin-provisioning-tools)
- AWS CLI
- SQLite3 and development libraries
- Build tools (rsync, curl, jq)

> **Bottom line**: If you have Linux + sudo + AWS credentials + disk space, just run the setup script. It handles everything else.

---

## Setup Script (`scripts/setup.sh`)

The setup script automates complete environment creation: dependencies, DeviceMapper pool, and application build.

### Most Common Usage

```bash
# First-time setup (recommended for new users)
sudo -E ./scripts/setup.sh --force
```

The `-E` flag preserves your AWS environment variables. The `--force` flag skips confirmation prompts.

**Expected output** (takes 1-3 minutes):
```
════════════════════════════════════════════════════════════════
      Fly.io Container Image Manager - Setup Script
════════════════════════════════════════════════════════════════

=== Pre-flight Checks ===
✓ Linux detected: 5.15.0-generic
✓ Running as root
✓ Disk space: 45GB available

=== Installing System Dependencies ===
✓ Go already installed: go1.21.0
✓ DeviceMapper tools already installed
...

=== Creating DeviceMapper Thin Pool ===
✓ Thin pool created successfully

=== Building Go Application ===
✓ Application built: ./flyio-image-manager

=== Setup Complete! ===
```

### When to Use Each Option

| Scenario | Command |
|----------|---------|
| **First time setup** | `sudo -E ./scripts/setup.sh --force` |
| **After reboot** (pool lost, files remain) | `sudo ./scripts/setup.sh --skip-deps --skip-build --force` |
| **Preview changes first** | `sudo ./scripts/setup.sh --dry-run` |
| **Larger pool for many images** | `sudo ./scripts/setup.sh --pool-size 8 --force` |
| **Using pre-built binary** | `sudo ./scripts/setup.sh --skip-build --force` |

### All Options

| Flag | Description |
|------|-------------|
| `--force` | Skip confirmation prompts (recommended for scripts) |
| `--skip-deps` | Skip dependency installation (faster re-setup) |
| `--skip-build` | Skip Go build (use existing binary) |
| `--pool-size SIZE` | Data pool size in GB (default: 2) |
| `--dry-run` | Preview without making changes |
| `--help` | Show help message |

### What It Does

1. **Pre-flight checks**: Verifies Linux, root access, disk space (10GB+)
2. **Installs dependencies**: Go, lvm2, thin-provisioning-tools, awscli, sqlite3
3. **Verifies tools**: go, dmsetup, losetup, mkfs.ext4, sqlite3
4. **Checks AWS credentials**: Environment variables or ~/.aws/credentials
5. **Creates DeviceMapper pool**: Backing files at `/var/lib/flyio/`, loop devices, thin pool
6. **Builds application**: Downloads Go deps, compiles `./flyio-image-manager`
7. **Validates setup**: Confirms pool active, directories exist, binary built

---

## Cleanup Script (`scripts/cleanup.sh`)

The cleanup script safely tears down the DeviceMapper environment in the correct order to prevent kernel panics.

### Usage

```bash
sudo ./scripts/cleanup.sh [options]
```

### Options

| Flag | Description |
|------|-------------|
| `--dry-run` | Preview what would be removed without making changes |
| `--force` | Skip confirmation prompts |
| `--all` | Remove everything including database, images, and FSM state |
| `--keep-db` | Keep the SQLite database (`images.db`) |
| `--keep-images` | Keep downloaded image files |
| `--help` | Show help message |

### Cleanup Order (Critical!)

The script removes components in this exact order to prevent issues:

1. **Unmount devices** - Any mounted filesystems in `/mnt/flyio/`
2. **Remove snapshots** - All `snap-*` devices
3. **Remove thin devices** - All `thin-*` devices
4. **Remove pool** - The main `pool` device
5. **Detach loops** - Loop devices for `pool_meta` and `pool_data`
6. **Remove files** - Backing files (ONLY after pool is gone)
7. **Optional cleanup** - Database, images, FSM state (prompts or `--all`)

⚠️ **WARNING**: Never delete pool backing files before removing the pool - this causes kernel panics.

### Examples

```bash
# Preview cleanup without making changes
sudo ./scripts/cleanup.sh --dry-run

# Full cleanup - removes everything
sudo ./scripts/cleanup.sh --all

# Full cleanup without prompts
sudo ./scripts/cleanup.sh --all --force

# Keep database but clean everything else
sudo ./scripts/cleanup.sh --keep-db

# Keep downloaded images
sudo ./scripts/cleanup.sh --keep-images

# Keep both database and images
sudo ./scripts/cleanup.sh --keep-db --keep-images
```

### Verification After Cleanup

```bash
# Should show no pool/thin/snap devices
dmsetup ls

# Should have no pool_* devices
losetup -a

# Pool files should be gone
ls /var/lib/flyio/
```

---

## Remote Server Execution

For running on a remote Linux server via SSH, adapt these examples to your environment:

```bash
# Define your remote server (adjust to your setup)
REMOTE_USER="user"
REMOTE_HOST="your-server.example.com"
REMOTE_PATH="/home/${REMOTE_USER}/flyio"

# Sync project to remote server
rsync -avz . ${REMOTE_USER}@${REMOTE_HOST}:${REMOTE_PATH}/

# Run setup on remote (non-interactive)
ssh ${REMOTE_USER}@${REMOTE_HOST} "cd ${REMOTE_PATH} && sudo ./scripts/setup.sh --force"

# Or run interactively (for prompts)
ssh -t ${REMOTE_USER}@${REMOTE_HOST}
cd ~/flyio
sudo ./scripts/setup.sh
```

### Remote Cleanup

```bash
# Full cleanup on remote
ssh ${REMOTE_USER}@${REMOTE_HOST} "cd ${REMOTE_PATH} && sudo ./scripts/cleanup.sh --all"

# Preview cleanup first
ssh ${REMOTE_USER}@${REMOTE_HOST} "cd ${REMOTE_PATH} && sudo ./scripts/cleanup.sh --dry-run"
```

### Cross-Compilation Workflow

When developing on macOS/Windows and deploying to a Linux server:

```bash
# Build locally for Linux
GOOS=linux GOARCH=amd64 go build -o flyio-image-manager ./cmd/flyio-image-manager

# Sync binary and scripts to remote
rsync -avz flyio-image-manager scripts/ ${REMOTE_USER}@${REMOTE_HOST}:${REMOTE_PATH}/

# Setup on remote (skip build since we synced binary)
ssh ${REMOTE_USER}@${REMOTE_HOST} "cd ${REMOTE_PATH} && sudo ./scripts/cleanup.sh --all && sudo ./scripts/setup.sh --skip-build --force"

# Test with TUI (requires -t for TTY allocation)
ssh -t ${REMOTE_USER}@${REMOTE_HOST} "cd ${REMOTE_PATH} && sudo ./flyio-image-manager process-image --s3-key images/node/5.tar"
```

---

## Common Workflows

### Fresh Start

```bash
# Clean slate + full setup
sudo ./scripts/cleanup.sh --all --force
sudo ./scripts/setup.sh --force
```

### Re-setup After Reboot

After a reboot, loop devices and the pool are gone but files remain:

```bash
# Option 1: Use setup-pool command (recommended - auto-creates pool)
sudo ./flyio-image-manager setup-pool --db /var/lib/flyio/images.db

# Option 2: Use setup script (full re-setup)
sudo ./scripts/setup.sh --skip-deps --skip-build --force
```

### Reset Pool Only (Keep Data)

```bash
sudo ./scripts/cleanup.sh --keep-db --keep-images --force
sudo ./scripts/setup.sh --skip-deps --skip-build --force
```

---

## Configure AWS Credentials

Set up AWS credentials for S3 access:

```bash
# Option 1: Environment variables
export AWS_ACCESS_KEY_ID="your-access-key"
export AWS_SECRET_ACCESS_KEY="your-secret-key"
export AWS_REGION="us-east-1"

# Option 2: AWS credentials file
aws configure
# Enter your credentials when prompted
```

### Verify S3 Access

```bash
aws s3 ls s3://flyio-container-images/images/
```

---

## Process Your First Image

Run this command immediately after setup:

```bash
sudo ./flyio-image-manager process-image --s3-key images/node/5.tar
```

### What You'll See (TUI Mode)

A beautiful terminal UI with animated progress bars:

```
╭─────────────────────────────────────────────────────╮
│  Processing: images/node/5.tar                      │
├─────────────────────────────────────────────────────┤
│  ✓ Download  ████████████████████████████████ 100%  │
│  ▸ Unpack    ████████████████░░░░░░░░░░░░░░░░  52%  │
│    Activate  ░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░   0%  │
╰─────────────────────────────────────────────────────╯
   Speed: 45.2 MB/s    Elapsed: 00:32
```

When complete (typically 1-2 minutes), you'll see:
```
✓ Image processed successfully!
  S3 Key:    images/node/5.tar
  Image ID:  img-a1b2c3d4
  Snapshot:  snap-node-5
  Duration:  1m 23s
```

### Verify It Worked

```bash
# List processed images (should show your image)
./flyio-image-manager list-images

# Check DeviceMapper devices (should show pool, thin-*, snap-*)
sudo dmsetup ls
```

**Expected `dmsetup ls` output:**
```
snap-node-5     (253:2)
thin-img-a1b2c  (253:1)
pool            (253:0)
```

### CLI Mode (for scripts/automation)

```bash
# Quiet mode - simple text output without TUI
sudo ./flyio-image-manager process-image --s3-key images/node/5.tar --quiet
```

### Interactive Dashboard

Monitor operations in real-time with the full-screen dashboard:

```bash
# Launch dashboard (use Ctrl+C or 'q' to exit)
sudo ./flyio-image-manager monitor

# For SSH sessions (disables alt-screen mode)
sudo ./flyio-image-manager monitor --inline
```

**Dashboard features:**
- **Monitor View** (Press `1`): FSM states, pool utilization, snapshots, logs
- **S3 Browser** (Press `2`): Browse and process images from S3

---

## Troubleshooting

### Error: `failed to create thin device: operation not permitted`

**Cause**: Not running with sudo
**Fix**:
```bash
sudo ./flyio-image-manager process-image --s3-key images/node/5.tar
```

### Error: `pool does not exist` or `Device "pool" not found`

**Cause**: Pool lost after reboot (loop devices don't persist)
**Fix** (choose one):
```bash
# Option 1: Use setup-pool command (recommended)
sudo ./flyio-image-manager setup-pool --db /var/lib/flyio/images.db

# Option 2: Use setup script
sudo ./scripts/setup.sh --skip-deps --skip-build --force
```

### Error: `NoCredentialProviders: no valid providers in chain`

**Cause**: AWS credentials not configured
**Fix**:
```bash
export AWS_ACCESS_KEY_ID="your-key"
export AWS_SECRET_ACCESS_KEY="your-secret"
export AWS_REGION="us-east-1"
# Then re-run your command
```

### Error: `AccessDenied: Access Denied` (S3)

**Cause**: Invalid AWS credentials or wrong bucket
**Verify**:
```bash
aws s3 ls s3://flyio-container-images/images/
# Should list image files - if not, check your credentials
```

### Error: `database is locked`

**Cause**: Another instance is running
**Fix**:
```bash
sudo pkill -f flyio-image-manager
# Then retry your command
```

### Error: `failed to open database file: out of memory (14)`

**Cause**: `/var/lib/flyio/` directory doesn't exist or wrong permissions
**Fix**:
```bash
sudo mkdir -p /var/lib/flyio && sudo chown $USER:$USER /var/lib/flyio
```

### Monitor shows "Pool: unavailable"

**Cause**: Pool exists but monitor can't read it
**Fix**: Ensure you're running monitor with sudo:
```bash
sudo ./flyio-image-manager monitor --inline
```

See [Troubleshooting Guide](TROUBLESHOOTING.md) for more detailed solutions.

---

## Next Steps

- **Process more images**: See [Development Guide](DEVELOPMENT.md) for batch processing
- **Monitor operations**: See [Troubleshooting Guide](TROUBLESHOOTING.md) for debugging
- **Operational procedures**: See [Operations Guide](OPERATIONS.md) for maintenance
- **Understand architecture**: See [System Architecture](../design/SYSTEM_ARCH.md)
- **Review FSM flows**: See [FSM Flow Design](../design/FSM_FLOWS.md)

---

## Advanced: Manual Setup

For users who need fine-grained control or are troubleshooting, here are the manual steps that `scripts/setup.sh` automates.

### Install Dependencies Manually

```bash
# Ubuntu/Debian
sudo apt-get update
sudo apt-get install -y golang-go lvm2 thin-provisioning-tools awscli sqlite3 libsqlite3-dev

# RHEL/CentOS/Fedora
sudo yum install -y golang device-mapper-persistent-data lvm2 awscli sqlite
```

### Create DeviceMapper Pool Manually

> **Recommended**: Use `setup-pool` command instead: `sudo ./flyio-image-manager setup-pool --db /var/lib/flyio/images.db`

```bash
# Create data directory
sudo mkdir -p /var/lib/flyio
cd /var/lib/flyio

# Create backing files (2GB pool)
sudo fallocate -l 4M pool_meta
sudo fallocate -l 2G pool_data

# Attach loop devices
METADATA_DEV=$(sudo losetup -f --show pool_meta)
DATA_DEV=$(sudo losetup -f --show pool_data)

# Calculate data size in sectors (2GB = 4194304 sectors)
DATA_SECTORS=$((2 * 1024 * 1024 * 1024 / 512))

# Create thin pool (256 = 128KB block size, 65536 = 32MB low water mark)
sudo dmsetup create pool --table "0 ${DATA_SECTORS} thin-pool ${METADATA_DEV} ${DATA_DEV} 256 65536"

# Verify
sudo dmsetup status pool
```

### Build Application Manually

```bash
cd /path/to/flyio
go mod download
go build -o flyio-image-manager ./cmd/flyio-image-manager
```

### Manual Cleanup (Emergency Only)

If `scripts/cleanup.sh` is unavailable, follow this exact order to avoid kernel panics:

```bash
# 1. Unmount any mounted devices
sudo umount /mnt/flyio/* 2>/dev/null || true

# 2. Remove snapshots first
sudo dmsetup ls | grep snap- | awk '{print $1}' | xargs -r -I{} sudo dmsetup remove {}

# 3. Remove thin devices
sudo dmsetup ls | grep thin- | awk '{print $1}' | xargs -r -I{} sudo dmsetup remove {}

# 4. Remove pool
sudo dmsetup remove pool

# 5. Detach loop devices
sudo losetup -a | grep pool | cut -d: -f1 | xargs -r sudo losetup -d

# 6. ONLY NOW remove backing files (after pool is gone!)
sudo rm -f /var/lib/flyio/pool_meta /var/lib/flyio/pool_data
```

⚠️ **CRITICAL**: Never delete pool backing files while the pool exists - this causes kernel panics that require a reboot.

---

## Support

For issues and questions:
- See [Troubleshooting Guide](TROUBLESHOOTING.md)
- Check [GitHub Issues](https://github.com/RupertBothma/flyio/issues)

