#!/bin/bash
#
# Fly.io Container Image Manager - Cleanup Script
#
# Safely tears down the environment: snapshots -> thin devices -> pool -> loops -> files
#
# SAFETY: Devices are removed in order to prevent kernel panics. Pool files are
# ONLY deleted after the pool is successfully removed.
#
# Usage: sudo ./scripts/cleanup.sh [--dry-run] [--force] [--all] [--keep-db] [--keep-images] [--verbose]

set -e

DATA_DIR="/var/lib/flyio"
MOUNT_DIR="/mnt/flyio"
FSM_DIR="/var/lib/flyio/fsm"
DRY_RUN=false
FORCE=false
KEEP_DB=false
KEEP_IMAGES=false
REMOVE_ALL=false
VERBOSE=false
POOL_STILL_EXISTS=false

# Colors
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${BLUE}ℹ${NC} $1"; }
success() { echo -e "${GREEN}✓${NC} $1"; }
warn()    { echo -e "${YELLOW}⚠${NC} $1"; }
error()   { echo -e "${RED}✗${NC} $1"; }
header()  { echo -e "\n${BOLD}=== $1 ===${NC}\n"; }
debug()   { [[ $VERBOSE == true ]] && echo -e "   [DEBUG] $1"; }

# Check if running interactively
is_interactive() {
    [[ -t 0 ]] && return 0 || return 1
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --dry-run)      DRY_RUN=true; shift ;;
            --force)        FORCE=true; shift ;;
            --keep-db)      KEEP_DB=true; shift ;;
            --keep-images)  KEEP_IMAGES=true; shift ;;
            --all)          REMOVE_ALL=true; shift ;;
            --verbose|-v)   VERBOSE=true; shift ;;
            --help|-h)
                echo "Usage: sudo $0 [OPTIONS]"
                echo ""
                echo "Options:"
                echo "  --dry-run      Show what would be done without making changes"
                echo "  --force        Force removal even if processes are running"
                echo "  --all          Remove all state (database, FSM, images) without prompting"
                echo "  --keep-db      Keep the SQLite database"
                echo "  --keep-images  Keep downloaded images"
                echo "  --verbose,-v   Show detailed debug output"
                echo "  --help,-h      Show this help"
                exit 0
                ;;
            *)              error "Unknown option: $1"; exit 1 ;;
        esac
    done
}

preflight_checks() {
    header "Pre-flight Checks"
    [[ $EUID -ne 0 ]] && { error "Root required: sudo $0"; exit 1; }
    success "Running as root"

    # Check for kernel deadlock (D-state processes)
    if ps aux 2>/dev/null | awk '$8 ~ /D/' | grep -E 'dmsetup|umount' | grep -v grep >/dev/null; then
        error "KERNEL DEADLOCK DETECTED - Reboot required"
        ps aux 2>/dev/null | awk '$8 ~ /D/' | grep -E 'dmsetup|umount' | grep -v grep
        exit 1
    fi
    success "No kernel deadlock"

    # Check for running flyio-image-manager processes
    local pids
    pids=$(pgrep -f 'flyio-image-manager' 2>/dev/null || true)
    if [[ -n "$pids" ]]; then
        warn "Found running flyio-image-manager processes: $pids"
        if [[ $FORCE != true ]]; then
            error "Cannot cleanup while flyio-image-manager is running. Use --force to override."
            exit 1
        fi
        warn "Continuing anyway due to --force flag"
    else
        success "No flyio-image-manager processes running"
    fi
}

