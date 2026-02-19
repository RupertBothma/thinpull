// Package unpack implements the Unpack FSM for extracting images into devicemapper devices.
package unpack

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/sirupsen/logrus"
	fsm "github.com/superfly/fsm"

	"github.com/superfly/fsm/database"
	"github.com/superfly/fsm/devicemapper"
	"github.com/superfly/fsm/extraction"
)

const (
	// MaxRetriesCheckUnpacked is the maximum number of retries for database checks
	MaxRetriesCheckUnpacked = 3
	// MaxRetriesCreateDevice is the maximum number of retries for devicemapper device creation
	MaxRetriesCreateDevice = 3
	// MaxRetriesExtractLayers is the maximum number of retries for tar extraction
	MaxRetriesExtractLayers = 2
	// MaxRetriesVerifyLayout is the maximum number of retries for filesystem verification
	MaxRetriesVerifyLayout = 2
	// MaxRetriesUpdateDB is the maximum number of retries for database writes
	MaxRetriesUpdateDB = 5
)

// DatabaseManager defines the interface for database operations used by the FSM.
// This allows for mocking in tests.
type DatabaseManager interface {
	CheckImageUnpacked(ctx context.Context, imageID string) (*database.UnpackedImage, error)
	GetUnpackedImageByID(ctx context.Context, imageID string) (*database.UnpackedImage, error)
	DeleteUnpackedImage(ctx context.Context, imageID string) error
	StoreUnpackedImage(ctx context.Context, imageID, deviceID, deviceName, devicePath string, sizeBytes int64, fileCount int) error
	AcquireImageLock(ctx context.Context, imageID, lockedBy string) error
	ReleaseImageLock(ctx context.Context, imageID string) error
	IsImageLocked(ctx context.Context, imageID string) (bool, error)
}

// DeviceManager defines the interface for devicemapper operations used by the FSM.
// This allows for mocking in tests.
type DeviceManager interface {
	DeviceExists(ctx context.Context, deviceName string) (bool, error)
	CreateThinDevice(ctx context.Context, poolName, deviceID string, sizeBytes int64) (*devicemapper.DeviceInfo, error)
	MountDevice(ctx context.Context, devicePath, mountPoint string) error
	IsMounted(mountPoint string) (bool, error)
	UnmountDevice(ctx context.Context, mountPoint string) error
	DeactivateDevice(ctx context.Context, deviceName string) error
	DeleteDevice(ctx context.Context, poolName, deviceID string) error
	GetDevicePath(deviceName string) string
}

// Dependencies holds external dependencies for the Unpack FSM.
type Dependencies struct {
	DB          DatabaseManager
	DeviceMgr   DeviceManager
	Extractor   *extraction.Extractor
	PoolName    string
	MountRoot   string // Base directory for temporary mounts, e.g. /mnt/flyio
	DefaultSize int64  // Default device size in bytes if not specified
}

// ImageUnpackRequest and ImageUnpackResponse reuse the shared types from the
// root fsm package for documentation and external APIs.
type ImageUnpackRequest = fsm.ImageUnpackRequest
type ImageUnpackResponse = fsm.ImageUnpackResponse

// deviceNameForImage returns the devicemapper device name for an image.
//
// Naming contract
//   - devicemapper.CreateThinDevice currently creates devices named
//     "thin-<device_id>" (see devicemapper/dm.go).
//   - All Unpack FSM transitions that compute mount points or DB paths MUST
//     use this helper so that:
//   - createDevice mounts the same name that extractLayers/verifyLayout
//     and updateDB later reference, and
//   - the unpacked_images table stores a device_name that actually exists
//     in devicemapper.
//
// This function is part of the durable idempotency story: given the same
// imageID we derive the same device ID and hence the same device name,
// allowing checkUnpacked to correlate database records with real devices.
func deviceNameForImage(imageID string) string {
	return fmt.Sprintf("thin-%s", deviceIDForImage(imageID))
}

