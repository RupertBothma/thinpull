# Usage Guide

**Document Type**: Guide
**Status**: Complete
**Version**: 1.2
**Last Updated**: 2025-11-26

---

## Overview

This guide provides complete instructions for building, configuring, and operating the Fly.io Container Image Management System. Follow these steps to set up and use the system for processing container images from S3 into devicemapper snapshots.

**Audience**: System administrators, operators, and developers

---

## Prerequisites

### System Requirements

| Requirement | Version | Purpose |
|------------|---------|---------|
| **Operating System** | Linux | DeviceMapper support required |
| **Go** | 1.23.0+ | Build and run the application |
| **DeviceMapper** | 2.x+ | Thin provisioning support |
| **Root Access** | sudo/root | DeviceMapper operations |
| **Disk Space** | 10GB+ free | Image storage and devicemapper pool |
| **Memory** | 2GB+ RAM | Image processing |

### Required Tools

```bash
# Verify Go installation
go version
# Expected: go version go1.23.0 or higher

# Verify DeviceMapper tools
which dmsetup
which losetup
which mkfs.ext4

# Check if running as root (required for devicemapper)
id
# Expected: uid=0(root)
```

### AWS Credentials

The system requires AWS credentials to access S3:

```bash
# Option 1: Environment variables
export AWS_ACCESS_KEY_ID="your-access-key"
export AWS_SECRET_ACCESS_KEY="your-secret-key"
export AWS_REGION="us-east-1"

# Option 2: AWS credentials file (~/.aws/credentials)
[default]
aws_access_key_id = your-access-key
aws_secret_access_key = your-secret-key

# Option 3: IAM role (if running on AWS EC2)
# No credentials needed, instance role provides access

# Verify access to S3 bucket
aws s3 ls s3://flyio-container-images/images/
```

---

## Installation

### Step 1: Clone and Build

```bash
# Clone the repository
cd /opt
git clone https://github.com/your-org/flyio.git
cd flyio

# Download dependencies
go mod download

# Build the application
go build -o flyio-image-manager ./cmd/flyio-image-manager

# Verify build
./flyio-image-manager --help
```

### Step 2: Create Directories

```bash
# Create required directories with proper permissions
sudo mkdir -p /var/lib/flyio/images
sudo mkdir -p /var/lib/flyio/fsm
sudo mkdir -p /mnt/flyio
sudo chmod 755 /var/lib/flyio
sudo chmod 755 /mnt/flyio
```

### Step 3: Set Up DeviceMapper Pool

This is the most critical setup step. The devicemapper pool must be created before processing images.

```bash
# Navigate to a directory with sufficient space
cd /var/lib/flyio

# Pool sizing should be based on S3 bucket analysis. Run:
#   ./analyze-s3 s3://flyio-container-images/images
# to get recommended sizes for your workload.

# Example for small workload (~500MB total images):
# - Metadata: 0.2% of data size (minimum 4MB)
# - Data: Total image size + 30% overhead for snapshots
fallocate -l 4M pool_meta
fallocate -l 2G pool_data

# Create loop devices
METADATA_DEV="$(losetup -f --show pool_meta)"
DATA_DEV="$(losetup -f --show pool_data)"

# Verify loop devices created
echo "Metadata device: $METADATA_DEV"
echo "Data device: $DATA_DEV"
# Example output: /dev/loop0, /dev/loop1

# Create thin pool
# Format: 0 <table_size> thin-pool <metadata_dev> <data_dev> <block_size> <low_water_mark>
#
# Parameters explained:
# - table_size: pool_data_bytes / 512 (sector size). 2GB = 4194304 sectors
# - block_size: 256 sectors (128KB) - CRITICAL for performance
#   WARNING: 2048 sectors (1MB blocks) causes severe I/O slowdown!
# - low_water_mark: 65536 sectors (32MB) - triggers space warning
dmsetup create --verifyudev pool --table "0 4194304 thin-pool ${METADATA_DEV} ${DATA_DEV} 256 65536"

# Verify pool created
dmsetup ls
dmsetup status pool
# Expected: pool active with no errors
```

**Production Setup Notes**:
- Use LVM volumes instead of loop devices for better performance
- Size metadata device based on expected image count (1MB per ~100 images)
- Size data device based on total image storage needs
- Monitor pool usage to avoid exhaustion

---

## Configuration

### Configuration Options

All configuration can be provided via command-line flags. The system uses sensible defaults:

