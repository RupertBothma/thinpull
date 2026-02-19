#!/bin/bash
# Cleanup test environment
#
# Usage: sudo ./test-scripts/cleanup-test-environment.sh [--all] [--force]
#
# Options:
#   --all    Remove all state without prompting
#   --force  Force unmount and device removal

echo "=== Fly.io Image Manager: Cleanup Test Environment ==="
echo ""

REMOVE_ALL=false
FORCE=false

# Parse args
while [[ $# -gt 0 ]]; do
    case $1 in
        --all)   REMOVE_ALL=true; shift ;;
        --force) FORCE=true; shift ;;
        *)       echo "Unknown option: $1"; exit 1 ;;
    esac
done

# Check root
if [[ $EUID -ne 0 ]]; then
    echo "❌ ERROR: Root access required"
    echo "   Run with: sudo $0"
    exit 1
fi

# Check for D-state processes (kernel deadlock indicator)
echo "Checking for kernel deadlock..."
if ps aux 2>/dev/null | awk '$8 ~ /D/' | grep -E 'dmsetup|umount' | grep -v grep > /dev/null; then
    echo "❌ ERROR: Kernel deadlock detected (processes in D state)"
    echo "   Cannot proceed safely. System reboot required."
    ps aux 2>/dev/null | awk '$8 ~ /D/' | grep -E 'dmsetup|umount' | grep -v grep
    exit 1
fi
echo "✓ No kernel deadlock detected"

# Check for running flyio processes
PIDS=$(pgrep -f 'flyio-image-manager' 2>/dev/null || true)
if [[ -n "$PIDS" ]]; then
    echo "⚠️  Found running flyio-image-manager processes: $PIDS"
    if [[ $FORCE != true ]]; then
        echo "❌ ERROR: Cannot cleanup while flyio-image-manager is running"
        exit 1
    fi
fi

# Unmount any mounted devices first
echo "Unmounting devices..."
if grep -q "/mnt/flyio" /proc/mounts 2>/dev/null; then
    grep "/mnt/flyio" /proc/mounts | awk '{print $2}' | while read -r mountpoint; do
        echo "  Unmounting: $mountpoint"
        timeout 5 umount "$mountpoint" 2>/dev/null || \
        timeout 5 umount -f "$mountpoint" 2>/dev/null || \
        umount -l "$mountpoint"
        sleep 0.5
    done
    # Verify unmount completed
    sleep 1
    if grep -q "/mnt/flyio" /proc/mounts 2>/dev/null; then
        echo "⚠️  Warning: Some devices still mounted"
        grep "/mnt/flyio" /proc/mounts
    else
        echo "✓ All devices unmounted"
    fi
else
    echo "  No mounted devices found"
fi

# Remove all thin devices (including any with non-standard naming)
echo ""
echo "Removing thin devices..."
DEVICE_COUNT=0

# First, find all devices by name pattern (thin-* snap-*)
for dev in $(dmsetup ls 2>/dev/null | grep -E "^(thin[-_]|snap-)" | awk '{print $1}'); do
    echo "  Removing: $dev"
    if timeout 15 dmsetup remove --verifyudev "$dev" 2>/dev/null; then
        DEVICE_COUNT=$((DEVICE_COUNT + 1))
    elif [[ $FORCE == true ]] && timeout 10 dmsetup remove --force "$dev" 2>/dev/null; then
        DEVICE_COUNT=$((DEVICE_COUNT + 1))
    else
        echo "  ⚠️  Failed to remove $dev"
    fi
    sleep 0.5
done

# Also check for any orphaned devices referencing the pool in their table
for dev in $(dmsetup ls 2>/dev/null | awk '{print $1}' | grep -v "^pool$"); do
    table=$(dmsetup table "$dev" 2>/dev/null || true)
    if echo "$table" | grep -qE "thin|/dev/mapper/pool"; then
        echo "  Removing pool-dependent: $dev"
        timeout 10 dmsetup remove --verifyudev "$dev" 2>/dev/null || \
        timeout 10 dmsetup remove --force "$dev" 2>/dev/null || true
        DEVICE_COUNT=$((DEVICE_COUNT + 1))
        sleep 0.5
    fi
done

# Verify all thin devices removed
sleep 1
REMAINING=$(dmsetup ls 2>/dev/null | grep -E "^(thin[-_]|snap-)" | wc -l)
if [[ $REMAINING -gt 0 ]]; then
    echo "⚠️  Warning: $REMAINING device(s) still exist"
    dmsetup ls | grep -E "^(thin[-_]|snap-)"
else
    if [[ $DEVICE_COUNT -gt 0 ]]; then
        echo "✓ Removed $DEVICE_COUNT device(s)"
    else
        echo "  No thin devices found"
    fi
fi

