// Package tui provides Terminal User Interface components for the Fly.io Image Manager.
package tui

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/superfly/fsm/database"
	"github.com/superfly/fsm/s3"
)

// ImageProcessFunc is a function that processes an image (download + unpack + activate).
type ImageProcessFunc func(ctx context.Context, s3Key string) error

// ImageProcessFuncWithProgress is a function that processes an image with progress callback.
type ImageProcessFuncWithProgress func(ctx context.Context, s3Key string, progressCh chan<- ProgressEvent) error

// DataFetcher retrieves dashboard data from various sources.
type DataFetcher struct {
	adminClient                  *AdminClient
	db                           *database.DB
	dbPath                       string // Path to the SQLite database (for diagnostics)
	poolName                     string
	dbError                      error // Error from database connection (if any)
	s3Client                     *s3.Client
	s3Bucket                     string
	s3Prefix                     string
	imageProcessFunc             ImageProcessFunc             // Function to trigger image processing (legacy)
	imageProcessFuncWithProgress ImageProcessFuncWithProgress // Function with progress callback
}

// NewDataFetcher creates a new data fetcher.
func NewDataFetcher(adminClient *AdminClient, db *database.DB, poolName string) *DataFetcher {
	return &DataFetcher{
		adminClient: adminClient,
		db:          db,
		poolName:    poolName,
		s3Bucket:    "flyio-container-images",
		s3Prefix:    "images/",
	}
}

// NewDataFetcherWithPath creates a new data fetcher with explicit database path for diagnostics.
func NewDataFetcherWithPath(adminClient *AdminClient, db *database.DB, dbPath, poolName string, dbError error) *DataFetcher {
	return &DataFetcher{
		adminClient: adminClient,
		db:          db,
		dbPath:      dbPath,
		poolName:    poolName,
		dbError:     dbError,
		s3Bucket:    "flyio-container-images",
		s3Prefix:    "images/",
	}
}

// SetS3Client sets the S3 client for fetching images.
func (f *DataFetcher) SetS3Client(client *s3.Client) {
	f.s3Client = client
}

// SetImageProcessFunc sets the function to trigger image processing.
func (f *DataFetcher) SetImageProcessFunc(fn ImageProcessFunc) {
	f.imageProcessFunc = fn
}

// SetImageProcessFuncWithProgress sets the function to trigger image processing with progress callback.
func (f *DataFetcher) SetImageProcessFuncWithProgress(fn ImageProcessFuncWithProgress) {
	f.imageProcessFuncWithProgress = fn
}

// DBPath returns the configured database path.
func (f *DataFetcher) DBPath() string {
	return f.dbPath
}

// DBError returns any error from database connection.
func (f *DataFetcher) DBError() error {
	return f.dbError
}

// FetchDashboardData retrieves all data needed for the dashboard.
// Returns an error if the FSM admin socket is unavailable (for connection status indicator),
// but still returns partial data for graceful degradation.
func (f *DataFetcher) FetchDashboardData(ctx context.Context) (*DashboardUpdateMsg, error) {
	msg := &DashboardUpdateMsg{
		ActiveRuns:     []FSMRun{},
		SystemStatus:   &SystemStatus{PoolName: f.poolName},
		RecentActivity: []LogEntry{},
	}

	var adminErr error

	// Fetch active FSM runs
	if f.adminClient != nil {
		runs, err := f.fetchActiveFSMs(ctx)
		if err != nil {
			adminErr = err
		} else {
			msg.ActiveRuns = runs
		}
	}

	// Fetch system status (always attempt, even if admin failed)
	status, err := f.fetchSystemStatus(ctx)
	if err == nil {
		msg.SystemStatus = status
	}

	// Fetch recent activity from database
	if f.db != nil {
		activity := f.fetchRecentActivity(ctx)
		msg.RecentActivity = activity
	}

	// Return admin error to signal connection status, but still return partial data
	return msg, adminErr
}

// fetchActiveFSMs retrieves active FSM runs from the admin interface.
func (f *DataFetcher) fetchActiveFSMs(ctx context.Context) ([]FSMRun, error) {
	active, err := f.adminClient.ListActive(ctx)
	if err != nil {
		return nil, err
	}

	runs := make([]FSMRun, 0, len(active))
	for _, a := range active {
		runs = append(runs, ActiveFSMToRun(a))
	}
	return runs, nil
}

