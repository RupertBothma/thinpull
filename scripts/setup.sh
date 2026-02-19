#!/bin/bash
#
# Fly.io Container Image Manager - Setup Script
#
# This script sets up the complete environment for running the Fly.io
# container image management system, including:
#   - System dependencies (Go, AWS CLI, devicemapper tools)
#   - DeviceMapper thin pool creation
#   - AWS credentials verification
#   - SQLite database initialization
#   - Go application build
#
# Usage:
#   sudo ./scripts/setup.sh [options]
#
# Options:
#   --skip-deps        Skip system dependency installation
#   --skip-build       Skip Go application build
#   --pool-size SIZE   Data pool size in GB (default: 2)
#   --force            Skip confirmation prompts
#   --dry-run          Show what would be done without making changes
#   --help             Show this help message
#
# Requirements:
#   - Linux (Ubuntu 20.04+ or similar)
#   - Root/sudo access for devicemapper operations
#   - 10GB+ free disk space
#   - AWS credentials (for S3 access)

set -e

#=============================================================================
# Configuration
#=============================================================================
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
DATA_DIR="/var/lib/flyio"
MOUNT_DIR="/mnt/flyio"
S3_BUCKET="s3://flyio-container-images/images"

# Default options
SKIP_DEPS=false
SKIP_BUILD=false
POOL_SIZE_GB=2
FORCE=false
DRY_RUN=false

#=============================================================================
# Color Output
#=============================================================================
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m'

info()    { echo -e "${BLUE}ℹ${NC} $1"; }
success() { echo -e "${GREEN}✓${NC} $1"; }
warn()    { echo -e "${YELLOW}⚠${NC} $1"; }
error()   { echo -e "${RED}✗${NC} $1"; }
header()  { echo -e "\n${BOLD}=== $1 ===${NC}\n"; }

#=============================================================================
# Parse Arguments
#=============================================================================
parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --skip-deps)    SKIP_DEPS=true; shift ;;
            --skip-build)   SKIP_BUILD=true; shift ;;
            --pool-size)    POOL_SIZE_GB="$2"; shift 2 ;;
            --force)        FORCE=true; shift ;;
            --dry-run)      DRY_RUN=true; shift ;;
            --help)         show_help; exit 0 ;;
            *)              error "Unknown option: $1"; show_help; exit 1 ;;
        esac
    done
}

show_help() {
    head -28 "$0" | tail -n +2 | sed 's/^# //' | sed 's/^#//'
}

#=============================================================================
# Pre-flight Checks
#=============================================================================
preflight_checks() {
    header "Pre-flight Checks"

    # Check Linux
    if [[ "$OSTYPE" != "linux-gnu"* ]]; then
        error "Linux required (detected: $OSTYPE)"
        echo "   This system requires Linux for devicemapper support."
        echo "   Options: Ubuntu VM, WSL2, EC2 instance, or Docker container"
        exit 1
    fi
    success "Linux detected: $(uname -r)"

    # Check root
    if [[ $EUID -ne 0 ]]; then
        error "Root access required"
        echo "   Run with: sudo $0"
        exit 1
    fi
    success "Running as root"

    # Check disk space (need at least 10GB)
    local available_kb
    available_kb=$(df "$DATA_DIR" 2>/dev/null | awk 'NR==2 {print $4}' || \
                   df /var 2>/dev/null | awk 'NR==2 {print $4}' || \
                   df / | awk 'NR==2 {print $4}')
    local available_gb=$((available_kb / 1024 / 1024))

    if [[ $available_gb -lt 10 ]]; then
        error "Insufficient disk space: ${available_gb}GB available, need 10GB+"
        exit 1
    fi
    success "Disk space: ${available_gb}GB available"

    # Check for existing pool
    if dmsetup ls 2>/dev/null | grep -q "^pool"; then
        warn "DeviceMapper pool already exists"
        if [[ $FORCE != true ]]; then
            read -p "Remove existing pool and recreate? [y/N] " -n 1 -r
            echo
            if [[ ! $REPLY =~ ^[Yy]$ ]]; then
                info "Keeping existing pool. Use --force to override."
                SKIP_POOL=true
            fi
        fi
    fi
}