collect_inventory() {
    header "Collecting Inventory"

    # Find all mounts under MOUNT_DIR (sorted in reverse to unmount children first)
    mapfile -t MOUNTED < <(grep "$MOUNT_DIR" /proc/mounts 2>/dev/null | awk '{print $2}' | sort -r || true)

    # Find snapshots (snap-*)
    mapfile -t SNAPS < <(dmsetup ls 2>/dev/null | grep "^snap-" | awk '{print $1}' || true)

    # Find thin devices (thin-* or thin_*)
    mapfile -t THINS < <(dmsetup ls 2>/dev/null | grep -E "^thin[-_]" | awk '{print $1}' || true)

    # Find any OTHER devices that reference the pool (orphaned devices with unusual names)
    mapfile -t ORPHANS < <(
        for dev in $(dmsetup ls 2>/dev/null | awk '{print $1}' | grep -Ev "^(pool|snap-|thin[-_])" || true); do
            table=$(dmsetup table "$dev" 2>/dev/null || true)
            if echo "$table" | grep -qE "thin|/dev/mapper/pool"; then
                echo "$dev"
            fi
        done
    )

    # Check pool existence (use word boundary to avoid matching pool-derived names)
    POOL_EXISTS=$(dmsetup ls 2>/dev/null | grep -qE "^pool[[:space:]]" && echo true || echo false)

    # Find loop devices for pool files
    mapfile -t LOOPS < <(losetup -a 2>/dev/null | grep -E 'pool_meta|pool_data' | cut -d: -f1 || true)

    info "Found: ${#MOUNTED[@]} mounts, ${#SNAPS[@]} snaps, ${#THINS[@]} thins, ${#ORPHANS[@]} orphans, pool=$POOL_EXISTS, ${#LOOPS[@]} loops"

    if [[ $VERBOSE == true ]]; then
        [[ ${#MOUNTED[@]} -gt 0 ]] && debug "Mounts: ${MOUNTED[*]}"
        [[ ${#SNAPS[@]} -gt 0 ]] && debug "Snaps: ${SNAPS[*]}"
        [[ ${#THINS[@]} -gt 0 ]] && debug "Thins: ${THINS[*]}"
        [[ ${#ORPHANS[@]} -gt 0 ]] && debug "Orphans: ${ORPHANS[*]}"
        [[ ${#LOOPS[@]} -gt 0 ]] && debug "Loops: ${LOOPS[*]}"
    fi
}

do_remove() {
    local name="$1" dev="$2"
    local retries=3

    [[ $DRY_RUN == true ]] && { echo "   [DRY-RUN] Would remove $dev"; return 0; }

    # Check if device still exists
    if ! dmsetup info "$dev" &>/dev/null; then
        debug "Device $dev already gone"
        return 0
    fi

    # Sync filesystem before removal
    sync

    # Try multiple times with increasing force
    for ((i=1; i<=retries; i++)); do
        debug "Attempt $i/$retries to remove $dev"

        # Try normal removal with udev verification
        if timeout 15 dmsetup remove --verifyudev "$dev" 2>/dev/null; then
            success "  Removed $dev"
            sleep 0.5
            return 0
        fi

        # Try force removal with udev
        if timeout 15 dmsetup remove --verifyudev --force "$dev" 2>/dev/null; then
            success "  Force removed $dev"
            sleep 0.5
            return 0
        fi

        # Try force without udev (last resort)
        if timeout 10 dmsetup remove --force "$dev" 2>/dev/null; then
            success "  Force removed (no udev) $dev"
            sleep 0.5
            return 0
        fi

        # Wait before retry
        sleep 1
    done

    warn "  Could not remove $dev after $retries attempts"
    return 1
}

unmount_all() {
    header "Unmounting Devices"
    [[ ${#MOUNTED[@]} -eq 0 ]] && { info "None mounted"; return; }

    # Sync before unmounting
    sync

    for mp in "${MOUNTED[@]}"; do
        [[ -z "$mp" ]] && continue
        info "Unmounting: $mp"
        [[ $DRY_RUN == true ]] && { echo "   [DRY-RUN]"; continue; }

        # Try normal unmount first
        if timeout 5 umount "$mp" 2>/dev/null; then
            success "  Unmounted $mp"
        # Force unmount
        elif timeout 5 umount -f "$mp" 2>/dev/null; then
            success "  Force unmounted $mp"
        # Lazy unmount (last resort - may leave data unflushed)
        elif [[ $FORCE == true ]] && umount -l "$mp" 2>/dev/null; then
            warn "  Lazy unmounted $mp (data may be unflushed)"
        else
            warn "  Could not unmount $mp"
            if [[ $VERBOSE == true ]]; then
                # Show what's using the mount
                fuser -vm "$mp" 2>&1 || true
            fi
        fi
        sleep 0.3
    done
}

remove_snapshots() {
    header "Removing Snapshots"
    [[ ${#SNAPS[@]} -eq 0 ]] && { info "None found"; return; }
    for dev in "${SNAPS[@]}"; do [[ -n "$dev" ]] && do_remove snap "$dev"; done
}

remove_thin_devices() {
    header "Removing Thin Devices"
    [[ ${#THINS[@]} -eq 0 ]] && { info "None found"; return; }
    for dev in "${THINS[@]}"; do [[ -n "$dev" ]] && do_remove thin "$dev"; done
}

remove_orphan_devices() {
    header "Removing Orphan Pool-Dependent Devices"
    [[ ${#ORPHANS[@]} -eq 0 ]] && { info "None found"; return; }
    for dev in "${ORPHANS[@]}"; do [[ -n "$dev" ]] && do_remove orphan "$dev"; done
}

# Catch any remaining pool-dependent devices that may have been missed
remove_remaining_pool_devices() {
    header "Checking for Remaining Pool-Dependent Devices"

    local remaining=()
    while IFS= read -r dev; do
        [[ -z "$dev" ]] && continue
        [[ "$dev" == "pool" ]] && continue

        local table
        table=$(dmsetup table "$dev" 2>/dev/null || true)
        if echo "$table" | grep -qE "thin|/dev/mapper/pool"; then
            remaining+=("$dev")
        fi
    done < <(dmsetup ls 2>/dev/null | awk '{print $1}' || true)

    if [[ ${#remaining[@]} -eq 0 ]]; then
        info "None found"
        return
    fi

    warn "Found ${#remaining[@]} remaining pool-dependent device(s)"
    for dev in "${remaining[@]}"; do
        do_remove remaining "$dev"
    done
}

remove_pool() {
    header "Removing Pool"
    [[ $POOL_EXISTS != true ]] && { info "Pool not found"; return; }
    [[ $DRY_RUN == true ]] && { echo "   [DRY-RUN] Would remove pool"; return; }

    # Sync and wait for I/O to complete
    sync
    sleep 1

    if timeout 15 dmsetup remove --verifyudev pool 2>/dev/null; then
        success "Pool removed"
    elif timeout 15 dmsetup remove --verifyudev --force pool 2>/dev/null; then
        success "Pool force removed"
    elif timeout 10 dmsetup remove --force pool 2>/dev/null; then
        success "Pool force removed (no udev)"
    else
        error "Could not remove pool"
        POOL_STILL_EXISTS=true
        if [[ $VERBOSE == true ]]; then
            info "Pool status:"
            dmsetup status pool 2>&1 || true
        fi
    fi
}

detach_loops() {
    header "Detaching Loop Devices"
    [[ ${#LOOPS[@]} -eq 0 ]] && { info "None found"; return; }
    for dev in "${LOOPS[@]}"; do
        [[ -z "$dev" ]] && continue
        [[ $DRY_RUN == true ]] && { echo "   [DRY-RUN] Would detach $dev"; continue; }
        losetup -d "$dev" 2>/dev/null && success "Detached $dev" || warn "Could not detach $dev"
    done
}

remove_files() {
    header "Removing Pool Files"
    [[ ${POOL_STILL_EXISTS:-false} == true ]] && { warn "Skipping - pool still exists (would cause kernel deadlock)"; return; }

    for f in "$DATA_DIR/pool_meta" "$DATA_DIR/pool_data"; do
        [[ ! -f "$f" ]] && continue
        [[ $DRY_RUN == true ]] && { echo "   [DRY-RUN] Would remove $f"; continue; }
        rm -f "$f" && success "Removed $f"
    done
}

# CRITICAL: Always remove FSM lock file and socket to prevent blocking subsequent runs
remove_fsm_locks() {
    header "Removing FSM Lock Files"

    local lock_file="$FSM_DIR/flyio-manager.lock"
    local socket_file="$FSM_DIR/fsm.sock"

    # Always remove lock file (critical for subsequent runs)
    if [[ -f "$lock_file" ]]; then
        [[ $DRY_RUN == true ]] && { echo "   [DRY-RUN] Would remove $lock_file"; } || {
            rm -f "$lock_file" && success "Removed FSM lock file"
        }
    else
        info "No FSM lock file found"
    fi

    # Always remove socket
    if [[ -S "$socket_file" ]]; then
        [[ $DRY_RUN == true ]] && { echo "   [DRY-RUN] Would remove $socket_file"; } || {
            rm -f "$socket_file" && success "Removed FSM socket"
        }
    fi
}

cleanup_optional() {
    header "Optional Cleanup"

    # Skip prompts if not interactive and not --all
    if ! is_interactive && [[ $REMOVE_ALL != true ]]; then
        info "Non-interactive mode: Use --all to remove database/images/fsm, or --keep-db/--keep-images to skip"
        return
    fi

    # Database
    if [[ -f "$DATA_DIR/images.db" ]]; then
        if [[ $REMOVE_ALL == true && $KEEP_DB != true ]]; then
            [[ $DRY_RUN != true ]] && rm -f "$DATA_DIR/images.db" && success "Removed database"
        elif [[ $KEEP_DB != true ]] && is_interactive; then
            read -p "Remove database? [y/N] " -n 1 -r; echo
            [[ $REPLY =~ ^[Yy]$ ]] && rm -f "$DATA_DIR/images.db" && success "Removed database"
        fi
    fi

    # Images directory
    if [[ -d "$DATA_DIR/images" ]]; then
        if [[ $REMOVE_ALL == true && $KEEP_IMAGES != true ]]; then
            [[ $DRY_RUN != true ]] && rm -rf "$DATA_DIR/images" && success "Removed images"
        elif [[ $KEEP_IMAGES != true ]] && is_interactive; then
            read -p "Remove downloaded images? [y/N] " -n 1 -r; echo
            [[ $REPLY =~ ^[Yy]$ ]] && rm -rf "$DATA_DIR/images" && success "Removed images"
        fi
    fi

    # FSM state (databases, but NOT the lock file which is handled separately)
    if [[ -d "$DATA_DIR/fsm" ]]; then
        local has_state=false
        [[ -f "$DATA_DIR/fsm/fsm-state.db" || -f "$DATA_DIR/fsm/fsm-history.db" ]] && has_state=true

        if [[ $has_state == true ]]; then
            if [[ $REMOVE_ALL == true ]]; then
                [[ $DRY_RUN != true ]] && rm -f "$DATA_DIR/fsm/fsm-state.db" "$DATA_DIR/fsm/fsm-history.db" && success "Removed FSM databases"
            elif is_interactive; then
                read -p "Remove FSM state databases? [y/N] " -n 1 -r; echo
                [[ $REPLY =~ ^[Yy]$ ]] && rm -f "$DATA_DIR/fsm/fsm-state.db" "$DATA_DIR/fsm/fsm-history.db" && success "Removed FSM databases"
            fi
        fi
    fi
}

verify_cleanup() {
    header "Verifying Cleanup"

    [[ $DRY_RUN == true ]] && { info "Skipping verification in dry-run mode"; return 0; }

    local issues=0

    # Check for remaining devicemapper devices
    local remaining_dm
    remaining_dm=$(dmsetup ls 2>/dev/null | grep -E "^(pool[[:space:]]|thin[-_]|snap-)" | wc -l)
    if [[ $remaining_dm -gt 0 ]]; then
        warn "Remaining devicemapper devices:"
        dmsetup ls | grep -E "^(pool[[:space:]]|thin[-_]|snap-)"
        issues=$((issues + 1))
    else
        success "No remaining devicemapper devices"
    fi

    # Check for remaining loop devices
    local remaining_loops
    remaining_loops=$(losetup -a 2>/dev/null | grep -E 'pool_meta|pool_data' | wc -l)
    if [[ $remaining_loops -gt 0 ]]; then
        warn "Remaining loop devices:"
        losetup -a | grep -E 'pool_meta|pool_data'
        issues=$((issues + 1))
    else
        success "No remaining loop devices"
    fi

    # Check for FSM lock file
    if [[ -f "$FSM_DIR/flyio-manager.lock" ]]; then
        warn "FSM lock file still exists: $FSM_DIR/flyio-manager.lock"
        issues=$((issues + 1))
    else
        success "FSM lock file removed"
    fi

    # Check for pool files (only if pool was removed)
    if [[ $POOL_STILL_EXISTS != true ]]; then
        if [[ -f "$DATA_DIR/pool_meta" || -f "$DATA_DIR/pool_data" ]]; then
            warn "Pool files still exist"
            issues=$((issues + 1))
        else
            success "Pool files removed"
        fi
    fi

    # Check for remaining mounts
    local remaining_mounts
    remaining_mounts=$(grep "$MOUNT_DIR" /proc/mounts 2>/dev/null | wc -l)
    if [[ $remaining_mounts -gt 0 ]]; then
        warn "Remaining mounts:"
        grep "$MOUNT_DIR" /proc/mounts
        issues=$((issues + 1))
    else
        success "No remaining mounts"
    fi

    echo ""
    if [[ $issues -eq 0 ]]; then
        success "All cleanup checks passed!"
    else
        warn "Cleanup completed with $issues issue(s)"
    fi

    return $issues
}

print_summary() {
    header "Cleanup Complete"
    echo "To verify manually:"
    echo "  dmsetup ls        # Should be empty or no pool/thin/snap"
    echo "  losetup -a        # Should have no pool_* devices"
    echo "  ls $DATA_DIR/     # Pool files should be gone"
    echo ""
    echo "To restart fresh:"
    echo "  sudo ./scripts/setup.sh"
}

main() {
    echo -e "${BOLD}════════════════════════════════════════════════════════════${NC}"
    echo -e "${BOLD}   Fly.io Container Image Manager - Cleanup Script${NC}"
    echo -e "${BOLD}════════════════════════════════════════════════════════════${NC}"

    parse_args "$@"
    [[ $DRY_RUN == true ]] && warn "DRY-RUN MODE - No changes will be made"
    [[ $VERBOSE == true ]] && info "Verbose mode enabled"
    [[ $FORCE == true ]] && warn "Force mode enabled"

    # Phase 1: Pre-flight checks
    preflight_checks

    # Phase 2: Collect inventory of what needs to be cleaned
    collect_inventory

    # Phase 3: Unmount filesystems (must be done before device removal)
    unmount_all

    # Phase 4: Remove devicemapper devices in correct order
    # Order is critical: snapshots -> thin devices -> orphans -> remaining -> pool
    remove_snapshots
    remove_thin_devices
    remove_orphan_devices
    remove_remaining_pool_devices  # Safety net for anything we missed
    remove_pool

    # Phase 5: Detach loop devices (must be after pool removal)
    detach_loops

    # Phase 6: Remove files (ONLY if pool was successfully removed)
    remove_files

    # Phase 7: Always remove FSM lock files (critical for subsequent runs)
    remove_fsm_locks

    # Phase 8: Optional cleanup (database, images, FSM state)
    cleanup_optional

    # Phase 9: Verification
    verify_cleanup

    print_summary
}

main "$@"