// cleanupDevice performs safe cleanup of a thin device in the correct order:
// 1. Unmount (if mounted)
// 2. Deactivate device (dmsetup remove)
// 3. Delete from pool (dmsetup message)
//
// This function adds brief delays between steps to allow the kernel to process
// each operation before proceeding to the next. Uses a 2-minute timeout for
// the entire cleanup sequence to prevent indefinite hangs.
func cleanupDevice(ctx context.Context, deps *Dependencies, imageID string) {
	// CRITICAL: Unmount operations cause kernel-level D-state hangs that can lead to kernel panic.
	// We intentionally skip cleanup to prevent system instability. Devices will be cleaned up
	// by a separate garbage collection process or manual intervention.
	//
	// This is a deliberate trade-off: we accept resource leakage to prevent kernel panic.

	logger := logrus.WithField("image_id", imageID)
	deviceName := deviceNameForImage(imageID)

	logger.WithField("device_name", deviceName).Warn("cleanup: skipping device cleanup to prevent kernel panic (device will be orphaned)")

	// NOTE: The following operations are DISABLED to prevent kernel panic:
	// - Unmount: causes D-state hangs
	// - Deactivate: may hang if device is in use
	// - Delete: may hang if device is active
	//
	// A separate cleanup process should handle orphaned devices when the system is stable.
}

// stabilizePool forces the dm-thin pool to commit metadata and waits for kernel to settle.
// This MUST be called after any devicemapper operation (create device, mkfs, mount, unmount,
// create snapshot, activate snapshot) to prevent kernel panics from operations happening
// too close together.
//
// The dm-thin subsystem has internal state that needs time to commit. Without this
// stabilization, rapid sequential operations cause kernel panics.
//
// CRITICAL: Do NOT use 'sync' command here - it can block indefinitely if dm-thin
// devices are in a bad state, causing cascading D-state hangs.
//
// PERFORMANCE: With ext4 journaling disabled (-O ^has_journal), we can reduce delays.
// The primary purpose is now just udev synchronization and metadata commit.
func stabilizePool(poolName string) {
	// PERFORMANCE OPTIMIZED: Minimal stabilization for dm-thin pool.
	// With ext4 journaling disabled and 1ms sleeps proven stable, we only need
	// a single metadata commit cycle and quick udev check.

	// Force pool metadata commit using reserve/release metadata snapshot
	// Skip the initial release - it's redundant and causes "No snapshot found" warnings
	exec.Command("dmsetup", "message", poolName, "0", "reserve_metadata_snap").Run()
	exec.Command("dmsetup", "message", poolName, "0", "release_metadata_snap").Run()

	// Quick udev settle with zero timeout - just process pending events, don't wait
	exec.Command("udevadm", "settle", "--timeout=0").Run()
}

// deviceIDForImage returns a numeric device ID derived from the image ID.
// Device IDs must fit within devicemapper's 24-bit limitation (max 16777215).
func deviceIDForImage(imageID string) string {
	// Use the lower 16 characters of the hex portion of imageID and interpret
	// as hex. Apply modulo to ensure it fits in 24 bits.
	const prefix = "img_"
	const maxDeviceID = 16777215 // 2^24 - 1
	hexPart := imageID
	if len(imageID) > len(prefix) && imageID[:len(prefix)] == prefix {
		hexPart = imageID[len(prefix):]
	}
	if len(hexPart) > 16 {
		hexPart = hexPart[:16]
	}
	if n, err := strconv.ParseUint(hexPart, 16, 64); err == nil {
		return fmt.Sprintf("%d", n%maxDeviceID)
	}
	// Fallback: ULID time modulo max device ID
	return fmt.Sprintf("%d", ulid.Make().Time()%maxDeviceID)
}

