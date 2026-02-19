#!/bin/bash
# create-dynamic-pool.sh - Create devicemapper thin pool with dynamic sizing
#
# This script analyzes the S3 bucket to determine appropriate pool sizes
# and creates a thin pool with optimal configuration.
#
# Usage: sudo ./create-dynamic-pool.sh [options]
#   --dry-run    Show recommended sizes without creating pool
#   --help       Show this help message
#
# Pool sizing algorithm:
# - Data size: (Total S3 image size × 2) + 30% overhead for snapshots and CoW
# - Metadata size: 0.2% of data size (minimum 4MB, industry best practice)
# - Block size: 256 sectors (128KB) - optimal for container images
#   CRITICAL: Do NOT use 2048 sectors (1MB) - causes severe I/O degradation
# - Low water mark: ~1% of pool size (triggers space warnings)

set -e

# Configuration
S3_BUCKET="s3://flyio-container-images/images"
POOL_DIR="/var/lib/flyio"
POOL_NAME="pool"

# Parse arguments
DRY_RUN=false
for arg in "$@"; do
    case $arg in
        --dry-run)
            DRY_RUN=true
            ;;
        --help)
            head -20 "$0" | tail -n +2 | sed 's/^# //'
            exit 0
            ;;
    esac
done

echo "================================================================"
echo "Dynamic Thin Pool Creator"
echo "================================================================"
echo ""

# Check if we can access S3 (optional - will use defaults if not)
echo "Step 1: Analyzing S3 bucket..."
TOTAL_SIZE=0
IMAGE_COUNT=0

if command -v aws &>/dev/null; then
    # Try to get actual S3 bucket size
    while IFS= read -r line; do
        size=$(echo "$line" | awk '{print $1}')
        if [[ "$size" =~ ^[0-9]+$ ]]; then
            TOTAL_SIZE=$((TOTAL_SIZE + size))
            IMAGE_COUNT=$((IMAGE_COUNT + 1))
        fi
    done < <(aws s3 ls "$S3_BUCKET" --recursive 2>/dev/null | grep '\.tar$' || true)
fi

if [ "$TOTAL_SIZE" -eq 0 ]; then
    echo "  Could not analyze S3 bucket (AWS CLI not configured or bucket not accessible)"
    echo "  Using default sizing for ~500MB workload..."
    TOTAL_SIZE=$((500 * 1024 * 1024))  # 500MB default
    IMAGE_COUNT=5
else
    echo "  Found $IMAGE_COUNT images totaling $(numfmt --to=iec $TOTAL_SIZE 2>/dev/null || echo "${TOTAL_SIZE} bytes")"
fi

echo ""
echo "Step 2: Calculating pool sizes..."

# Calculate recommended sizes
# Data: (total × 2) + 30% = total × 2.6, rounded up to nearest GB
DATA_SIZE_BYTES=$((TOTAL_SIZE * 26 / 10))
DATA_SIZE_GB=$(( (DATA_SIZE_BYTES + 1073741823) / 1073741824 ))  # Round up
if [ "$DATA_SIZE_GB" -lt 2 ]; then
    DATA_SIZE_GB=2  # Minimum 2GB
fi
DATA_SIZE_BYTES=$((DATA_SIZE_GB * 1073741824))
DATA_SIZE_SECTORS=$((DATA_SIZE_BYTES / 512))

# Metadata: 0.2% of data size (minimum 4MB)
META_SIZE_BYTES=$((DATA_SIZE_BYTES / 500))
if [ "$META_SIZE_BYTES" -lt 4194304 ]; then
    META_SIZE_BYTES=4194304  # 4MB minimum
fi
META_SIZE_MB=$((META_SIZE_BYTES / 1048576))

# Low water mark: ~1% of data size in sectors
LOW_WATER_MARK=$((DATA_SIZE_SECTORS / 100))
if [ "$LOW_WATER_MARK" -lt 65536 ]; then
    LOW_WATER_MARK=65536  # Minimum 32MB
