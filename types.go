package fsm

import (
	"encoding/json"
	"time"
)

// ImageDownloadRequest represents the request to download a container image from S3.
// This is the input to the Download FSM.
//
// Callers SHOULD NOT choose ImageID directly. Instead, they should derive a
// deterministic image ID from the idempotency key (currently the S3 object
// key) using fsm.DeriveImageIDFromS3Key, and pass that value here.
type ImageDownloadRequest struct {
	// S3Key is the S3 object key (e.g., "image-abc123.tar")
	S3Key string `json:"s3_key"`

	// ImageID is a deterministic identifier derived from the idempotency key
	// (for example, via fsm.DeriveImageIDFromS3Key).
	ImageID string `json:"image_id"`

	// Bucket is the S3 bucket name (optional, defaults to configured bucket)
	Bucket string `json:"bucket,omitempty"`

	// Region is the S3 region (optional, defaults to configured region)
	Region string `json:"region,omitempty"`
}

// ImageDownloadResponse represents the response from the Download FSM.
// This contains information about the downloaded image.
type ImageDownloadResponse struct {
	// ImageID is the unique identifier for this image
	ImageID string `json:"image_id"`

	// LocalPath is the local filesystem path where the image is stored
	LocalPath string `json:"local_path"`

	// Checksum is the SHA256 hash of the downloaded file
	Checksum string `json:"checksum"`

	// SizeBytes is the size of the downloaded file in bytes
	SizeBytes int64 `json:"size_bytes"`

	// Downloaded indicates if the image was downloaded (true) or already existed (false)
	Downloaded bool `json:"downloaded"`

	// AlreadyExist indicates that the image was found in a valid, existing state
	// and the download was skipped via idempotency (fsm.Handoff).
	AlreadyExist bool `json:"already_exist"`

	// DownloadedAt is the timestamp when the download completed
	DownloadedAt time.Time `json:"downloaded_at,omitempty"`
}

// ImageUnpackRequest represents the request to unpack a container image into a devicemapper device.
// This is the input to the Unpack FSM.
type ImageUnpackRequest struct {
	// ImageID is the unique identifier for this image
	ImageID string `json:"image_id"`

	// LocalPath is the local filesystem path to the tarball
	LocalPath string `json:"local_path"`

	// Checksum is the SHA256 hash for verification
	Checksum string `json:"checksum"`

	// PoolName is the devicemapper pool name (optional, defaults to configured pool)
	PoolName string `json:"pool_name,omitempty"`

	// DeviceSize is the size of the device to create in bytes (optional, defaults to 10GB)
	DeviceSize int64 `json:"device_size,omitempty"`
}

// ImageUnpackResponse represents the response from the Unpack FSM.
// This contains information about the unpacked image and devicemapper device.
type ImageUnpackResponse struct {
	// ImageID is the unique identifier for this image
	ImageID string `json:"image_id"`

	// DeviceID is the devicemapper thin device ID (numeric)
	DeviceID string `json:"device_id"`

	// DeviceName is the human-readable device name
	DeviceName string `json:"device_name"`

	// DevicePath is the full path to the device node (e.g., "/dev/mapper/image-abc123")
	DevicePath string `json:"device_path"`

	// SizeBytes is the total size of extracted content in bytes
	SizeBytes int64 `json:"size_bytes"`

	// FileCount is the number of files extracted
	FileCount int `json:"file_count"`

	// Unpacked indicates if the image was unpacked (true) or already existed (false)
	Unpacked bool `json:"unpacked"`

	// UnpackedAt is the timestamp when unpacking completed
	UnpackedAt time.Time `json:"unpacked_at,omitempty"`
}

// ImageActivateRequest represents the request to activate a container image by creating a snapshot.
// This is the input to the Activate FSM.
type ImageActivateRequest struct {
	// ImageID is the unique identifier for this image
	ImageID string `json:"image_id"`

	// DeviceID is the devicemapper thin device ID to snapshot
	DeviceID string `json:"device_id"`

	// DeviceName is the origin device name
	DeviceName string `json:"device_name"`

	// SnapshotName is the name for the snapshot (optional, generated if not provided)
	SnapshotName string `json:"snapshot_name,omitempty"`

	// PoolName is the devicemapper pool name (optional, defaults to configured pool)
	PoolName string `json:"pool_name,omitempty"`
}

// ImageActivateResponse represents the response from the Activate FSM.
// This contains information about the created snapshot.
type ImageActivateResponse struct {
	// ImageID is the unique identifier for this image
	ImageID string `json:"image_id"`

	// SnapshotID is the devicemapper snapshot ID (numeric)
	SnapshotID string `json:"snapshot_id"`

	// SnapshotName is the human-readable snapshot name
	SnapshotName string `json:"snapshot_name"`

	// DevicePath is the full path to the snapshot device node
	DevicePath string `json:"device_path"`

	// Active indicates if the snapshot is active
	Active bool `json:"active"`

	// Activated indicates if the snapshot was created (true) or already existed (false)
	Activated bool `json:"activated"`

	// ActivatedAt is the timestamp when activation completed
	ActivatedAt time.Time `json:"activated_at,omitempty"`
}

// Codec implementation for JSON serialization
// The FSM library will automatically use JSON marshaling for these types

// Marshal implements the Codec interface for ImageDownloadRequest
func (r *ImageDownloadRequest) Marshal() ([]byte, error) {
	return json.Marshal(r)
}

// Unmarshal implements the Codec interface for ImageDownloadRequest
func (r *ImageDownloadRequest) Unmarshal(data []byte) error {
	return json.Unmarshal(data, r)
}

// Marshal implements the Codec interface for ImageDownloadResponse
func (r *ImageDownloadResponse) Marshal() ([]byte, error) {
	return json.Marshal(r)
}

// Unmarshal implements the Codec interface for ImageDownloadResponse
func (r *ImageDownloadResponse) Unmarshal(data []byte) error {
	return json.Unmarshal(data, r)
}

// Marshal implements the Codec interface for ImageUnpackRequest
func (r *ImageUnpackRequest) Marshal() ([]byte, error) {
	return json.Marshal(r)
}

// Unmarshal implements the Codec interface for ImageUnpackRequest
func (r *ImageUnpackRequest) Unmarshal(data []byte) error {
	return json.Unmarshal(data, r)
}

// Marshal implements the Codec interface for ImageUnpackResponse
func (r *ImageUnpackResponse) Marshal() ([]byte, error) {
	return json.Marshal(r)
}

// Unmarshal implements the Codec interface for ImageUnpackResponse
func (r *ImageUnpackResponse) Unmarshal(data []byte) error {
	return json.Unmarshal(data, r)
}

// Marshal implements the Codec interface for ImageActivateRequest
func (r *ImageActivateRequest) Marshal() ([]byte, error) {
	return json.Marshal(r)
}

// Unmarshal implements the Codec interface for ImageActivateRequest
func (r *ImageActivateRequest) Unmarshal(data []byte) error {
	return json.Unmarshal(data, r)
}

// Marshal implements the Codec interface for ImageActivateResponse
func (r *ImageActivateResponse) Marshal() ([]byte, error) {
	return json.Marshal(r)
}

// Unmarshal implements the Codec interface for ImageActivateResponse
func (r *ImageActivateResponse) Unmarshal(data []byte) error {
	return json.Unmarshal(data, r)
}