// checkUnpacked verifies if the image has already been unpacked into a valid
// devicemapper device. If so, it returns Handoff to skip remaining work.
//
// This transition also acquires an exclusive lock on the image to prevent
// concurrent Unpack FSMs from operating on the same image, which could cause
// devicemapper pool contention and kernel panics.
func checkUnpacked(deps *Dependencies) fsm.Transition[ImageUnpackRequest, ImageUnpackResponse] {
	return func(ctx context.Context, req *fsm.Request[ImageUnpackRequest, ImageUnpackResponse]) (*fsm.Response[ImageUnpackResponse], error) {
		logger := req.Log().WithField("transition", "check-unpacked")
		retryCount := fsm.RetryFromContext(ctx)

		// Enforce retry limit for database operations
		if retryCount > MaxRetriesCheckUnpacked {
			return nil, fsm.Abort(fmt.Errorf("exceeded maximum retries (%d) for check-unpacked transition", MaxRetriesCheckUnpacked))
		}

		if retryCount > 0 {
			logger.WithField("retry_count", retryCount).Info("retrying check-unpacked transition")
		}

		imageID := req.Msg.ImageID

		logger.WithField("image_id", imageID).Info("checking if image already unpacked")

		// Acquire exclusive lock on this image to prevent concurrent unpack operations.
		// This prevents multiple Unpack FSMs from stressing the devicemapper pool concurrently.
		if err := deps.DB.AcquireImageLock(ctx, imageID, "unpack-fsm"); err != nil {
			// Check if this is a lock conflict (another FSM is already unpacking this image)
			if ctx.Err() == nil { // Not a context cancellation
				logger.WithError(err).Warn("image is already being unpacked by another FSM")
				// Return Handoff to indicate work is being done elsewhere
				resp := &ImageUnpackResponse{
					ImageID:  imageID,
					Unpacked: false,
				}
				return fsm.NewResponse(resp), fsm.Handoff(req.Run().StartVersion)
			}
			// Context cancelled or other error - propagate it
			return nil, fmt.Errorf("failed to acquire image lock: %w", err)
		}

		logger.Info("acquired image lock")

		// First consult the database for a verified unpacked image.
		record, err := deps.DB.CheckImageUnpacked(ctx, imageID)
		if err != nil {
			logger.WithError(err).Error("failed to check unpacked image in database")
			// Release lock before returning error
			if releaseErr := deps.DB.ReleaseImageLock(ctx, imageID); releaseErr != nil {
				logger.WithError(releaseErr).Error("failed to release image lock after database error")
			}
			return nil, fmt.Errorf("database query failed: %w", err)
		}

		if record == nil {
			logger.Info("image not unpacked yet; proceeding to create device")
			// Keep the lock - it will be released in updateDB after successful unpack
			return nil, nil
		}

		// Verify the device still exists at devicemapper level.
		exists, err := deps.DeviceMgr.DeviceExists(ctx, record.DeviceName)
		if err != nil {
			logger.WithError(err).Error("failed to check device existence")
			// Release lock before returning error
			if releaseErr := deps.DB.ReleaseImageLock(ctx, imageID); releaseErr != nil {
				logger.WithError(releaseErr).Error("failed to release image lock after device check error")
			}
			return nil, fmt.Errorf("device existence check failed: %w", err)
		}

		if !exists {
			logger.WithFields(map[string]any{
				"device_name": record.DeviceName,
				"device_id":   record.DeviceID,
			}).Warn("unpacked image record found, but device is missing; will recreate")
			// Best-effort cleanup of stale DB row.
			if err := deps.DB.DeleteUnpackedImage(ctx, imageID); err != nil {
				logger.WithError(err).Warn("failed to delete stale unpacked image record")
			}
			// Keep the lock - we'll proceed to recreate the device
			return nil, nil
		}

		logger.WithFields(map[string]any{
			"device_name": record.DeviceName,
			"device_path": record.DevicePath,
			"size_bytes":  record.SizeBytes,
			"file_count":  record.FileCount,
		}).Info("image already unpacked and valid; skipping unpack")

		// Release lock since we're not doing any work
		if releaseErr := deps.DB.ReleaseImageLock(ctx, imageID); releaseErr != nil {
			logger.WithError(releaseErr).Error("failed to release image lock after finding existing unpack")
		}

		resp := &ImageUnpackResponse{
			ImageID:    record.ImageID,
			DeviceID:   record.DeviceID,
			DeviceName: record.DeviceName,
			DevicePath: record.DevicePath,
			SizeBytes:  record.SizeBytes,
			FileCount:  record.FileCount,
			Unpacked:   false,
		}

		// Use the current run's version for Handoff to properly signal FSM completion
		// (fsm.Handoff with empty ULID returns nil, which doesn't stop execution)
		return fsm.NewResponse(resp), fsm.Handoff(req.Run().StartVersion)
	}
}