// fetchSystemStatus retrieves system status from database and devicemapper.
func (f *DataFetcher) fetchSystemStatus(ctx context.Context) (*SystemStatus, error) {
	status := &SystemStatus{
		PoolName:    f.poolName,
		DBPath:      f.dbPath,
		DBError:     "",
		DBConnected: f.db != nil,
	}

	// Report database connection error if any
	if f.dbError != nil {
		status.DBError = f.dbError.Error()
	}

	// Fetch image counts from database
	if f.db != nil {
		// Count total images
		images, err := f.db.ListImages(ctx, "")
		if err != nil {
			status.DBError = fmt.Sprintf("query error: %v", err)
		} else {
			status.TotalImages = len(images)
		}

		// Count unpacked images from unpacked_images table
		if unpackedImages, err := f.db.ListUnpackedImages(ctx); err == nil {
			status.UnpackedCount = len(unpackedImages)
		}

		// Count active snapshots
		if snapshots, err := f.db.ListActiveSnapshots(ctx); err == nil {
			status.ActiveSnaps = len(snapshots)
		}
	}

	// Fetch devicemapper pool status
	poolStatus, poolErr := f.fetchPoolStatus(ctx)
	if poolErr == nil && poolStatus != nil {
		status.PoolDataUsed = poolStatus.DataUsed
		status.PoolDataTotal = poolStatus.DataTotal
		status.PoolMetaUsed = poolStatus.MetaUsed
		status.PoolMetaTotal = poolStatus.MetaTotal
	} else if poolErr != nil {
		status.PoolError = poolErr.Error()
	}

	return status, nil
}

// PoolStatus holds devicemapper pool usage information.
type PoolStatus struct {
	DataUsed  int64
	DataTotal int64
	MetaUsed  int64
	MetaTotal int64
}

// fetchPoolStatus retrieves devicemapper pool status using dmsetup.
func (f *DataFetcher) fetchPoolStatus(ctx context.Context) (*PoolStatus, error) {
	if f.poolName == "" {
		return nil, fmt.Errorf("pool name not configured")
	}

	// Run dmsetup status <pool>
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Use full path to dmsetup to avoid PATH issues
	cmd := exec.CommandContext(ctx, "/usr/sbin/dmsetup", "status", f.poolName)
	output, err := cmd.Output()
	if err != nil {
		// Try without full path as fallback
		cmd = exec.CommandContext(ctx, "dmsetup", "status", f.poolName)
		output, err = cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("dmsetup status failed: %w", err)
		}
	}

	return parsePoolStatus(string(output))
}

