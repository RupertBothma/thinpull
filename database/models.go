package database

import "time"

// Image represents a downloaded container image.
type Image struct {
	ID                int64
	ImageID           string
	S3Key             string
	LocalPath         string
	Checksum          string
	SizeBytes         int64
	DownloadStatus    string
	ActivationStatus  string
	CreatedAt         time.Time
	DownloadStartedAt *time.Time
	DownloadedAt      *time.Time
	ActivatedAt       *time.Time
	UpdatedAt         time.Time
}

// UnpackedImage represents an image extracted into a devicemapper device.
type UnpackedImage struct {
	ID             int64
	ImageID        string
	DeviceID       string
	DeviceName     string
	DevicePath     string
	SizeBytes      int64
	FileCount      int
	LayoutVerified bool
	CreatedAt      time.Time
	UnpackedAt     time.Time
	UpdatedAt      time.Time
}

// Snapshot represents an active devicemapper snapshot.
type Snapshot struct {
	ID             int64
	ImageID        string
	SnapshotID     string
	SnapshotName   string
	DevicePath     string
	OriginDeviceID string
	Active         bool
	CreatedAt      time.Time
	DeactivatedAt  *time.Time
	UpdatedAt      time.Time
}

// DownloadStatus constants
const (
	DownloadStatusPending     = "pending"
	DownloadStatusDownloading = "downloading"
	DownloadStatusCompleted   = "completed"
	DownloadStatusFailed      = "failed"
)

// ActivationStatus constants
const (
	ActivationStatusInactive = "inactive"
	ActivationStatusActive   = "active"
	ActivationStatusFailed   = "failed"
)