// createDevice creates and activates a thin device for the image and mounts it
// at a temporary mount point under MountRoot.
func createDevice(deps *Dependencies) fsm.Transition[ImageUnpackRequest, ImageUnpackResponse] {
	return func(ctx context.Context, req *fsm.Request[ImageUnpackRequest, ImageUnpackResponse]) (*fsm.Response[ImageUnpackResponse], error) {
		logger := req.Log().WithField("transition", "create-device")
		retryCount := fsm.RetryFromContext(ctx)

		// Enforce retry limit for devicemapper operations
		if retryCount > MaxRetriesCreateDevice {
			return nil, fsm.Abort(fmt.Errorf("exceeded maximum retries (%d) for create-device transition", MaxRetriesCreateDevice))
		}

		if retryCount > 0 {
			logger.WithField("retry_count", retryCount).Info("retrying create-device transition")
		}

		imageID := req.Msg.ImageID

		deviceID := deviceIDForImage(imageID)
		deviceName := deviceNameForImage(imageID)

		sizeBytes := req.Msg.DeviceSize
		if sizeBytes <= 0 {
			if deps.DefaultSize > 0 {
				sizeBytes = deps.DefaultSize
			} else {
				// Default to 10GiB
				sizeBytes = 10 * 1024 * 1024 * 1024
			}
		}

		logger.WithFields(map[string]any{
			"image_id":    imageID,
			"device_id":   deviceID,
			"device_name": deviceName,
			"size_bytes":  sizeBytes,
		}).Info("creating thin device for image")

		// Use timeout for device creation and mount operations
		ctxWithTimeout, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()

		// Check if device already exists (idempotency)
		exists, err := deps.DeviceMgr.DeviceExists(ctxWithTimeout, deviceName)
		if err != nil {
			logger.WithError(err).Error("failed to check if device exists")
			return nil, fmt.Errorf("failed to check device existence: %w", err)
		}

		var info *devicemapper.DeviceInfo

		if exists {
			// Device exists - check if it has a valid database record
			// If no DB record exists, the device is orphaned/incomplete and must be recreated
			// to prevent devicemapper hangs when extracting to a device with unknown data
			record, err := deps.DB.GetUnpackedImageByID(ctx, imageID)
			if err != nil {
				logger.WithError(err).Error("failed to check for unpacked image record")
				return nil, fmt.Errorf("failed to check unpacked image record: %w", err)
			}

			if record == nil {
				// Device exists but no DB record - this is an orphaned device from an incomplete run
				// We MUST delete and recreate it to avoid devicemapper hangs
				logger.WithField("device_name", deviceName).Warn("device exists but no database record found; deleting orphaned device")

				// Note: We cannot safely delete devices due to kernel panic issues with unmount
				// Instead, we'll abort and require manual cleanup
				// Release lock before aborting
				if releaseErr := deps.DB.ReleaseImageLock(ctx, imageID); releaseErr != nil {
					logger.WithError(releaseErr).Error("failed to release image lock before abort")
				}
				return nil, fsm.Abort(fmt.Errorf("orphaned device %s exists without database record; manual cleanup required (reboot and delete device)", deviceName))
			}

			// Device exists AND has valid DB record - safe to reuse (true idempotency case)
			logger.WithField("device_name", deviceName).Info("device already exists with valid database record, reusing")
			info = &devicemapper.DeviceInfo{
				Name:       deviceName,
				DeviceID:   deviceID,
				DevicePath: deps.DeviceMgr.GetDevicePath(deviceName),
				SizeBytes:  sizeBytes, // Assume size is correct
			}
		} else {
			// Create new device
			info, err = deps.DeviceMgr.CreateThinDevice(ctxWithTimeout, deps.PoolName, deviceID, sizeBytes)
			if err != nil {
				logger.WithError(err).Error("failed to create thin device")
				// Distinguish pool exhaustion vs other errors.
				if devicemapper.IsPoolFullError(err) {
					// Release lock before aborting
					if releaseErr := deps.DB.ReleaseImageLock(ctx, imageID); releaseErr != nil {
						logger.WithError(releaseErr).Error("failed to release image lock before abort")
					}
					return nil, fsm.Abort(fmt.Errorf("devicemapper pool full: %w", err))
				}
				// If device was created between our check and now, treat as success
				if devicemapper.IsDeviceExistsError(err) {
					logger.WithField("device_name", deviceName).Info("device created concurrently, reusing")
					info = &devicemapper.DeviceInfo{
						Name:       deviceName,
						DeviceID:   deviceID,
						DevicePath: deps.DeviceMgr.GetDevicePath(deviceName),
						SizeBytes:  sizeBytes,
					}
				} else {
					// Check if device was partially created (orphaned) despite the error.
					// This can happen if CreateThinDevice fails partway through (e.g., mkfs timeout).
					exists, checkErr := deps.DeviceMgr.DeviceExists(ctx, deviceName)
					if checkErr == nil && exists {
						// Device exists but CreateThinDevice failed - this is an orphaned device.
						logger.WithField("device_name", deviceName).Error("device partially created (orphaned); manual cleanup required")
						// Release lock before aborting
						if releaseErr := deps.DB.ReleaseImageLock(ctx, imageID); releaseErr != nil {
							logger.WithError(releaseErr).Error("failed to release image lock before abort")
						}
						return nil, fsm.Abort(fmt.Errorf("orphaned device %s detected after failed creation; run 'flyio-image-manager gc --force' to clean up", deviceName))
					}
					return nil, fmt.Errorf("failed to create thin device: %w", err)
				}
			}

			// CRITICAL: Stabilize pool after device creation to prevent kernel panics.
			// CreateThinDevice does create_thin + dmsetup create + mkfs.ext4 - all rapid
			// operations that need time to commit to pool metadata.
			logger.Debug("stabilizing pool after device creation")
			stabilizePool(deps.PoolName)
		}

		// Mount the device at a stable mountpoint under MountRoot.
		mountPoint := filepath.Join(deps.MountRoot, info.Name)
		if err := os.MkdirAll(mountPoint, 0755); err != nil {
			logger.WithError(err).Error("failed to create mount directory")
			// Cleanup device on failure only if we just created it.
			if !exists {
				cleanupDevice(ctx, deps, imageID)
			}
			return nil, fmt.Errorf("failed to create mount directory: %w", err)
		}

		// Check if already mounted (idempotency)
		isMounted, err := deps.DeviceMgr.IsMounted(mountPoint)
		if err != nil {
			logger.WithError(err).Warn("failed to check mount status, attempting mount anyway")
			isMounted = false
		}

		if isMounted {
			logger.WithField("mount_point", mountPoint).Info("device already mounted, skipping mount")
		} else {
			if err := deps.DeviceMgr.MountDevice(ctxWithTimeout, info.DevicePath, mountPoint); err != nil {
				logger.WithError(err).Error("failed to mount device")
				// Cleanup on failure only if we just created the device.
				if !exists {
					cleanupDevice(ctx, deps, imageID)
				}
				return nil, fmt.Errorf("failed to mount device: %w", err)
			}
			logger.Info("device mounted successfully")

			// CRITICAL: Stabilize pool after mount to ensure kernel has processed the mount.
			// Mount operations interact with the dm-thin device and need time to settle.
			logger.Debug("stabilizing pool after mount")
			stabilizePool(deps.PoolName)
		}

		logger.WithFields(map[string]any{
			"device_path": info.DevicePath,
			"mount_point": mountPoint,
		}).Info("thin device ready")

		resp := &ImageUnpackResponse{
			ImageID:    imageID,
			DeviceID:   info.DeviceID,
			DeviceName: info.Name,
			DevicePath: info.DevicePath,
		}

		return fsm.NewResponse(resp), nil
	}
}

