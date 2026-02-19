#!/bin/bash
# Analyze S3 bucket image sizes to determine optimal devicemapper pool configuration
#
# This script:
# 1. Lists all images in the configured S3 bucket
# 2. Retrieves size metadata for each image
# 3. Calculates statistics (min, max, avg, total)
# 4. Recommends optimal devicemapper pool configuration

set -e

BUCKET="flyio-container-images"
PREFIX="images/"

echo "=== S3 Bucket Image Size Analysis ==="
echo ""
echo "Bucket: s3://${BUCKET}/${PREFIX}"
echo ""

# Check if AWS CLI is available
if ! command -v aws &> /dev/null; then
    echo "ERROR: AWS CLI not found. Please install it first."
    echo "  Ubuntu/Debian: sudo apt-get install awscli"
    echo "  macOS: brew install awscli"
    exit 1
fi

# List all objects and get their sizes
echo "Step 1: Listing all images in bucket..."
echo ""

# Use AWS CLI to list objects with sizes
# Output format: size (bytes) \t key
aws s3api list-objects-v2 \
    --bucket "${BUCKET}" \
    --prefix "${PREFIX}" \
    --query 'Contents[?!ends_with(Key, `/`)].{Size:Size,Key:Key}' \
    --output text | sort -n > /tmp/s3-image-sizes.txt

if [ ! -s /tmp/s3-image-sizes.txt ]; then
    echo "ERROR: No images found in bucket or AWS credentials not configured"
    exit 1
fi

# Calculate statistics
total_images=$(wc -l < /tmp/s3-image-sizes.txt)
total_size=$(awk '{sum+=$1} END {print sum}' /tmp/s3-image-sizes.txt)
min_size=$(head -1 /tmp/s3-image-sizes.txt | awk '{print $1}')
max_size=$(tail -1 /tmp/s3-image-sizes.txt | awk '{print $1}')
avg_size=$((total_size / total_images))

# Convert bytes to human-readable format
human_size() {
    local bytes=$1
    if [ $bytes -lt 1024 ]; then
        echo "${bytes} B"
    elif [ $bytes -lt 1048576 ]; then
        echo "$((bytes / 1024)) KB"
    elif [ $bytes -lt 1073741824 ]; then
        echo "$((bytes / 1048576)) MB"
    else
        echo "$((bytes / 1073741824)) GB"
    fi
}

echo "Step 2: Image Statistics"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Total images:     ${total_images}"
echo "Total size:       $(human_size $total_size) (${total_size} bytes)"
echo "Minimum size:     $(human_size $min_size) (${min_size} bytes)"
echo "Maximum size:     $(human_size $max_size) (${max_size} bytes)"
echo "Average size:     $(human_size $avg_size) (${avg_size} bytes)"
echo ""

echo "Step 3: Top 10 Largest Images"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
tail -10 /tmp/s3-image-sizes.txt | while read size key; do
    printf "%-12s  %s\n" "$(human_size $size)" "$key"
done
echo ""

echo "Step 4: Devicemapper Pool Configuration Recommendations"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

# Calculate recommended pool size
# Rule: 2x largest image + 20% overhead for multiple concurrent images
recommended_data_size=$((max_size * 2 * 12 / 10))  # 2x + 20%
recommended_data_size_gb=$((recommended_data_size / 1073741824 + 1))

# Metadata size: 0.1% of data size (minimum 4MB)
recommended_meta_size=$((recommended_data_size / 1000))
if [ $recommended_meta_size -lt 4194304 ]; then
    recommended_meta_size=4194304  # 4MB minimum
fi
recommended_meta_size_mb=$((recommended_meta_size / 1048576))

echo "Current configuration (from README.md):"
echo "  - Metadata device: 1 MB"
echo "  - Data device:     2 GB"
echo "  - Block size:      2048 sectors (1 MB) ← TOO LARGE, CAUSES SLOW I/O"
echo ""
echo "Recommended configuration:"
echo "  - Metadata device: ${recommended_meta_size_mb} MB"
echo "  - Data device:     ${recommended_data_size_gb} GB"
echo "  - Block size:      256 sectors (128 KB) ← OPTIMAL FOR PERFORMANCE"
echo "  - Low water mark:  65536 sectors (32 MB)"
echo ""

echo "Step 5: Updated Setup Commands"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
cat <<EOF
# Cleanup existing pool (if any)
sudo dmsetup remove pool 2>/dev/null || true
sudo losetup -D

# Create pool files with recommended sizes
fallocate -l ${recommended_meta_size_mb}M pool_meta
fallocate -l ${recommended_data_size_gb}G pool_data

# Attach loop devices
METADATA_DEV="\$(losetup -f --show pool_meta)"
DATA_DEV="\$(losetup -f --show pool_data)"

# Create optimized thin pool
# Block size: 128KB (256 sectors) - 8x faster than 1MB blocks
# Low water mark: 32MB (65536 sectors)
dmsetup create --verifyudev pool --table "0 $((recommended_data_size_gb * 2097152)) thin-pool \${METADATA_DEV} \${DATA_DEV} 256 65536"
EOF

echo ""
echo "✓ Analysis complete. Results saved to /tmp/s3-image-sizes.txt"