fi

# Block size: ALWAYS 256 sectors (128KB)
# CRITICAL: Do NOT use 2048 (1MB blocks) - causes severe I/O slowdown
BLOCK_SIZE=256

echo ""
echo "  Recommended Configuration:"
echo "  ─────────────────────────────────────────────────────────────"
echo "  Data device:      ${DATA_SIZE_GB}GB ($(numfmt --to=iec $DATA_SIZE_BYTES 2>/dev/null || echo "$DATA_SIZE_BYTES bytes"))"
echo "  Metadata device:  ${META_SIZE_MB}MB"
echo "  Table size:       ${DATA_SIZE_SECTORS} sectors"
echo "  Block size:       ${BLOCK_SIZE} sectors (128KB) - OPTIMAL"
echo "  Low water mark:   ${LOW_WATER_MARK} sectors"
echo ""
echo "  Pool sizing rationale:"
echo "  • Metadata: 0.2% of data size (thin pool best practice, min 4MB)"
echo "  • Data: S3 bucket size × 2 + 30% overhead for snapshots"
echo "  • Block size: 256 sectors (128KB) optimal for container images"
echo "    WARNING: 1MB blocks (2048 sectors) cause severe I/O slowdown!"
echo "  • Low water mark: ~1% of pool size for timely space warnings"
echo ""

if [ "$DRY_RUN" = true ]; then
    echo "DRY RUN - No changes made"
    echo ""
    echo "To create pool manually, run:"
    echo ""
    echo "  cd $POOL_DIR"
    echo "  fallocate -l ${META_SIZE_MB}M pool_meta"
    echo "  fallocate -l ${DATA_SIZE_GB}G pool_data"
    echo "  METADATA_DEV=\$(losetup -f --show pool_meta)"
    echo "  DATA_DEV=\$(losetup -f --show pool_data)"
    echo "  dmsetup create --verifyudev $POOL_NAME --table \"0 $DATA_SIZE_SECTORS thin-pool \$METADATA_DEV \$DATA_DEV $BLOCK_SIZE $LOW_WATER_MARK\""
    exit 0
fi

# Check for root
if [ "$(id -u)" -ne 0 ]; then
    echo "ERROR: This script must be run as root (sudo)"
    exit 1
fi

echo "Step 3: Creating pool..."

# Cleanup existing pool
echo "  Removing existing pool (if any)..."
dmsetup remove "$POOL_NAME" 2>/dev/null || true
losetup -D 2>/dev/null || true
rm -f "$POOL_DIR/pool_meta" "$POOL_DIR/pool_data" 2>/dev/null || true

# Create directory
mkdir -p "$POOL_DIR"
cd "$POOL_DIR"

# Create backing files
echo "  Creating backing files..."
fallocate -l "${META_SIZE_MB}M" pool_meta
fallocate -l "${DATA_SIZE_GB}G" pool_data

# Create loop devices
echo "  Setting up loop devices..."
METADATA_DEV=$(losetup -f --show pool_meta)
DATA_DEV=$(losetup -f --show pool_data)

echo "    Metadata: $METADATA_DEV"
echo "    Data: $DATA_DEV"

# Create thin pool
echo "  Creating thin pool..."
dmsetup create --verifyudev "$POOL_NAME" --table "0 $DATA_SIZE_SECTORS thin-pool $METADATA_DEV $DATA_DEV $BLOCK_SIZE $LOW_WATER_MARK"

# Verify
echo ""
echo "Step 4: Verification"
echo "  ─────────────────────────────────────────────────────────────"

if dmsetup status "$POOL_NAME" &>/dev/null; then
    echo "  ✓ Pool '$POOL_NAME' created successfully"
    dmsetup status "$POOL_NAME"
else
    echo "  ✗ ERROR: Pool creation failed"
    exit 1
fi

echo ""
echo "================================================================"
echo "Pool ready for use"
echo "================================================================"