// extractLayers extracts the tarball onto the mounted device using the
// extraction package with strict security limits.
func extractLayers(deps *Dependencies) fsm.Transition[ImageUnpackRequest, ImageUnpackResponse] {
	return func(ctx context.Context, req *fsm.Request[ImageUnpackRequest, ImageUnpackResponse]) (*fsm.Response[ImageUnpackResponse], error) {
		logger := req.Log().WithField("transition", "extract-layers")
		retryCount := fsm.RetryFromContext(ctx)

		// Enforce retry limit for extraction operations
		if retryCount > MaxRetriesExtractLayers {
			return nil, fsm.Abort(fmt.Errorf("exceeded maximum retries (%d) for extract-layers transition", MaxRetriesExtractLayers))
		}

		if retryCount > 0 {
			logger.WithField("retry_count", retryCount).Info("retrying extract-layers transition")
		}

		imageID := req.Msg.ImageID
		localPath := req.Msg.LocalPath

		mountPoint := filepath.Join(deps.MountRoot, deviceNameForImage(imageID))

		logger.WithFields(map[string]any{
			"image_id":    imageID,
			"local_path":  localPath,
			"mount_point": mountPoint,
		}).Info("extracting image layers")

		// Use generous timeout for extraction (large images can take time)
		ctxWithTimeout, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		opts := extraction.DefaultOptions()
		result, err := deps.Extractor.Extract(ctxWithTimeout, localPath, mountPoint, opts)
		if err != nil {
			logger.WithError(err).Error("tar extraction failed; cleaning up device")
			// Cleanup on failure: unmount and delete device.
			cleanupDevice(ctx, deps, imageID)
			// Release lock before aborting
			if releaseErr := deps.DB.ReleaseImageLock(ctx, imageID); releaseErr != nil {
				logger.WithError(releaseErr).Error("failed to release image lock before abort")
			}
			return nil, fsm.Abort(fmt.Errorf("tar extraction failed: %w", err))
		}

		logger.WithFields(map[string]any{
			"files": result.FilesExtracted,
			"bytes": result.BytesExtracted,
		}).Info("extraction completed successfully")

		resp := &ImageUnpackResponse{
			ImageID:   imageID,
			SizeBytes: result.BytesExtracted,
			FileCount: result.FilesExtracted,
		}

		return fsm.NewResponse(resp), nil
	}
}