#=============================================================================
# Install System Dependencies
#=============================================================================
install_dependencies() {
    header "Installing System Dependencies"

    if [[ $SKIP_DEPS == true ]]; then
        info "Skipping dependency installation (--skip-deps)"
        return
    fi

    if [[ $DRY_RUN == true ]]; then
        info "[DRY-RUN] Would install: golang-go, lvm2, thin-provisioning-tools, awscli, sqlite3, libsqlite3-dev, rsync, curl, jq"
        return
    fi

    # Update package list
    info "Updating package list..."
    apt-get update -qq

    # Install Go
    if command -v go &>/dev/null; then
        success "Go already installed: $(go version | awk '{print $3}')"
    else
        info "Installing Go..."
        apt-get install -y golang-go
        success "Go installed: $(go version | awk '{print $3}')"
    fi

    # Install DeviceMapper tools
    if command -v dmsetup &>/dev/null && command -v losetup &>/dev/null; then
        success "DeviceMapper tools already installed"
    else
        info "Installing lvm2 (devicemapper tools)..."
        apt-get install -y lvm2 thin-provisioning-tools
        success "DeviceMapper tools installed"
    fi

    # Install AWS CLI
    if command -v aws &>/dev/null; then
        success "AWS CLI already installed: $(aws --version 2>&1 | awk '{print $1}')"
    else
        info "Installing AWS CLI..."
        apt-get install -y awscli
        success "AWS CLI installed"
    fi

    # Install SQLite (CLI + dev libraries)
    if command -v sqlite3 &>/dev/null; then
        success "SQLite already installed: $(sqlite3 --version | awk '{print $1}')"
    else
        info "Installing SQLite..."
        apt-get install -y sqlite3
        success "SQLite installed"
    fi

    # Install SQLite dev libraries (for CGO-based drivers if needed)
    if dpkg -l libsqlite3-dev &>/dev/null 2>&1; then
        success "SQLite dev libraries already installed"
    else
        info "Installing SQLite dev libraries..."
        apt-get install -y libsqlite3-dev 2>/dev/null || true
        success "SQLite dev libraries installed"
    fi

    # Install thin-provisioning-tools (for thin_check, thin_dump, etc.)
    if command -v thin_check &>/dev/null; then
        success "Thin-provisioning tools already installed"
    else
        info "Installing thin-provisioning tools..."
        apt-get install -y thin-provisioning-tools 2>/dev/null || true
        success "Thin-provisioning tools installed"
    fi

    # Install additional tools
    info "Installing additional tools..."
    apt-get install -y rsync curl jq 2>/dev/null || true
    success "Additional tools installed"
}

#=============================================================================
# Verify Required Tools
#=============================================================================
verify_tools() {
    header "Verifying Required Tools"

    local missing=0
    for cmd in go dmsetup losetup mkfs.ext4 sqlite3; do
        if command -v "$cmd" &>/dev/null; then
            success "$cmd found: $(command -v "$cmd")"
        else
            error "$cmd NOT FOUND"
            missing=$((missing + 1))
        fi
    done

    if [[ $missing -gt 0 ]]; then
        error "$missing required tool(s) missing. Run without --skip-deps to install."
        exit 1
    fi
}

#=============================================================================
# Check AWS Credentials
#=============================================================================
check_aws_credentials() {
    header "Checking AWS Credentials"

    # Check if credentials are configured
    local has_creds=false

    if [[ -n "$AWS_ACCESS_KEY_ID" ]]; then
        success "AWS credentials found in environment"
        has_creds=true
    elif [[ -f "$HOME/.aws/credentials" ]] || [[ -f "/home/$SUDO_USER/.aws/credentials" ]]; then
        success "AWS credentials file found"
        has_creds=true
    fi

    if [[ $has_creds == false ]]; then
        warn "AWS credentials not configured"
        echo ""
        echo "   Configure with one of:"
        echo "   1. Environment variables:"
        echo "      export AWS_ACCESS_KEY_ID='your-key'"
        echo "      export AWS_SECRET_ACCESS_KEY='your-secret'"
        echo ""
        echo "   2. AWS CLI configuration (as your user, not root):"
        echo "      sudo -u $SUDO_USER aws configure"
        echo ""

        if [[ $FORCE != true ]]; then
            read -p "Continue without S3 verification? [y/N] " -n 1 -r
            echo
            if [[ ! $REPLY =~ ^[Yy]$ ]]; then
                exit 1
            fi
        fi
        return
    fi

    # Test S3 access
    info "Testing S3 bucket access..."
    if [[ $DRY_RUN == true ]]; then
        info "[DRY-RUN] Would test: aws s3 ls $S3_BUCKET"
        return
    fi

    # Use SUDO_USER credentials for AWS if running as root
    local aws_cmd="aws"
    if [[ -n "$SUDO_USER" ]] && [[ -f "/home/$SUDO_USER/.aws/credentials" ]]; then
        aws_cmd="sudo -u $SUDO_USER aws"
    fi

    if $aws_cmd s3 ls "$S3_BUCKET" &>/dev/null; then
        success "S3 bucket access verified"
        local image_count
        image_count=$($aws_cmd s3 ls "$S3_BUCKET" --recursive 2>/dev/null | grep -c '\.tar$' || echo "0")
        info "Found $image_count image(s) in S3 bucket"
    else
        warn "Cannot access S3 bucket (credentials may be invalid or bucket doesn't exist)"
        echo "   Bucket: $S3_BUCKET"
        echo "   Continuing setup anyway..."
    fi
}


