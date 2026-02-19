// Command analyze-s3 analyzes the S3 bucket to determine optimal devicemapper pool configuration.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type imageInfo struct {
	key  string
	size int64
}

func main() {
	ctx := context.Background()

	// Load AWS configuration (anonymous access for public bucket)
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(aws.AnonymousCredentials{}),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Failed to load AWS config: %v\n", err)
		os.Exit(1)
	}

	// Create S3 client
	client := s3.NewFromConfig(cfg)

	bucket := "flyio-container-images"
	prefix := "images/"

	fmt.Println("=== S3 Bucket Image Size Analysis ===")
	fmt.Println()
	fmt.Printf("Bucket: s3://%s/%s\n", bucket, prefix)
	fmt.Println()

	// List all objects
	fmt.Println("Step 1: Listing all images in bucket...")
	fmt.Println()

	var images []imageInfo
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: Failed to list objects: %v\n", err)
			os.Exit(1)
		}

		for _, obj := range page.Contents {
			// Skip directories (keys ending with /)
			if obj.Key != nil && !isDirectory(*obj.Key) {
				images = append(images, imageInfo{
					key:  *obj.Key,
					size: *obj.Size,
				})
			}
		}
	}

	if len(images) == 0 {
		fmt.Println("ERROR: No images found in bucket")
		os.Exit(1)
	}

	// Sort by size
	sort.Slice(images, func(i, j int) bool {
		return images[i].size < images[j].size
	})

	// Calculate statistics
	var totalSize int64
	for _, img := range images {
		totalSize += img.size
	}
	avgSize := totalSize / int64(len(images))
	minSize := images[0].size
	maxSize := images[len(images)-1].size

	fmt.Println("Step 2: Image Statistics")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("Total images:     %d\n", len(images))
	fmt.Printf("Total size:       %s (%d bytes)\n", humanSize(totalSize), totalSize)
	fmt.Printf("Minimum size:     %s (%d bytes)\n", humanSize(minSize), minSize)
	fmt.Printf("Maximum size:     %s (%d bytes)\n", humanSize(maxSize), maxSize)
	fmt.Printf("Average size:     %s (%d bytes)\n", humanSize(avgSize), avgSize)
	fmt.Println()

	fmt.Println("Step 3: Top 10 Largest Images")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	start := len(images) - 10
	if start < 0 {
		start = 0
	}
	for i := start; i < len(images); i++ {
		fmt.Printf("%-12s  %s\n", humanSize(images[i].size), images[i].key)
	}
	fmt.Println()

	// Calculate recommended pool size
	// Rule: Total size × 2 + 30% overhead for snapshots and CoW operations
	recommendedDataSize := totalSize * 26 / 10 // 2x + 30%
	recommendedDataSizeGB := recommendedDataSize / (1024 * 1024 * 1024)
	if recommendedDataSize%(1024*1024*1024) != 0 {
		recommendedDataSizeGB++
	}
	if recommendedDataSizeGB < 2 {
		recommendedDataSizeGB = 2 // Minimum 2GB
	}
	dataSizeBytes := recommendedDataSizeGB * 1024 * 1024 * 1024
	dataSizeSectors := dataSizeBytes / 512

	// Metadata size: 0.2% of data size (minimum 4MB) - thin pool best practice
	recommendedMetaSize := dataSizeBytes / 500
	if recommendedMetaSize < 4*1024*1024 {
		recommendedMetaSize = 4 * 1024 * 1024 // 4MB minimum
	}
	recommendedMetaSizeMB := recommendedMetaSize / (1024 * 1024)

	// Low water mark: ~1% of pool size (minimum 32MB)
	lowWaterMark := dataSizeSectors / 100
	if lowWaterMark < 65536 {
		lowWaterMark = 65536
	}

	fmt.Println("Step 4: Devicemapper Pool Configuration")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()
	fmt.Println("⚠️  INCORRECT configuration (causes severe I/O slowdown):")
	fmt.Println("  - Block size:      2048 sectors (1 MB) ← TOO LARGE, CAUSES SLOW I/O")
	fmt.Println()
	fmt.Println("✓ RECOMMENDED configuration (optimal performance):")
	fmt.Printf("  - Metadata device: %d MB (0.2%% of data, min 4MB)\n", recommendedMetaSizeMB)
	fmt.Printf("  - Data device:     %d GB (total size × 2 + 30%% overhead)\n", recommendedDataSizeGB)
	fmt.Println("  - Block size:      256 sectors (128 KB) ← OPTIMAL FOR PERFORMANCE")
	fmt.Printf("  - Table size:      %d sectors\n", dataSizeSectors)
	fmt.Printf("  - Low water mark:  %d sectors (~1%% of pool)\n", lowWaterMark)
	fmt.Println()
	fmt.Println("Pool sizing rationale:")
	fmt.Println("  • Metadata: 0.2% of data size (thin pool best practice, min 4MB)")
	fmt.Println("  • Data: Total image size × 2 + 30% overhead for snapshots/CoW")
	fmt.Println("  • Block size: 256 sectors (128KB) - industry standard for containers")
	fmt.Println("    Docker/Podman use 64KB-128KB blocks; 1MB blocks cause 8x slowdown")
	fmt.Println("  • Low water mark: ~1% of pool for timely space warnings")
	fmt.Println()
	fmt.Println("Commands to create pool:")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("cd /var/lib/flyio")
	fmt.Printf("fallocate -l %dM pool_meta\n", recommendedMetaSizeMB)
	fmt.Printf("fallocate -l %dG pool_data\n", recommendedDataSizeGB)
	fmt.Println("METADATA_DEV=$(losetup -f --show pool_meta)")
	fmt.Println("DATA_DEV=$(losetup -f --show pool_data)")
	fmt.Printf("dmsetup create --verifyudev pool --table \"0 %d thin-pool $METADATA_DEV $DATA_DEV 256 %d\"\n", dataSizeSectors, lowWaterMark)
	fmt.Println()
}

func isDirectory(key string) bool {
	return len(key) > 0 && key[len(key)-1] == '/'
}

func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