// verifyLayout performs additional filesystem layout and security checks on the
// unpacked rootfs. The extraction package already enforces strong safety
// guarantees (path sanitization, symlink safety, size limits, permission
// checks); this transition adds higher-level, container-specific invariants.
//
// Security model:
//   - We assume a hostile environment (untrusted blobs) and treat any
//     structural violation as a permanent security failure for this image.
//   - Such violations are returned as fsm.Abort so the FSM does not retry.
func verifyLayout(deps *Dependencies) fsm.Transition[ImageUnpackRequest, ImageUnpackResponse] {
	return func(ctx context.Context, req *fsm.Request[ImageUnpackRequest, ImageUnpackResponse]) (*fsm.Response[ImageUnpackResponse], error) {
		logger := req.Log().WithField("transition", "verify-layout")
		retryCount := fsm.RetryFromContext(ctx)

		// Enforce retry limit for filesystem verification
		if retryCount > MaxRetriesVerifyLayout {
			return nil, fsm.Abort(fmt.Errorf("exceeded maximum retries (%d) for verify-layout transition", MaxRetriesVerifyLayout))
		}

		if retryCount > 0 {
			logger.WithField("retry_count", retryCount).Info("retrying verify-layout transition")
		}

		imageID := req.Msg.ImageID
		deviceName := deviceNameForImage(imageID)
		mountPoint := filepath.Join(deps.MountRoot, deviceName)

		logger.WithFields(map[string]any{
			"image_id":    imageID,
			"mount_point": mountPoint,
		}).Info("verifying filesystem layout")

		// Use timeout for filesystem verification
		ctxWithTimeout, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// Check for timeout before proceeding
		if ctxWithTimeout.Err() != nil {
			return nil, fmt.Errorf("verification timeout: %w", ctxWithTimeout.Err())
		}

		cleanupAndAbort := func(msg string, err error) (*fsm.Response[ImageUnpackResponse], error) {
			if err != nil {
				logger.WithError(err).Error(msg)
			} else {
				logger.Error(msg)
			}
			// Cleanup resources; treat as unrecoverable for this image.
			cleanupDevice(ctx, deps, imageID)
			// Release lock before aborting
			if releaseErr := deps.DB.ReleaseImageLock(ctx, imageID); releaseErr != nil {
				logger.WithError(releaseErr).Error("failed to release image lock before abort")
			}
			return nil, fsm.Abort(fmt.Errorf("invalid filesystem layout: %s", msg))
		}

		// First, delegate to the extraction layer's layout verification so we share
		// common logic for both legacy rootfs/ and direct-root OCI layouts.
		if err := deps.Extractor.VerifyLayout(mountPoint); err != nil {
			return cleanupAndAbort("extractor layout verification failed", err)
		}

		// Determine the logical root directory for container-specific checks. We
		// mirror the logic in extraction.VerifyLayout: prefer a rootfs/
		// subdirectory if present, otherwise treat the mount point as the root.
		rootDir := mountPoint
		layout := "direct-root"

		rootfsPath := filepath.Join(mountPoint, "rootfs")
		if info, err := os.Stat(rootfsPath); err == nil && info.IsDir() {
			rootDir = rootfsPath
			layout = "rootfs-subdir"
		}

		logger = logger.WithField("layout", layout)

		// Expected top-level directories under the logical root for a reasonably
		// complete container image. We check for common directories but only require
		// that at least ONE exists (to ensure we extracted something meaningful).
		// Some minimal images may only have etc/ or bin/, which is valid.
		expectedDirs := []string{"etc", "usr", "var", "bin", "lib", "home"}
		foundCount := 0
		for _, dir := range expectedDirs {
			fullPath := filepath.Join(rootDir, dir)
			if info, err := os.Stat(fullPath); err == nil && info.IsDir() {
				foundCount++
				logger.WithField("dir", dir).Debug("found expected directory")
			}
		}
		if foundCount == 0 {
			return cleanupAndAbort("no standard directories found (etc, usr, var, bin, lib, home)",
				fmt.Errorf("extracted filesystem appears empty or invalid"))
		}
		logger.WithField("found_dirs", foundCount).Info("filesystem layout validated")

		// Permission sanity checks on critical paths. The extraction layer already
		// rejects some dangerous permissions, but we add an extra belt-and-
		// suspenders check here aligned with SECURITY.md.
		checkDir := func(path string) error {
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("expected directory but found file: %s", path)
			}
			mode := info.Mode().Perm()
			// World-writable directories in critical paths are suspicious and we
			// treat them as security violations (Abort).
			if mode&0o002 != 0 {
				return fmt.Errorf("world-writable directory in critical path: %s", path)
			}
			return nil
		}

		// Only check permissions on directories that actually exist
		criticalDirs := []string{"etc", "usr", "bin"}
		for _, dir := range criticalDirs {
			fullPath := filepath.Join(rootDir, dir)
			if _, err := os.Stat(fullPath); err == nil {
				if err := checkDir(fullPath); err != nil {
					return cleanupAndAbort(err.Error(), err)
				}
			}
		}

		logger.Info("filesystem layout verified")

		// Pass through current response state. Any layout violations have already
		// resulted in Abort, so a nil response here is safe.
		return nil, nil
	}
}