#=============================================================================
# Create DeviceMapper Pool
#=============================================================================
create_devicemapper_pool() {
    header "Creating DeviceMapper Thin Pool"

    if [[ ${SKIP_POOL:-false} == true ]]; then
        info "Keeping existing pool"
        return
    fi

    # Calculate sizes
    local data_size_bytes=$((POOL_SIZE_GB * 1024 * 1024 * 1024))
    local data_size_sectors=$((data_size_bytes / 512))
    local meta_size_mb=$((POOL_SIZE_GB * 4 / 10))  # ~0.4% of data size
    [[ $meta_size_mb -lt 4 ]] && meta_size_mb=4

    # Block size: 256 sectors = 128KB
    local block_size=256
    local block_size_bytes=$((block_size * 512))
    # Low water mark: 10% of pool size, in blocks
    # For a 2GB pool with 128KB blocks: 2GB * 0.10 / 128KB = ~1600 blocks
    local low_water_mark=$(( (data_size_bytes / 10) / block_size_bytes ))
    [[ $low_water_mark -lt 100 ]] && low_water_mark=100
    [[ $low_water_mark -gt 65536 ]] && low_water_mark=65536

    info "Pool configuration:"
    echo "   Data size:      ${POOL_SIZE_GB}GB"
    echo "   Metadata size:  ${meta_size_mb}MB"
    echo "   Block size:     $block_size sectors (128KB)"
    echo "   Low water mark: $low_water_mark blocks ($((low_water_mark * 128 / 1024))MB)"

    if [[ $DRY_RUN == true ]]; then
        info "[DRY-RUN] Would create pool at $DATA_DIR"
        return
    fi

    # Cleanup existing pool (if any)
    info "Cleaning up any existing pool..."
    for dev in $(dmsetup ls 2>/dev/null | grep -E "(thin-|snap-)" | awk '{print $1}'); do
        info "  Removing device: $dev"
        dmsetup remove "$dev" 2>/dev/null || true
    done

    if dmsetup ls 2>/dev/null | grep -q "^pool"; then
        if ! dmsetup remove pool 2>/dev/null; then
            warn "Pool busy, trying force remove..."
            if ! dmsetup remove --force pool; then
                error "Cannot remove existing pool. Use scripts/cleanup.sh first."
                exit 1
            fi
        fi
    fi

    # Detach loop devices
    losetup -D 2>/dev/null || true

    # Create directories
    mkdir -p "$DATA_DIR" "$MOUNT_DIR"
    chmod 755 "$DATA_DIR" "$MOUNT_DIR"
    cd "$DATA_DIR"

    # Remove old pool files
    rm -f pool_meta pool_data

    # Create backing files
    info "Creating backing files..."
    fallocate -l "${meta_size_mb}M" pool_meta
    fallocate -l "${POOL_SIZE_GB}G" pool_data
    success "Backing files created"

    # Setup loop devices
    # Note: We tried --direct-io=on but it causes "invalid error" on loop devices
    # backed by sparse files on ZFS. Using standard buffered I/O instead.
    info "Setting up loop devices..."
    METADATA_DEV=$(losetup -f --show pool_meta)
    DATA_DEV=$(losetup -f --show pool_data)
    echo "   Metadata: $METADATA_DEV"
    echo "   Data:     $DATA_DEV"
    
    # Disable writeback throttling on loop devices if possible
    # This prevents the block layer from throttling writes to dm-thin
    for dev in "$METADATA_DEV" "$DATA_DEV"; do
        local dev_name=$(basename "$dev")
        if [[ -f "/sys/block/$dev_name/queue/wbt_lat_usec" ]]; then
            echo 0 > "/sys/block/$dev_name/queue/wbt_lat_usec" 2>/dev/null || true
            info "  Disabled writeback throttling on $dev_name"
        fi
    done
    success "Loop devices attached"

    # Create thin pool with proper low water mark
    info "Creating thin pool..."
    dmsetup create --verifyudev pool --table "0 $data_size_sectors thin-pool $METADATA_DEV $DATA_DEV $block_size $low_water_mark"

    # Verify pool
    if dmsetup status pool &>/dev/null; then
        success "Thin pool created successfully"
        dmsetup status pool
    else
        error "Pool creation failed"
        exit 1
    fi
}