| Flag | Default | Description |
|------|---------|-------------|
| `--bucket` | `flyio-container-images` | S3 bucket name |
| `--region` | `us-east-1` | AWS region |
| `--db` | `/var/lib/flyio/images.db` | SQLite database path |
| `--fsm-db` | `/var/lib/flyio/fsm` | FSM state directory (BoltDB) |
| `--pool` | `pool` | DeviceMapper pool name |
| `--mount-root` | `/mnt/flyio` | Temporary mount point directory |
| `--local-dir` | `/var/lib/flyio/images` | Downloaded image storage |
| `--download-queue` | `5` | Max concurrent downloads |
| `--unpack-queue` | `2` | Max concurrent unpacking operations |
| `--log-level` | `info` | Log level (debug, info, warn, error) |

### Environment Variables

You can also set configuration via environment variables:

```bash
export AWS_REGION="us-east-1"
export AWS_ACCESS_KEY_ID="your-key"
export AWS_SECRET_ACCESS_KEY="your-secret"
```

---

## CLI Commands

### process-image

Process a container image through the complete pipeline (Download → Unpack → Activate).

**Usage**:
```bash
sudo ./flyio-image-manager process-image --s3-key <key> [options]
```

**Required Flags**:
- `--s3-key`: S3 object key (e.g., "images/alpine-3.18.tar")

**Optional Flags**:
- `--image-id`: Override auto-derived image ID
- `--bucket`: Override S3 bucket
- `--region`: Override AWS region
- `--pool`: Override devicemapper pool name
- `--log-level`: Set log verbosity

**Example 1: Basic usage**
```bash
sudo ./flyio-image-manager process-image \
  --s3-key "images/alpine-3.18.tar"
```

**Output**:
```json
{"level":"info","msg":"processing image","s3_key":"images/alpine-3.18.tar","image_id":"img_abc123...","bucket":"flyio-container-images","time":"2025-11-21T20:00:00Z"}
{"level":"info","msg":"resuming in-flight FSM runs","time":"2025-11-21T20:00:00Z"}
{"level":"info","msg":"starting download FSM","time":"2025-11-21T20:00:01Z"}
{"level":"info","msg":"download FSM completed","image_id":"img_abc123...","local_path":"/var/lib/flyio/images/img_abc123...tar","checksum":"sha256:def456...","size_bytes":5242880,"downloaded":true,"time":"2025-11-21T20:00:35Z"}
{"level":"info","msg":"starting unpack FSM","time":"2025-11-21T20:00:35Z"}
{"level":"info","msg":"unpack FSM completed","image_id":"img_abc123...","device_id":"abc12345","device_name":"thin-abc12345","device_path":"/dev/mapper/thin-abc12345","unpacked":true,"time":"2025-11-21T20:01:10Z"}
{"level":"info","msg":"starting activate FSM","time":"2025-11-21T20:01:10Z"}
{"level":"info","msg":"activate FSM completed","image_id":"img_abc123...","snapshot_id":"abc12345-snap","snapshot_name":"snap-img_abc123...","device_path":"/dev/mapper/thin-abc12345-snap","activated":true,"time":"2025-11-21T20:01:11Z"}

✓ Image processed successfully!
  Image ID:      img_abc123...
  Snapshot ID:   abc12345-snap
  Snapshot Name: snap-img_abc123...
  Device Path:   /dev/mapper/thin-abc12345-snap
```

**Example 2: Idempotent re-run (image already processed)**
```bash
# Run the same command again
sudo ./flyio-image-manager process-image \
  --s3-key "images/alpine-3.18.tar"
```

**Output** (completes in <1 second):
```json
{"level":"info","msg":"image already downloaded","time":"2025-11-21T20:02:00Z"}
{"level":"info","msg":"image already unpacked","time":"2025-11-21T20:02:00Z"}
{"level":"info","msg":"image already activated","time":"2025-11-21T20:02:00Z"}

✓ Image processed successfully!
  Image ID:      img_abc123...
  Snapshot ID:   abc12345-snap
  Snapshot Name: snap-img_abc123...
  Device Path:   /dev/mapper/thin-abc12345-snap
```

**Example 3: Debug mode**
```bash
sudo ./flyio-image-manager process-image \
  --s3-key "images/alpine-3.18.tar" \
  --log-level debug
```

---

### list-images

List all downloaded images with their status.

**Usage**:
```bash
./flyio-image-manager list-images [options]
```

**Optional Flags**:
- `--db`: Database path (default: `/var/lib/flyio/images.db`)
- `--log-level`: Set log verbosity