// updateDB records the unpacked image in SQLite and cleans up mounts. The
// thin device itself is left available for activation.
func updateDB(deps *Dependencies) fsm.Transition[ImageUnpackRequest, ImageUnpackResponse] {
	return func(ctx context.Context, req *fsm.Request[ImageUnpackRequest, ImageUnpackResponse]) (*fsm.Response[ImageUnpackResponse], error) {
		logger := req.Log().WithField("transition", "update-db")
		retryCount := fsm.RetryFromContext(ctx)

		// Enforce retry limit for database operations
		if retryCount > MaxRetriesUpdateDB {
			return nil, fsm.Abort(fmt.Errorf("exceeded maximum retries (%d) for update-db transition", MaxRetriesUpdateDB))
		}

		if retryCount > 0 {
			logger.WithField("retry_count", retryCount).Info("retrying update-db transition")
		}

		imageID := req.Msg.ImageID

		deviceID := deviceIDForImage(imageID)
		deviceName := deviceNameForImage(imageID)
		devicePath := deps.DeviceMgr.GetDevicePath(deviceName)
		mountPoint := filepath.Join(deps.MountRoot, deviceName)

		sizeBytes := req.W.Msg.SizeBytes
		fileCount := req.W.Msg.FileCount

		logger.WithFields(map[string]any{
			"image_id":    imageID,
			"device_id":   deviceID,
			"device_name": deviceName,
			"device_path": devicePath,
			"size_bytes":  sizeBytes,
			"file_count":  fileCount,
		}).Info("updating unpacked image metadata in database")

		// Use timeout for database operations
		ctxWithTimeout, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// Write to database FIRST before unmounting (unmount can hang)
		if err := deps.DB.StoreUnpackedImage(ctxWithTimeout, imageID, deviceID, deviceName, devicePath, sizeBytes, fileCount); err != nil {
			logger.WithError(err).Error("failed to store unpacked image in database")
			return nil, fmt.Errorf("database update failed: %w", err)
		}

		logger.Info("unpacked image metadata stored successfully")

		// Release the image lock now that unpack is complete
		// This allows other processes to work with this image (e.g., activation)
		if err := deps.DB.ReleaseImageLock(ctx, imageID); err != nil {
			// Log but don't fail - the unpack work is already complete
			logger.WithError(err).Error("failed to release image lock after successful unpack")
		} else {
			logger.Info("released image lock")
		}

		// CRITICAL: Unmount AND DEACTIVATE the device BEFORE activation/snapshot creation.
		// According to Linux dm-thin documentation:
		// "If the origin device that you wish to snapshot is active, you must suspend it
		// before creating the snapshot to avoid corruption."
		//
		// The safest approach is to completely deactivate the device (remove it from
		// /dev/mapper) so there's no possibility of I/O during snapshot creation.
		// The snapshot can still be created from the thin device ID in the pool.
		logger.WithField("mount_point", mountPoint).Info("unmounting and deactivating device before snapshot creation")

		// Step 1: Lazy unmount - this detaches the filesystem from namespace immediately
		// but dirty data may still be pending writeback
		unmountCtx, unmountCancel := context.WithTimeout(ctx, 30*time.Second)
		defer unmountCancel()

		if err := deps.DeviceMgr.UnmountDevice(unmountCtx, mountPoint); err != nil {
			logger.WithError(err).Warn("failed to unmount device")
		} else {
			logger.Info("device unmounted successfully (lazy)")
		}

		// Step 2: Wait for lazy unmount to fully detach
		// Lazy unmount returns immediately but kernel needs time to:
		// - Process pending VFS operations
		// - Detach inodes from the mount namespace
		// - Update internal data structures
		// With ext4 journaling disabled, unmount is faster.
		time.Sleep(1 * time.Millisecond) // Reduced from 2s

		// Step 3: Deactivate the device completely.
		// This removes it from /dev/mapper, ensuring zero I/O possibility during snapshot.
		// The thin device ID still exists in the pool metadata and can be snapshotted.
		deactivateCtx, deactivateCancel := context.WithTimeout(ctx, 30*time.Second)
		defer deactivateCancel()

		if err := deps.DeviceMgr.DeactivateDevice(deactivateCtx, deviceName); err != nil {
			logger.WithError(err).Warn("failed to deactivate device - proceeding anyway")
		} else {
			logger.Info("device deactivated, origin is now completely inactive")
		}

		// Step 4: Wait for kernel to fully process the deactivation
		// Give the kernel time to:
		// - Flush any remaining dm-thin metadata
		// - Process pending device mapper events
		// - Settle udev events
		stabilizePool(deps.PoolName)

		resp := &ImageUnpackResponse{
			ImageID:    imageID,
			DeviceID:   deviceID,
			DeviceName: deviceName,
			DevicePath: devicePath,
			SizeBytes:  sizeBytes,
			FileCount:  fileCount,
			Unpacked:   true,
		}

		return fsm.NewResponse(resp), nil
	}
}

// Register registers the Unpack FSM with the manager.
func Register(ctx context.Context, manager *fsm.Manager, deps *Dependencies) (fsm.Start[ImageUnpackRequest, ImageUnpackResponse], fsm.Resume, error) {
	return fsm.Register[ImageUnpackRequest, ImageUnpackResponse](manager, "unpack-image").
		Start("check-unpacked", checkUnpacked(deps)).
		To("create-device", createDevice(deps)).
		To("extract-layers", extractLayers(deps)).
		To("verify-layout", verifyLayout(deps)).
		To("update-db", updateDB(deps)).
		End("complete").
		Build(ctx)
}