#=============================================================================
# Build Application
#=============================================================================
build_application() {
    header "Building Go Application"

    if [[ $SKIP_BUILD == true ]]; then
        info "Skipping build (--skip-build)"
        return
    fi

    cd "$PROJECT_DIR"

    if [[ $DRY_RUN == true ]]; then
        info "[DRY-RUN] Would build: go build -o flyio-image-manager ./cmd/flyio-image-manager"
        return
    fi

    info "Downloading dependencies..."
    go mod download

    info "Building application..."
    go build -o flyio-image-manager ./cmd/flyio-image-manager
    chmod +x flyio-image-manager

    if [[ -f flyio-image-manager ]]; then
        success "Application built: $PROJECT_DIR/flyio-image-manager"
    else
        error "Build failed"
        exit 1
    fi
}

#=============================================================================
# Final Validation
#=============================================================================
validate_setup() {
    header "Validating Setup"

    local issues=0

    # Check pool
    if dmsetup status pool &>/dev/null; then
        success "DeviceMapper pool: active"
    else
        error "DeviceMapper pool: NOT FOUND"
        issues=$((issues + 1))
    fi

    # Check directories
    if [[ -d "$DATA_DIR" ]]; then
        success "Data directory: $DATA_DIR"
    else
        error "Data directory: NOT FOUND"
        issues=$((issues + 1))
    fi

    # Check application
    if [[ -f "$PROJECT_DIR/flyio-image-manager" ]]; then
        success "Application: $PROJECT_DIR/flyio-image-manager"
    elif [[ $SKIP_BUILD == true ]]; then
        warn "Application: not built (--skip-build)"
    else
        error "Application: NOT FOUND"
        issues=$((issues + 1))
    fi

    # Check loop devices
    local loop_count
    loop_count=$(losetup -a 2>/dev/null | grep -c pool || echo "0")
    if [[ $loop_count -ge 2 ]]; then
        success "Loop devices: $loop_count attached"
    else
        warn "Loop devices: $loop_count (expected 2)"
    fi

    if [[ $issues -eq 0 ]]; then
        success "All checks passed!"
    else
        error "$issues issue(s) found"
    fi
}

#=============================================================================
# Print Next Steps
#=============================================================================
print_next_steps() {
    header "Setup Complete!"

    echo "Next steps:"
    echo ""
    echo "  1. Test the application:"
    echo "     sudo ./flyio-image-manager process-image --s3-key images/node/5.tar"
    echo ""
    echo "  2. List available images:"
    echo "     sudo ./flyio-image-manager list-images"
    echo ""
    echo "  3. View pool status:"
    echo "     sudo dmsetup status pool"
    echo ""
    echo "  4. When done, clean up with:"
    echo "     sudo ./scripts/cleanup.sh"
    echo ""
}

#=============================================================================
# Main
#=============================================================================
main() {
    echo -e "${BOLD}════════════════════════════════════════════════════════════════${NC}"
    echo -e "${BOLD}      Fly.io Container Image Manager - Setup Script${NC}"
    echo -e "${BOLD}════════════════════════════════════════════════════════════════${NC}"

    parse_args "$@"

    if [[ $DRY_RUN == true ]]; then
        warn "DRY-RUN MODE - No changes will be made"
    fi

    preflight_checks
    install_dependencies
    verify_tools
    check_aws_credentials
    create_devicemapper_pool
    build_application
    validate_setup
    print_next_steps
}

main "$@"