# Remove pool
echo ""
echo "Removing devicemapper pool..."
if dmsetup ls 2>/dev/null | grep -qE "^pool[[:space:]]"; then
    sync
    sleep 1
    if timeout 15 dmsetup remove --verifyudev pool 2>/dev/null; then
        echo "✓ Pool removed"
    elif [[ $FORCE == true ]] && timeout 10 dmsetup remove --force pool 2>/dev/null; then
        echo "✓ Pool removed (force)"
    else
        echo "❌ ERROR: Failed to remove pool. Aborting cleanup of files."
        echo "   Removing backing files while pool is active WILL cause kernel deadlock."
        echo ""
        echo "Pool status:"
        dmsetup status pool 2>&1 || true
        exit 1
    fi
    sleep 1
else
    echo "  No pool found"
fi

# Verify pool is definitely gone
if dmsetup ls 2>/dev/null | grep -qE "^pool[[:space:]]"; then
    echo "❌ ERROR: Pool still exists. Cannot proceed with file deletion."
    exit 1
fi

# Detach loop devices
echo ""
echo "Detaching loop devices..."
LOOP_COUNT=$(losetup -a 2>/dev/null | wc -l)
if [[ $LOOP_COUNT -gt 0 ]]; then
    losetup -D 2>/dev/null
    echo "✓ Detached $LOOP_COUNT loop device(s)"
else
    echo "  No loop devices attached"
fi

# Remove pool files
echo ""
echo "Removing pool files..."
if [[ -f /var/lib/flyio/pool_meta || -f /var/lib/flyio/pool_data ]]; then
    rm -f /var/lib/flyio/pool_meta /var/lib/flyio/pool_data
    echo "✓ Pool files removed"
else
    echo "  No pool files found"
fi

# Clean test logs
echo ""
echo "Cleaning test logs..."
if [[ -d /tmp/flyio-test ]]; then
    rm -rf /tmp/flyio-test
    echo "✓ Test logs cleaned"
else
    echo "  No test logs found"
fi

# Always remove FSM lock file and socket (critical for subsequent runs)
echo ""
echo "Removing FSM lock file and socket..."
FSM_DIR="/var/lib/flyio/fsm"
if [[ -f "$FSM_DIR/flyio-manager.lock" ]]; then
    rm -f "$FSM_DIR/flyio-manager.lock"
    echo "✓ FSM lock file removed"
fi
if [[ -S "$FSM_DIR/fsm.sock" ]]; then
    rm -f "$FSM_DIR/fsm.sock"
    echo "✓ FSM socket removed"
fi

# Database and FSM state
echo ""
if [[ $REMOVE_ALL == true ]]; then
    echo "Removing database and FSM state (--all mode)..."
    rm -f /var/lib/flyio/images.db
    rm -rf /var/lib/flyio/fsm
    rm -rf /var/lib/flyio/images
    echo "✓ Database, FSM state, and images removed"
elif [[ -t 0 ]]; then
    # Interactive mode
    read -p "Remove database and FSM state? [y/N] " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        echo "Removing database and FSM state..."
        rm -f /var/lib/flyio/images.db
        rm -rf /var/lib/flyio/fsm
        echo "✓ Database and FSM state removed"
    fi

    read -p "Remove downloaded images? [y/N] " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        rm -rf /var/lib/flyio/images
        echo "✓ Images removed"
    fi

    read -p "Remove all flyio directories? [y/N] " -n 1 -r
    echo
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        echo "Removing directories..."
        rm -rf /var/lib/flyio
        rm -rf /mnt/flyio
        echo "✓ Directories removed"
    fi
else
    echo "Non-interactive mode: use --all to remove database/fsm/images"
fi

# Verify cleanup
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Verifying cleanup..."

ISSUES=0

# Check devicemapper
REMAINING_DM=$(dmsetup ls 2>/dev/null | grep -E "^(thin[-_]|snap-|pool[[:space:]])" | wc -l)
if [[ $REMAINING_DM -gt 0 ]]; then
    echo "⚠️  Remaining devicemapper devices:"
    dmsetup ls | grep -E "^(thin[-_]|snap-|pool[[:space:]])"
    ISSUES=$((ISSUES + 1))
else
    echo "✓ No remaining devicemapper devices"
fi

# Check loop devices
REMAINING_LOOPS=$(losetup -a 2>/dev/null | grep -E 'pool_meta|pool_data' | wc -l)
if [[ $REMAINING_LOOPS -gt 0 ]]; then
    echo "⚠️  Remaining loop devices:"
    losetup -a | grep -E 'pool_meta|pool_data'
    ISSUES=$((ISSUES + 1))
else
    echo "✓ No remaining loop devices"
fi

# Check lock file
if [[ -f "$FSM_DIR/flyio-manager.lock" ]]; then
    echo "⚠️  FSM lock file still exists"
    ISSUES=$((ISSUES + 1))
else
    echo "✓ FSM lock file removed"
fi

echo ""
if [[ $ISSUES -eq 0 ]]; then
    echo "✓ Cleanup complete - all checks passed!"
else
    echo "⚠️  Cleanup complete with $ISSUES issue(s)"
fi
echo ""
echo "To restart fresh:"
echo "  • sudo ./scripts/setup.sh"
echo ""