**Example**:
```bash
./flyio-image-manager list-images
```

**Output**:
```
Found 3 images:

Image ID:         img_abc123...
  S3 Key:         images/alpine-3.18.tar
  Local Path:     /var/lib/flyio/images/img_abc123...tar
  Size:           5242880 bytes
  Status:         completed
  Activation:     active
  Downloaded At:  2025-11-21T20:00:35Z

Image ID:         img_def456...
  S3 Key:         images/ubuntu-22.04.tar
  Local Path:     /var/lib/flyio/images/img_def456...tar
  Size:           10485760 bytes
  Status:         completed
  Activation:     active
  Downloaded At:  2025-11-21T19:45:12Z

Image ID:         img_ghi789...
  S3 Key:         images/nginx-latest.tar
  Local Path:     /var/lib/flyio/images/img_ghi789...tar
  Size:           7340032 bytes
  Status:         completed
  Activation:     inactive
  Downloaded At:  2025-11-21T19:30:00Z
```

---

### list-snapshots

List all active snapshots.

**Usage**:
```bash
./flyio-image-manager list-snapshots [options]
```

**Optional Flags**:
- `--db`: Database path
- `--log-level`: Set log verbosity

**Example**:
```bash
./flyio-image-manager list-snapshots
```

**Output**:
```
Found 2 active snapshots:

Snapshot ID:      abc12345-snap
  Image ID:       img_abc123...
  Snapshot Name:  snap-img_abc123...
  Device Path:    /dev/mapper/thin-abc12345-snap
  Active:         true
  Created At:     2025-11-21T20:01:11Z

Snapshot ID:      def45678-snap
  Image ID:       img_def456...
  Snapshot Name:  snap-img_def456...
  Device Path:    /dev/mapper/thin-def45678-snap
  Active:         true
  Created At:     2025-11-21T19:45:45Z
```

---

### daemon

Run the application as a background daemon with crash recovery support.

**Usage**:
```bash
sudo ./flyio-image-manager daemon [options]
```

**Optional Flags**:
- All configuration flags (see Configuration section)

**Example**:
```bash
# Run as daemon with custom configuration
sudo ./flyio-image-manager daemon \
  --bucket my-bucket \
  --region us-west-2 \
  --pool production-pool \
  --download-queue 10 \
  --log-level info
```

**Output**:
```json
{"level":"info","msg":"starting daemon","time":"2025-11-21T20:00:00Z"}
{"level":"info","msg":"resuming in-flight FSM runs","time":"2025-11-21T20:00:01Z"}
{"level":"info","msg":"daemon started successfully","time":"2025-11-21T20:00:02Z"}
```

**Graceful Shutdown**:
```bash
# Send SIGTERM or SIGINT
kill -TERM <pid>
# or press Ctrl+C

# Output:
{"level":"info","msg":"received shutdown signal","signal":"terminated","time":"2025-11-21T20:05:00Z"}
{"level":"info","msg":"shutting down gracefully...","time":"2025-11-21T20:05:00Z"}
{"level":"info","msg":"shutdown complete","time":"2025-11-21T20:05:02Z"}
```

---

### monitor

Launch an interactive TUI dashboard for live FSM tracking, system monitoring, and S3 image browsing.

**Usage**:
```bash
sudo ./flyio-image-manager monitor [options]
```

**Optional Flags**:
- `--db`: Database path (default: `/var/lib/flyio/images.db`)
- `--fsm-db`: FSM database directory (default: `/var/lib/flyio/fsm`)
- `--pool`: DeviceMapper pool name (default: `pool`)
- `--bucket`: S3 bucket name (default: `flyio-container-images`)
- `--region`: AWS region (default: `us-east-1`)
- `--log-level`: Set log verbosity
- `--inline`: Run in inline mode (non-fullscreen, for SSH sessions)

**Example**:
```bash
# Launch dashboard with default settings
sudo ./flyio-image-manager monitor

# Launch with custom paths
sudo ./flyio-image-manager monitor \
  --db /custom/path/images.db \
  --fsm-db /custom/path/fsm \
  --pool my-pool

# Launch in inline mode (for SSH)
sudo ./flyio-image-manager monitor --inline
```

**Dashboard Views**:

The monitor command provides a full-screen interactive TUI with multiple views:

**View 1: Monitor Dashboard** (Press `1`)
- **Active FSM Runs Panel** - Shows currently running FSMs (Download, Unpack, Activate) with:
  - FSM type and current state
  - Image ID being processed
  - Progress indicators for running operations
  - Error messages if any

