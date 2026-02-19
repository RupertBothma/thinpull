#!/bin/bash
# Setup test environment for integration testing
set -e

echo "=== Fly.io Image Manager: Setup Test Environment ==="
echo ""

# Check root
if [[ $EUID -ne 0 ]]; then
    echo "❌ ERROR: Root access required"
    echo "   Run with: sudo $0"
    exit 1
fi

# Install required packages if missing
echo "Checking required packages..."
PACKAGES_TO_INSTALL=""

# Check for SQLite CLI (for debugging/verification)
if ! command -v sqlite3 &>/dev/null; then
    PACKAGES_TO_INSTALL="$PACKAGES_TO_INSTALL sqlite3"
fi

# Check for DeviceMapper tools
if ! command -v dmsetup &>/dev/null; then
    PACKAGES_TO_INSTALL="$PACKAGES_TO_INSTALL lvm2"
fi

# Check for thin-provisioning tools
if ! command -v thin_check &>/dev/null; then
    PACKAGES_TO_INSTALL="$PACKAGES_TO_INSTALL thin-provisioning-tools"
fi

# Install missing packages
if [[ -n "$PACKAGES_TO_INSTALL" ]]; then
    echo "Installing missing packages:$PACKAGES_TO_INSTALL"
    apt-get update -qq
    apt-get install -y $PACKAGES_TO_INSTALL
    echo "✓ Packages installed"
else
    echo "✓ All required packages already installed"
fi

# Create directories
echo "Creating directories..."
mkdir -p /var/lib/flyio/{images,fsm}
mkdir -p /mnt/flyio
mkdir -p /tmp/flyio-test
echo "✓ Directories created"

# Build application
echo ""
echo "Building application..."
cd "$(dirname "$0")/.."
go build -o flyio-image-manager ./cmd/flyio-image-manager
chmod +x flyio-image-manager
echo "✓ Application built: $(pwd)/flyio-image-manager"

# Cleanup any existing devicemapper setup
echo ""
echo "Cleaning up existing devicemapper setup..."
for dev in $(dmsetup ls 2>/dev/null | grep -E "(thin-|snap-)" | awk '{print $1}'); do
    echo "  Removing device: $dev"
    dmsetup remove "$dev" 2>/dev/null || dmsetup remove --force "$dev" || true
done

if dmsetup ls 2>/dev/null | grep -q "^pool"; then
    echo "  Removing existing pool"
    if ! dmsetup remove pool 2>/dev/null; then
        echo "  Pool busy, trying force remove..."
        if ! dmsetup remove --force pool; then
            echo "❌ ERROR: Failed to remove pool. Cannot proceed safely."
            echo "   Please check for active processes: ps aux | grep dmsetup"
            exit 1
        fi
    fi
fi

# Verify pool is gone before proceeding
if dmsetup ls 2>/dev/null | grep -q "^pool"; then
    echo "❌ ERROR: Pool still exists. Aborting to prevent kernel deadlock."
    exit 1
fi

losetup -D 2>/dev/null || true
echo "✓ Cleanup complete"

# Create devicemapper pool
echo ""
echo "Creating devicemapper pool..."
cd /var/lib/flyio

# Remove old pool files if they exist
rm -f pool_meta pool_data

# Create pool files with optimized sizes
# - 4MB metadata (0.2% of data size, recommended for thin pools)
# - 2GB data (sufficient for 3-4 large images ~500MB each)
echo "  Allocating pool_meta (4MB)..."
fallocate -l 4M pool_meta

echo "  Allocating pool_data (2GB)..."
fallocate -l 2G pool_data

# Setup loop devices
echo "  Setting up loop devices..."
METADATA_DEV=$(losetup -f --show pool_meta)
DATA_DEV=$(losetup -f --show pool_data)
echo "  Metadata device: $METADATA_DEV"
echo "  Data device: $DATA_DEV"

# Create thin pool with optimized configuration
# Table format: "0 <data_size_sectors> thin-pool <metadata_dev> <data_dev> <data_block_size> <low_water_mark>"
# 2GB = 2 * 1024 * 1024 * 1024 bytes = 2147483648 bytes
# Sector size = 512 bytes
# Sectors = 2147483648 / 512 = 4194304
# Block size: 256 sectors (128KB) - optimal for container images
# Low water mark: 65536 sectors (32MB)
echo "  Creating thin pool..."
dmsetup create --verifyudev pool --table "0 4194304 thin-pool $METADATA_DEV $DATA_DEV 256 65536"

# Verify pool creation
if dmsetup status pool &>/dev/null; then
    echo "✓ Pool created successfully"
    dmsetup status pool
else
    echo "❌ ERROR: Pool creation failed"
    exit 1
fi

echo ""
echo "=== Setup Complete ==="
echo ""
echo "Test environment ready:"
echo "  • Application: $(dirname "$0")/../flyio-image-manager"
echo "  • Database: /var/lib/flyio/images.db"
echo "  • FSM state: /var/lib/flyio/fsm/"
echo "  • Pool: $(dmsetup status pool | awk '{print $6, $7}')"
echo ""
echo "Next: Run the application with ./flyio-image-manager process-image --s3-key images/<name>.tar"