// parsePoolStatus parses dmsetup status output for a thin-pool.
// Format: 0 <length> thin-pool <transaction_id> <used_metadata>/<total_metadata> <used_data>/<total_data> ...
func parsePoolStatus(output string) (*PoolStatus, error) {
	fields := strings.Fields(output)
	if len(fields) < 7 {
		return nil, fmt.Errorf("unexpected dmsetup output format")
	}

	// Check if this is a thin-pool
	if fields[2] != "thin-pool" {
		return nil, fmt.Errorf("not a thin-pool device")
	}

	// Parse metadata usage (field 4): used/total
	metaParts := strings.Split(fields[4], "/")
	if len(metaParts) != 2 {
		return nil, fmt.Errorf("invalid metadata format")
	}
	metaUsed, err := strconv.ParseInt(metaParts[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid metadata used value: %w", err)
	}
	metaTotal, err := strconv.ParseInt(metaParts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid metadata total value: %w", err)
	}

	// Parse data usage (field 5): used/total
	dataParts := strings.Split(fields[5], "/")
	if len(dataParts) != 2 {
		return nil, fmt.Errorf("invalid data format")
	}
	dataUsed, err := strconv.ParseInt(dataParts[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid data used value: %w", err)
	}
	dataTotal, err := strconv.ParseInt(dataParts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid data total value: %w", err)
	}

	// Convert from sectors (512 bytes) to bytes
	const sectorSize = 512
	return &PoolStatus{
		DataUsed:  dataUsed * sectorSize,
		DataTotal: dataTotal * sectorSize,
		MetaUsed:  metaUsed * sectorSize,
		MetaTotal: metaTotal * sectorSize,
	}, nil
}

// activityEntry is a helper for sorting activity by time.
type activityEntry struct {
	timestamp time.Time
	entry     LogEntry
}

// fetchRecentActivity retrieves recent activity from the database.
// It combines recent images, unpacked images, and snapshots into a unified activity log.
func (f *DataFetcher) fetchRecentActivity(ctx context.Context) []LogEntry {
	var activities []activityEntry

	// Fetch recent images
	if images, err := f.db.ListImages(ctx, ""); err == nil {
		for _, img := range images {
			if img.DownloadedAt != nil {
				activities = append(activities, activityEntry{
					timestamp: *img.DownloadedAt,
					entry: LogEntry{
						Timestamp: *img.DownloadedAt,
						Level:     "info",
						Message:   fmt.Sprintf("Downloaded: %s", truncateString(img.S3Key, 40)),
						Fields:    map[string]string{"image_id": img.ImageID, "status": img.DownloadStatus},
					},
				})
			}
		}
	}

	// Fetch recent unpacked images
	if unpacked, err := f.db.ListUnpackedImages(ctx); err == nil {
		for _, img := range unpacked {
			activities = append(activities, activityEntry{
				timestamp: img.UnpackedAt,
				entry: LogEntry{
					Timestamp: img.UnpackedAt,
					Level:     "info",
					Message:   fmt.Sprintf("Unpacked: %s → %s", truncateString(img.ImageID, 16), img.DeviceName),
					Fields:    map[string]string{"device": img.DevicePath},
				},
			})
		}
	}

	// Fetch recent snapshots
	if snapshots, err := f.db.ListActiveSnapshots(ctx); err == nil {
		for _, snap := range snapshots {
			activities = append(activities, activityEntry{
				timestamp: snap.CreatedAt,
				entry: LogEntry{
					Timestamp: snap.CreatedAt,
					Level:     "info",
					Message:   fmt.Sprintf("Activated: %s", snap.SnapshotName),
					Fields:    map[string]string{"device": snap.DevicePath},
				},
			})
		}
	}

	// Sort by timestamp (newest first) and limit to 20
	sortActivities(activities)
	entries := make([]LogEntry, 0, 20)
	for i := 0; i < len(activities) && i < 20; i++ {
		entries = append(entries, activities[i].entry)
	}

	return entries
}

// sortActivities sorts activities by timestamp, newest first.
func sortActivities(activities []activityEntry) {
	for i := 0; i < len(activities)-1; i++ {
		for j := i + 1; j < len(activities); j++ {
			if activities[j].timestamp.After(activities[i].timestamp) {
				activities[i], activities[j] = activities[j], activities[i]
			}
		}
	}
}

// truncateString truncates a string to maxLen, adding "..." if truncated.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// FetchS3Images fetches images from S3 and enriches with local status.
func (f *DataFetcher) FetchS3Images(ctx context.Context) ([]S3Image, error) {
	if f.s3Client == nil {
		return nil, fmt.Errorf("S3 client not configured")
	}

	// Fetch S3 objects
	objects, err := f.s3Client.ListImagesDetailed(ctx, f.s3Bucket, f.s3Prefix)
	if err != nil {
		return nil, fmt.Errorf("failed to list S3 images: %w", err)
	}

	// Build local status map from database
	localStatus := make(map[string]ImageStatus)
	if f.db != nil {
		// Check downloaded images
		if images, err := f.db.ListImages(ctx, ""); err == nil {
			for _, img := range images {
				localStatus[img.S3Key] = ImageStatusDownloaded
			}
		}

		// Check unpacked images (overrides downloaded)
		if unpacked, err := f.db.ListUnpackedImages(ctx); err == nil {
			for _, img := range unpacked {
				// Find the S3 key for this image ID
				if dbImg, err := f.db.GetImageByID(ctx, img.ImageID); err == nil {
					localStatus[dbImg.S3Key] = ImageStatusUnpacked
				}
			}
		}

		// Check active snapshots (overrides unpacked)
		if snapshots, err := f.db.ListActiveSnapshots(ctx); err == nil {
			for _, snap := range snapshots {
				// Find the S3 key for this image ID
				if dbImg, err := f.db.GetImageByID(ctx, snap.ImageID); err == nil {
					localStatus[dbImg.S3Key] = ImageStatusActive
				}
			}
		}
	}

	// Convert to S3Image slice
	images := make([]S3Image, 0, len(objects))
	for _, obj := range objects {
		// Skip directories
		if strings.HasSuffix(obj.Key, "/") {
			continue
		}

		img := S3Image{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified,
			Status:       ImageStatusAvailable,
		}

		if status, ok := localStatus[obj.Key]; ok {
			img.Status = status
		}

		images = append(images, img)
	}

	return images, nil
}

// TriggerImageProcess triggers the image processing pipeline for an S3 key.
func (f *DataFetcher) TriggerImageProcess(ctx context.Context, s3Key string) error {
	if f.imageProcessFunc == nil {
		return fmt.Errorf("image process function not configured")
	}

	return f.imageProcessFunc(ctx, s3Key)
}

// TriggerImageProcessWithProgress triggers the image processing pipeline with progress updates.
func (f *DataFetcher) TriggerImageProcessWithProgress(ctx context.Context, s3Key string, progressCh chan<- ProgressEvent) error {
	if f.imageProcessFuncWithProgress != nil {
		return f.imageProcessFuncWithProgress(ctx, s3Key, progressCh)
	}
	// Fallback to legacy function without progress
	if f.imageProcessFunc != nil {
		return f.imageProcessFunc(ctx, s3Key)
	}
	return fmt.Errorf("image process function not configured")
}
