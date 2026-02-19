// Package tui provides Terminal User Interface components for the Fly.io Image Manager.
package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/superfly/fsm/s3"
)

// S3Image represents an image available in S3 with its status.
type S3Image struct {
	Key          string
	Size         int64
	LastModified time.Time
	Status       ImageStatus // Local status (available, downloaded, unpacked, active)
}

// ImageStatus represents the local status of an S3 image.
type ImageStatus int

const (
	ImageStatusAvailable  ImageStatus = iota // Available in S3, not downloaded
	ImageStatusDownloaded                    // Downloaded locally
	ImageStatusUnpacked                      // Unpacked to devicemapper
	ImageStatusActive                        // Has active snapshot
)

// String returns the string representation of ImageStatus.
func (s ImageStatus) String() string {
	switch s {
	case ImageStatusDownloaded:
		return "downloaded"
	case ImageStatusUnpacked:
		return "unpacked"
	case ImageStatusActive:
		return "active"
	default:
		return "available"
	}
}

// StatusIcon returns the icon for the status.
func (s ImageStatus) StatusIcon() string {
	switch s {
	case ImageStatusActive:
		return "●" // Green dot for active
	case ImageStatusUnpacked:
		return "◉" // Filled circle for unpacked
	case ImageStatusDownloaded:
		return "○" // Empty circle for downloaded
	default:
		return "◌" // Dotted circle for available
	}
}

// S3BrowserState holds the state for the S3 browser component.
type S3BrowserState struct {
	Images       []S3Image
	SelectedIdx  int
	Loading      bool
	Error        error
	LastRefresh  time.Time
	ScrollOffset int
	VisibleRows  int
}

// NewS3BrowserState creates a new S3 browser state.
func NewS3BrowserState() *S3BrowserState {
	return &S3BrowserState{
		Images:      []S3Image{},
		SelectedIdx: 0,
		VisibleRows: 10,
	}
}

// SelectedImage returns the currently selected image, or nil if none.
func (s *S3BrowserState) SelectedImage() *S3Image {
	if len(s.Images) == 0 || s.SelectedIdx < 0 || s.SelectedIdx >= len(s.Images) {
		return nil
	}
	return &s.Images[s.SelectedIdx]
}

// MoveUp moves selection up.
func (s *S3BrowserState) MoveUp() {
	if s.SelectedIdx > 0 {
		s.SelectedIdx--
		// Adjust scroll offset if needed
		if s.SelectedIdx < s.ScrollOffset {
			s.ScrollOffset = s.SelectedIdx
		}
	}
}

// MoveDown moves selection down.
func (s *S3BrowserState) MoveDown() {
	if s.SelectedIdx < len(s.Images)-1 {
		s.SelectedIdx++
		// Adjust scroll offset if needed
		if s.SelectedIdx >= s.ScrollOffset+s.VisibleRows {
			s.ScrollOffset = s.SelectedIdx - s.VisibleRows + 1
		}
	}
}

// FetchS3Images fetches images from S3 and enriches with local status.
func FetchS3Images(ctx context.Context, s3Client *s3.Client, bucket, prefix string, localImages map[string]ImageStatus) ([]S3Image, error) {
	objects, err := s3Client.ListImagesDetailed(ctx, bucket, prefix)
	if err != nil {
		return nil, fmt.Errorf("failed to list S3 images: %w", err)
	}

	images := make([]S3Image, 0, len(objects))
	for _, obj := range objects {
		// Skip directories (keys ending with /)
		if strings.HasSuffix(obj.Key, "/") {
			continue
		}

		img := S3Image{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified,
			Status:       ImageStatusAvailable,
		}

		// Check local status if we have it
		if status, ok := localImages[obj.Key]; ok {
			img.Status = status
		}

		images = append(images, img)
	}

	return images, nil
}

// ImageName extracts a display name from an S3 key.
func ImageName(key string) string {
	// Get the base name without extension
	base := filepath.Base(key)
	// Remove .tar extension if present
	return strings.TrimSuffix(base, ".tar")
}

// ImageDisplayName extracts a full display name including the parent directory (runtime type).
// For keys like "images/golang/1.tar" returns "golang/1"
func ImageDisplayName(key string) string {
	// Remove .tar extension if present
	key = strings.TrimSuffix(key, ".tar")

	// Split path and get last two components (runtime/version)
	parts := strings.Split(key, "/")
	if len(parts) >= 2 {
		// Return "runtime/version" format (e.g., "golang/1")
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}

	// Fallback to base name
	return filepath.Base(key)
}

// ImageRuntime extracts the runtime type from an S3 key.
// For keys like "images/golang/1.tar" returns "golang"
func ImageRuntime(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return "unknown"
}

// ImageVersion extracts the version/number from an S3 key.
// For keys like "images/golang/1.tar" returns "1"
func ImageVersion(key string) string {
	base := filepath.Base(key)
	return strings.TrimSuffix(base, ".tar")
}