- **System Status Panel** - Displays:
  - DeviceMapper pool usage (data and metadata)
  - Total images downloaded
  - Unpacked image count
  - Active snapshot count
  - System health status

- **Activity Log Panel** - Scrollable log of recent events with:
  - Timestamps
  - Log levels (info, warn, error)
  - Contextual messages

**View 2: S3 Image Browser** (Press `2`)
- Browse available container images from S3
- Displays for each image:
  - **Runtime type** (golang, node, python, etc.)
  - **Version number** extracted from filename
  - **File size** in human-readable format
  - **Last modified date**
  - **Status indicator** (✓ downloaded, ○ available)
- Press `Enter` to process selected image through full pipeline

**Keyboard Controls**:

| Key | Action |
|-----|--------|
| `1` | Switch to Monitor view |
| `2` | Switch to S3 Images view |
| `Tab` | Switch between panels (in Monitor view) |
| `j` / `↓` | Navigate down / Scroll logs down |
| `k` / `↑` | Navigate up / Scroll logs up |
| `Enter` | Process selected image (in S3 Images view) |
| `g` | Jump to top |
| `G` | Jump to bottom |
| `r` | Manual refresh |
| `q` / `Ctrl+C` | Quit dashboard |

**Connection Status**:

The title bar shows a connection indicator:
- 🟢 Green dot: Successfully connected to FSM admin socket
- 🔴 Red dot: Unable to connect (daemon may not be running)

**Example Dashboard View**:
```
⟳  Fly.io Image Manager Dashboard ●  Uptime: 5m23s                    [1] Monitor  [2] Images

╭─ Active FSM Runs ─────────────────╮  ╭─ System Status ────────────────╮
│ ⟳ download   img_abc123... running│  │ Pool Data: 1.2 GB / 10 GB (12%)│
│ ○ unpack     img_def456... pending│  │ Pool Meta: 128 KB / 1 MB (12%) │
│                                   │  │                                │
│                                   │  │ Total Images:     15           │
│                                   │  │ Unpacked:         12           │
│                                   │  │ Active Snapshots: 8            │
│                                   │  │ Health: ✓ OK                   │
╰───────────────────────────────────╯  ╰────────────────────────────────╯

╭─ Activity Log ────────────────────────────────────────────────────────╮
│ 10:15:32 info  Starting download for img_abc123...                    │
│ 10:15:30 info  Completed activation for img_xyz789...                 │
│ 10:15:28 info  Unpack completed for img_xyz789...                     │
│ 10:15:25 info  Download completed for img_xyz789...                   │
╰───────────────────────────────────────────────────────────────────────╯

1/2 views  •  j/k navigate  •  Enter process  •  r refresh  •  q quit
```

**Example S3 Images View**:
```
⟳  Fly.io Image Manager Dashboard ●                                   [1] Monitor  [2] Images

╭─ S3 Images ───────────────────────────────────────────────────────────╮
│  Runtime   Version   Size      Date         Status                    │
│ ─────────────────────────────────────────────────────────────────────│
│ > golang   2         47.4 MB   2025-11-20   ✓ downloaded              │
│   golang   3         52.1 MB   2025-11-20   ○ available               │
│   node     5         38.2 MB   2025-11-20   ✓ downloaded              │
│   python   1         65.8 MB   2025-11-20   ○ available               │
│   python   2         68.3 MB   2025-11-20   ○ available               │
╰───────────────────────────────────────────────────────────────────────╯

j/k navigate  •  Enter process selected image  •  r refresh  •  q quit
```

**Notes**:
- The dashboard auto-refreshes every second
- Works best with a terminal at least 80 columns wide
- Use `--inline` mode for SSH sessions to avoid terminal escape code issues
- Processing an image from the S3 browser runs the full pipeline (Download → Unpack → Activate)

---

### setup-pool

Create or recreate the devicemapper thin-pool. Essential after system reboots or kernel panics.

**Usage**:
```bash
sudo ./flyio-image-manager setup-pool [options]
```

**Optional Flags**:
- `--db`: Database path (pool files stored in same directory)
- `--pool`: DeviceMapper pool name (default: `pool`)
- `--log-level`: Set log verbosity

**Example 1: Create pool (first time or after reboot)**
```bash
sudo ./flyio-image-manager setup-pool --db /var/lib/flyio/images.db
```

