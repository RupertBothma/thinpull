#!/bin/bash
set -e

echo "=== DeviceMapper Complete Reset Script ==="
echo ""

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

info() {
    echo -e "${BLUE}ℹ${NC} $1"
}

success() {
    echo -e "${GREEN}✓${NC} $1"
}

warn() {
    echo -e "${YELLOW}⚠${NC} $1"
}

error() {
    echo -e "${RED}✗${NC} $1"
}

# Step 1: Kill any hanging processes
info "Step 1: Killing any hanging dmsetup/mount processes"
sudo pkill -9 dmsetup 2>/dev/null || true
sudo pkill -9 mount 2>/dev/null || true
sleep 1
success "Processes killed"

# Step 2: Skip unmounting (causes kernel-level D-state hangs)
info "Step 2: Skipping unmount (prevents kernel panic)"
warn "Devices will remain mounted - this is intentional to prevent kernel hangs"
# CRITICAL: Unmount operations cause kernel-level D-state hangs that lead to kernel panic.
# We intentionally skip unmounting and let dmsetup remove handle cleanup.
# The kernel will automatically release mounts when devices are removed.
success "Unmount skipped (kernel panic prevention)"

# Step 3: Remove all thin devices (with timeout protection)
info "Step 3: Removing all thin devices"
REMOVED_COUNT=0

# Remove all thin-* and snap-* devices by querying dmsetup
for device in $(dmsetup ls 2>/dev/null | grep -E "^(thin[-_]|snap-)" | awk '{print $1}'); do
    if dmsetup info "$device" &>/dev/null; then
        warn "Removing device $device"
        if timeout 15 sudo dmsetup remove --verifyudev "$device" 2>/dev/null; then
            REMOVED_COUNT=$((REMOVED_COUNT + 1))
        else
            warn "Normal removal failed, trying force"
            timeout 10 sudo dmsetup remove --force "$device" 2>/dev/null || true
        fi
        sleep 0.5
    fi
done

# Also find any orphaned devices referencing the pool
for device in $(dmsetup ls 2>/dev/null | awk '{print $1}' | grep -v "^pool$"); do
    table=$(dmsetup table "$device" 2>/dev/null || true)
    if echo "$table" | grep -qE "thin|/dev/mapper/pool"; then
        warn "Removing orphaned pool-dependent device: $device"
        timeout 10 sudo dmsetup remove --verifyudev "$device" 2>/dev/null || \
        timeout 10 sudo dmsetup remove --force "$device" 2>/dev/null || true
        REMOVED_COUNT=$((REMOVED_COUNT + 1))
        sleep 0.5
    fi
done

if [[ $REMOVED_COUNT -gt 0 ]]; then
    success "Removed $REMOVED_COUNT thin device(s)"
else
    info "No thin devices found"
fi

# Step 4: Remove the pool (with timeout protection)
info "Step 4: Removing devicemapper pool"
if timeout 5 sudo dmsetup status pool &>/dev/null; then
    timeout 10 sudo dmsetup remove pool 2>/dev/null || {
        warn "Pool removal timed out, forcing suspension"
        timeout 5 sudo dmsetup suspend pool 2>/dev/null || true
        timeout 10 sudo dmsetup remove pool 2>/dev/null || true
    }
    success "Pool removed"
else
    warn "Pool already gone or hung (skipping)"
fi

# Step 5: Detach loop devices
info "Step 5: Detaching loop devices"
for loop_dev in $(losetup -a | grep -E 'pool_meta|pool_data' | cut -d: -f1); do
    warn "Detaching $loop_dev"
    sudo losetup -d "$loop_dev" 2>/dev/null || true
done
success "Loop devices detached"

# Step 6: Remove old pool files
info "Step 6: Removing old pool files"
sudo rm -f pool_meta pool_data
success "Pool files removed"

# Step 6b: Clean FSM state (critical for subsequent runs)
info "Step 6b: Cleaning FSM state"
FSM_DIR="/var/lib/flyio/fsm"
if [[ -f "$FSM_DIR/flyio-manager.lock" ]]; then
    sudo rm -f "$FSM_DIR/flyio-manager.lock"
    success "FSM lock file removed"
fi
if [[ -S "$FSM_DIR/fsm.sock" ]]; then
    sudo rm -f "$FSM_DIR/fsm.sock"
    success "FSM socket removed"
fi
# Optionally clean FSM databases
if [[ -f "$FSM_DIR/fsm-state.db" ]]; then
    sudo rm -f "$FSM_DIR/fsm-state.db" "$FSM_DIR/fsm-history.db"
    success "FSM databases removed"
fi

# Step 7: Create new pool files with optimized configuration
info "Step 7: Creating new pool files"
echo "  - Metadata: 4MB (0.2% of data size)"
echo "  - Data: 2GB (sufficient for 3-4 large images)"
fallocate -l 4M pool_meta
fallocate -l 2G pool_data
success "Pool files created"

# Step 8: Attach loop devices
info "Step 8: Attaching loop devices"
METADATA_DEV="$(losetup -f --show pool_meta)"
DATA_DEV="$(losetup -f --show pool_data)"
echo "  - Metadata device: $METADATA_DEV"
echo "  - Data device: $DATA_DEV"
success "Loop devices attached"

# Step 9: Create thin pool with optimized settings
info "Step 9: Creating thin pool with optimized configuration"
echo "  - Block size: 256 sectors (128KB) - optimal for container images"
echo "  - Low water mark: 65536 sectors (32MB)"
sudo dmsetup create --verifyudev pool --table "0 4194304 thin-pool ${METADATA_DEV} ${DATA_DEV} 256 65536"
success "Thin pool created"

# Step 10: Verify pool status
info "Step 10: Verifying pool status"
if timeout 5 sudo dmsetup status pool &>/dev/null; then
    sudo dmsetup status pool
    success "Pool is operational"
else
    error "Pool status check failed"
    exit 1
fi

# Step 11: Ensure mount point directory exists
info "Step 11: Ensuring mount point directory exists"
sudo mkdir -p /mnt/flyio
sudo chmod 755 /mnt/flyio
success "Mount point directory ready"

echo ""
echo -e "${GREEN}=== DeviceMapper Reset Complete ===${NC}"
echo ""
echo "Pool configuration:"
sudo dmsetup table pool
echo ""
echo "Pool status:"
sudo dmsetup status pool