**Output (new pool)**:
```json
{"level":"info","msg":"creating thin-pool","time":"2025-11-26T12:00:00Z"}
{"component":"pool-manager","msg":"creating new thin pool","pool_name":"pool","data_size":2147483648,"meta_size":1048576}
{"component":"pool-manager","msg":"metadata loop device created","device":"/dev/loop0"}
{"component":"pool-manager","msg":"data loop device created","device":"/dev/loop1"}
{"component":"pool-manager","msg":"thin pool created successfully"}
{"component":"pool-manager","msg":"pool verified successfully"}
Pool 'pool' created successfully.
Pool files created in: /var/lib/flyio
```

**Output (pool already exists)**:
```json
{"level":"info","msg":"pool already exists","pool_name":"pool","needs_check":false,"read_only":false}
Pool 'pool' is healthy and ready.
```

**Example 2: Recreate pool after kernel panic**
```bash
# First, clean up the corrupted pool (if it exists)
sudo dmsetup remove pool 2>/dev/null || true

# Create a fresh pool
sudo ./flyio-image-manager setup-pool --db /var/lib/flyio/images.db
```

**Note**: The pool uses loop devices backed by files. After a system reboot, the loop devices are lost and must be recreated using this command.

**See Also**: [Operations Guide - Pool Recovery](OPERATIONS.md#pool-recovery-after-kernel-panic) for detailed recovery procedures and pool architecture.

---

## Common Workflows

### Workflow 1: Process a Single Image

```bash
# Step 1: Verify devicemapper pool is active
sudo dmsetup status pool

# Step 2: Process the image
sudo ./flyio-image-manager process-image \
  --s3-key "images/alpine-3.18.tar"

# Step 3: Verify snapshot created
sudo dmsetup ls | grep thin

# Step 4: Mount and inspect the snapshot (optional)
sudo mkdir -p /mnt/test
sudo mount /dev/mapper/thin-abc12345-snap /mnt/test
ls -la /mnt/test
sudo umount /mnt/test
```

### Workflow 2: Process Multiple Images

```bash
# Create a script to process multiple images
cat > process-batch.sh <<'EOF'
#!/bin/bash
IMAGES=(
  "images/alpine-3.18.tar"
  "images/ubuntu-22.04.tar"
  "images/nginx-latest.tar"
)

for img in "${IMAGES[@]}"; do
  echo "Processing: $img"
  sudo ./flyio-image-manager process-image --s3-key "$img"
  echo "---"
done
EOF

chmod +x process-batch.sh
./process-batch.sh
```

### Workflow 3: Check Image Status

```bash
# List all images
./flyio-image-manager list-images

# List active snapshots
./flyio-image-manager list-snapshots

# Check database directly
sqlite3 /var/lib/flyio/images.db <<EOF
SELECT image_id, s3_key, download_status, activation_status 
FROM images 
ORDER BY created_at DESC;
EOF
```

### Workflow 4: Verify Idempotency

```bash
# Process an image
sudo ./flyio-image-manager process-image \
  --s3-key "images/test.tar"

# Re-process the same image (should be instant)
time sudo ./flyio-image-manager process-image \
  --s3-key "images/test.tar"

# Expected: Completes in <1 second with "already downloaded/unpacked/activated" messages
```

### Workflow 5: Test Crash Recovery

```bash
# Terminal 1: Start processing a large image
sudo ./flyio-image-manager process-image \
  --s3-key "images/large-image.tar" \
  --log-level debug

# Terminal 2: Kill the process mid-operation
ps aux | grep flyio-image-manager
sudo kill -9 <pid>

# Terminal 1: Restart the same command
sudo ./flyio-image-manager process-image \
  --s3-key "images/large-image.tar" \
  --log-level debug

# Expected: FSM resumes from last persisted state, completes successfully
```

---

## Database Inspection

### Useful SQL Queries

```bash
# Open database
sqlite3 /var/lib/flyio/images.db
```

**Query 1: List all images**
```sql
SELECT 
  image_id, 
  s3_key, 
  download_status, 
  activation_status,
  size_bytes,
  downloaded_at
FROM images
ORDER BY created_at DESC;
```

**Query 2: Find images ready for unpacking**
```sql
SELECT image_id, s3_key, local_path
FROM images
WHERE download_status = 'completed'
  AND image_id NOT IN (SELECT image_id FROM unpacked_images);
```

**Query 3: Find unpacked images ready for activation**
```sql
SELECT u.image_id, u.device_id, u.device_name
FROM unpacked_images u
LEFT JOIN snapshots s ON u.image_id = s.image_id
WHERE u.layout_verified = 1
  AND s.snapshot_id IS NULL;
```

**Query 4: Check active snapshots**
```sql
SELECT 
  s.snapshot_id,
  s.snapshot_name,
  s.device_path,
  i.s3_key,
  s.created_at
FROM snapshots s
JOIN images i ON s.image_id = i.image_id
WHERE s.active = 1
ORDER BY s.created_at DESC;
```

**Query 5: Database statistics**
```sql
SELECT 
  (SELECT COUNT(*) FROM images) AS total_images,
  (SELECT COUNT(*) FROM images WHERE download_status = 'completed') AS downloaded,
  (SELECT COUNT(*) FROM unpacked_images) AS unpacked,
  (SELECT COUNT(*) FROM snapshots WHERE active = 1) AS active_snapshots;
```

### Database Schema

**images table**:
```sql
CREATE TABLE images (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    image_id TEXT NOT NULL UNIQUE,
    s3_key TEXT NOT NULL UNIQUE,
    local_path TEXT NOT NULL,
    checksum TEXT,
    size_bytes INTEGER NOT NULL,
    download_status TEXT NOT NULL DEFAULT 'pending',
    activation_status TEXT DEFAULT 'inactive',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    downloaded_at DATETIME,
    activated_at DATETIME,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

**unpacked_images table**:
```sql
CREATE TABLE unpacked_images (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    image_id TEXT NOT NULL UNIQUE,
    device_id TEXT NOT NULL,
    device_name TEXT NOT NULL,
    device_path TEXT NOT NULL,
    size_bytes INTEGER,
    file_count INTEGER,
    layout_verified INTEGER DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    unpacked_at DATETIME,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (image_id) REFERENCES images(image_id)
);
```

**snapshots table**:
```sql
CREATE TABLE snapshots (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    image_id TEXT NOT NULL,
    snapshot_id TEXT NOT NULL UNIQUE,
    snapshot_name TEXT NOT NULL,
    device_path TEXT NOT NULL,
    origin_device_id TEXT NOT NULL,
    active INTEGER DEFAULT 1,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deactivated_at DATETIME,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (image_id) REFERENCES images(image_id)
);
```

---

## Troubleshooting

### Issue: "dmsetup: device-mapper: create ioctl on pool failed: Device or resource busy"

**Cause**: Another process is using the devicemapper pool or loop devices.

**Solution**:
```bash
# Check what's using devicemapper
sudo dmsetup ls
sudo dmsetup status pool

# If pool exists but stuck, remove it
sudo dmsetup remove pool

# Detach loop devices
sudo losetup -D

# Recreate pool (see Setup DeviceMapper Pool section)
```

### Issue: "failed to open database: unable to open database file"

**Cause**: Database file permissions or directory doesn't exist.

**Solution**:
```bash
# Create directory
sudo mkdir -p /var/lib/flyio

# Check permissions
ls -la /var/lib/flyio/images.db

# Fix ownership (if needed)
sudo chown $USER:$USER /var/lib/flyio/images.db
```

### Issue: "NoCredentialProviders: no valid providers in chain"

**Cause**: AWS credentials not configured.

**Solution**:
```bash
# Set environment variables
export AWS_ACCESS_KEY_ID="your-key"
export AWS_SECRET_ACCESS_KEY="your-secret"
export AWS_REGION="us-east-1"

# Or configure AWS CLI
aws configure
```

### Issue: "failed to mount device: mount: /mnt/flyio/...: can't find in /etc/fstab"

**Cause**: Mount point doesn't exist.

**Solution**:
```bash
# Create mount root
sudo mkdir -p /mnt/flyio

# Verify permissions
ls -la /mnt/flyio
```

### Issue: Image processing hangs during extraction

**Cause**: Extraction timeout (default 30 minutes) or disk I/O issues.

**Solution**:
```bash
# Check disk space
df -h /var/lib/flyio

# Check I/O wait
iostat -x 1

# If tar archive is corrupted, it will abort automatically
# Check logs for security violations or tar errors
```

### Issue: "devicemapper pool full"

**Cause**: Thin pool has no free space.

**Solution**:
```bash
# Check pool status
sudo dmsetup status pool

# Extend pool (create larger backing file)
# WARNING: This requires pool recreation
sudo dmsetup remove pool
fallocate -l 10G pool_data  # Increase from 2GB to 10GB
# Then recreate pool with new size
```

---

## Performance Tuning

### Concurrent Operations

```bash
# Increase concurrent downloads (default: 5)
sudo ./flyio-image-manager daemon --download-queue 10

# Increase concurrent unpacking (default: 2, I/O-bound)
sudo ./flyio-image-manager daemon --unpack-queue 4
```

**Note**: Unpacking is I/O-bound. Increasing beyond 2-3 may not improve performance and could cause disk contention.

### Database Optimization

The database is already optimized with:
- WAL mode for concurrent reads
- Proper indexes on frequently queried columns
- Connection pooling (10 max open, 5 max idle)

No tuning typically needed.

### DeviceMapper Pool Sizing

```bash
# Calculate metadata size: ~1MB per 100 thin devices
# For 1000 images: 10MB metadata
METADATA_SIZE="10M"

# Calculate data size: Total image storage needed
# For 50 images at 200MB average: 10GB data
DATA_SIZE="10G"

fallocate -l $METADATA_SIZE pool_meta
fallocate -l $DATA_SIZE pool_data
```

---

## Monitoring

### Log Files

```bash
# View structured logs (JSON format)
journalctl -u flyio-image-manager -f

# Filter by level
journalctl -u flyio-image-manager | grep '"level":"error"'

# View specific image processing
journalctl -u flyio-image-manager | grep 'img_abc123'
```

### Health Checks & Safeguards

The system includes comprehensive safeguards to prevent kernel panics and ensure system stability when working with devicemapper thin-pools.

**For detailed safeguard architecture, see [Operations Guide - System Safeguards](OPERATIONS.md#system-safeguards).**

#### Automatic Health Checks

Health checks run automatically before `process-image` and other dm-heavy operations:

| Check | Description | Failure Action |
|-------|-------------|----------------|
| **Pool Existence** | Verifies thin-pool exists | Auto-creates pool if missing |
| **Pool Health** | Checks for `needs_check`, read-only, error states | Blocks operation |
| **D-state Detection** | Finds processes in uninterruptible sleep | Blocks operation |
| **Kernel Logs** | Scans dmesg for BUG, panic, OOM errors | Blocks on critical errors |
| **Memory Pressure** | Checks available memory > 5% | Blocks operation |
| **I/O Wait** | Checks I/O wait < 50% | Blocks operation |

#### Manual Health Checks

```bash
# Check devicemapper pool health
sudo dmsetup status pool

# Check for D-state processes
ps aux | awk '$8 ~ /D/ {print}'

# Check kernel logs for dm errors
sudo dmesg | grep -i "device-mapper\|dm-thin" | tail -20

# Check memory pressure
free -h
```

#### Health Check Failure Recovery

If health checks fail, see [Troubleshooting Guide](TROUBLESHOOTING.md#issue-pool-does-not-exist) for recovery procedures.

### Metrics

Built-in Prometheus metrics (if enabled):
- FSM transition durations
- FSM success/failure rates
- Queue depths
- Active runs

**Note**: Metrics exposition endpoint not implemented yet.

---

## Best Practices

### 1. Run as systemd service

```bash
# Create systemd service file
sudo tee /etc/systemd/system/flyio-image-manager.service > /dev/null <<EOF
[Unit]
Description=Fly.io Container Image Management System
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/flyio
ExecStart=/opt/flyio/flyio-image-manager daemon --log-level info
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
EOF

# Enable and start service
sudo systemctl enable flyio-image-manager
sudo systemctl start flyio-image-manager

# Check status
sudo systemctl status flyio-image-manager
```

### 2. Regular backups

```bash
# Backup database
sqlite3 /var/lib/flyio/images.db ".backup /backup/images-$(date +%Y%m%d).db"

# Backup FSM state
tar -czf /backup/fsm-state-$(date +%Y%m%d).tar.gz /var/lib/flyio/fsm/
```

### 3. Monitor disk space

```bash
# Set up disk space monitoring
df -h /var/lib/flyio | awk 'NR==2 {if ($5+0 > 80) print "WARNING: Disk usage " $5}'
```

### 4. Cleanup old images

For garbage collection and orphaned device cleanup, see [Operations Guide - Orphaned Device Management](OPERATIONS.md#orphaned-device-management).

Quick reference:

```bash
# Preview orphaned devices (dry run)
flyio-image-manager gc --dry-run

# Clean up orphaned devices (requires --force)
flyio-image-manager gc --force

# List old images in database
sqlite3 /var/lib/flyio/images.db "SELECT image_id, s3_key, downloaded_at FROM images WHERE downloaded_at < datetime('now', '-30 days');"

# Manual cleanup process (future enhancement)
# 1. Deactivate snapshots
# 2. Delete devices
# 3. Remove files
# 4. Update database
```

---

## Security Considerations

### 1. Run with minimum privileges

While devicemapper requires root, consider:
- Using capabilities instead of full root (CAP_SYS_ADMIN, CAP_MKNOD)
- Running in a container with limited access
- Using SELinux/AppArmor policies

### 2. Validate S3 sources

```bash
# Only process images from trusted S3 buckets
# The system validates tar structure and checks for:
# - Path traversal attempts
# - Symlink attacks
# - Oversized files
# - Malicious permissions
```

### 3. Network security

```bash
# Restrict S3 access to specific bucket
# Use VPC endpoints for S3 (no internet access needed)
# Monitor network traffic for anomalies
```

---

## References

- [Quick Start Guide](QUICKSTART.md) - Initial setup
- [Development Guide](DEVELOPMENT.md) - Building and testing
- [Troubleshooting Guide](TROUBLESHOOTING.md) - Detailed error resolution
- [System Architecture](../design/SYSTEM_ARCH.md) - How it works
- [Database Schema](../api/DATABASE.md) - Complete schema reference
- [API Interfaces](../api/INTERFACES.md) - Request/Response types
- [Durable State Contracts](../design/DURABLE_STATE_CONTRACTS.md) - Crash recovery behavior

---

## Getting Help

### Collect Diagnostic Information

```bash
#!/bin/bash
# Save as: collect-diagnostics.sh

echo "=== System Information ==="
uname -a
go version

echo -e "\n=== DeviceMapper Status ==="
sudo dmsetup ls
sudo dmsetup status pool

echo -e "\n=== Database Status ==="
sqlite3 /var/lib/flyio/images.db "SELECT COUNT(*) FROM images;"
sqlite3 /var/lib/flyio/images.db "SELECT COUNT(*) FROM snapshots WHERE active = 1;"

echo -e "\n=== Disk Space ==="
df -h /var/lib/flyio
df -h /mnt/flyio

echo -e "\n=== Recent Logs ==="
journalctl -u flyio-image-manager --since "1 hour ago" | tail -100

echo -e "\n=== Process Status ==="
ps aux | grep flyio-image-manager
```

### Support Resources

When reporting issues, include:
1. Output of diagnostic script above
2. Specific error messages with full context
3. Steps to reproduce the issue
4. S3 key and image ID (if relevant)
5. Database queries showing relevant state

---

## Appendix: Example Session

Complete example of processing an image from start to finish:

```bash
# 1. Verify prerequisites
$ go version
go version go1.23.0 linux/amd64

$ sudo dmsetup status pool
pool: 0 4194304 thin-pool 100 200/1000 50/100000 - rw discard_passdown queue_if_no_space -

$ aws s3 ls s3://flyio-container-images/images/ | head -3
2025-11-20 10:00:00    5242880 alpine-3.18.tar
2025-11-20 10:00:00   10485760 ubuntu-22.04.tar
2025-11-20 10:00:00    7340032 nginx-latest.tar

# 2. Process image
$ sudo ./flyio-image-manager process-image --s3-key "images/alpine-3.18.tar"
[... JSON logs ...]
✓ Image processed successfully!
  Image ID:      img_f3e4d5c6b7a8...
  Snapshot ID:   a1b2c3d4-snap
  Snapshot Name: snap-img_f3e4d5c6b7a8...
  Device Path:   /dev/mapper/thin-a1b2c3d4-snap

# 3. Verify in database
$ sqlite3 /var/lib/flyio/images.db "SELECT image_id, download_status, activation_status FROM images;"
img_f3e4d5c6b7a8...|completed|active

# 4. Mount and inspect snapshot
$ sudo mkdir -p /mnt/test
$ sudo mount /dev/mapper/thin-a1b2c3d4-snap /mnt/test
$ ls /mnt/test
bin  dev  etc  home  lib  media  mnt  opt  proc  root  run  sbin  srv  sys  tmp  usr  var

$ sudo umount /mnt/test

# 5. Re-run to verify idempotency
$ time sudo ./flyio-image-manager process-image --s3-key "images/alpine-3.18.tar"
[... logs showing "already downloaded/unpacked/activated" ...]
✓ Image processed successfully!

real    0m0.542s  # <1 second!
user    0m0.123s
sys     0m0.089s

# Success! Image is processed and ready to use.
```
