package activate

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
	fsm "github.com/superfly/fsm"

	"github.com/superfly/fsm/database"
	"github.com/superfly/fsm/devicemapper"
)

const (
	// MaxRetriesCheckSnapshot is the maximum number of retries for database checks
	MaxRetriesCheckSnapshot = 3
	// MaxRetriesCreateSnapshot is the maximum number of retries for devicemapper snapshot creation
	MaxRetriesCreateSnapshot = 3
	// MaxRetriesRegister is the maximum number of retries for database writes
	MaxRetriesRegister = 5
)

// Dependencies holds external dependencies for the Activate FSM.
type Dependencies struct {
	DB        *database.DB
	DeviceMgr *devicemapper.Client
	PoolName  string
}

// stabilizePool forces the dm-thin pool to commit metadata and waits for kernel to settle.
// This MUST be called after any devicemapper operation (create snapshot, activate snapshot)
// to prevent kernel panics from operations happening too close together.
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

type ImageActivateRequest = fsm.ImageActivateRequest
type ImageActivateResponse = fsm.ImageActivateResponse

// snapshotNameForImage returns a stable snapshot name for an image.
func snapshotNameForImage(imageID string) string {
	return fmt.Sprintf("snap-%s", imageID)
}

// checkSnapshot verifies if an active snapshot already exists for the image.
func checkSnapshot(deps *Dependencies) fsm.Transition[ImageActivateRequest, ImageActivateResponse] {
	return func(ctx context.Context, req *fsm.Request[ImageActivateRequest, ImageActivateResponse]) (*fsm.Response[ImageActivateResponse], error) {
		logger := req.Log().WithField("transition", "check-snapshot")
		retryCount := fsm.RetryFromContext(ctx)

		// Enforce retry limit for database operations
		if retryCount > MaxRetriesCheckSnapshot {
			return nil, fsm.Abort(fmt.Errorf("exceeded maximum retries (%d) for check-snapshot transition", MaxRetriesCheckSnapshot))
		}

		if retryCount > 0 {
			logger.WithField("retry_count", retryCount).Info("retrying check-snapshot transition")
		}

		imageID := req.Msg.ImageID

		// Prefer request-provided snapshot name, otherwise derive.
		snapshotName := req.Msg.SnapshotName
		if snapshotName == "" {
			snapshotName = snapshotNameForImage(imageID)
		}

		logger.WithFields(map[string]any{
			"image_id":      imageID,
			"snapshot_name": snapshotName,
		}).Info("checking for existing active snapshot")

		record, err := deps.DB.CheckSnapshotExists(ctx, imageID, snapshotName)
		if err != nil {
			logger.WithError(err).Error("failed to check snapshot in database")
			return nil, fmt.Errorf("database query failed: %w", err)
		}

		if record == nil {
			logger.Info("no active snapshot found; proceeding to create")
			return nil, nil
		}

		// Ensure the underlying device still exists.
		deviceName := filepath.Base(record.DevicePath)
		exists, err := deps.DeviceMgr.DeviceExists(ctx, deviceName)
		if err != nil {
			logger.WithError(err).Error("failed to check snapshot device existence")
			return nil, fmt.Errorf("device existence check failed: %w", err)
		}

		if !exists {
			logger.WithFields(map[string]any{
				"snapshot_name": record.SnapshotName,
				"device_path":   record.DevicePath,
			}).Warn("snapshot record found but device missing; treating as not activated")
			// Best-effort cleanup of stale DB row.
			if err := deps.DB.DeactivateSnapshot(ctx, record.SnapshotID); err != nil {
				logger.WithError(err).Warn("failed to deactivate stale snapshot record")
			}
			return nil, nil
		}

		logger.WithFields(map[string]any{
			"snapshot_id":   record.SnapshotID,
			"snapshot_name": record.SnapshotName,
			"device_path":   record.DevicePath,
		}).Info("image already activated; skipping activation")

		resp := &ImageActivateResponse{
			ImageID:      record.ImageID,
			SnapshotID:   record.SnapshotID,
			SnapshotName: record.SnapshotName,
			DevicePath:   record.DevicePath,
			Active:       record.Active,
			Activated:    false,
			ActivatedAt:  record.CreatedAt,
		}

		// Use the current run's version for Handoff to properly signal FSM completion
		// (fsm.Handoff with empty ULID returns nil, which doesn't stop execution)
		return fsm.NewResponse(resp), fsm.Handoff(req.Run().StartVersion)
	}
}

// createSnapshot creates and activates a devicemapper snapshot for the image.
func createSnapshot(deps *Dependencies) fsm.Transition[ImageActivateRequest, ImageActivateResponse] {
	return func(ctx context.Context, req *fsm.Request[ImageActivateRequest, ImageActivateResponse]) (*fsm.Response[ImageActivateResponse], error) {
		logger := req.Log().WithField("transition", "create-snapshot")
		retryCount := fsm.RetryFromContext(ctx)

		// Enforce retry limit for devicemapper operations
		if retryCount > MaxRetriesCreateSnapshot {
			return nil, fsm.Abort(fmt.Errorf("exceeded maximum retries (%d) for create-snapshot transition", MaxRetriesCreateSnapshot))
		}

		if retryCount > 0 {
			logger.WithField("retry_count", retryCount).Info("retrying create-snapshot transition")
		}

		imageID := req.Msg.ImageID
		originDeviceID := req.Msg.DeviceID
		originDeviceName := req.Msg.DeviceName // The dm device name (e.g., "thin-abc123")

		if originDeviceID == "" {
			logger.Error("origin device ID is required")
			return nil, fsm.Abort(fmt.Errorf("origin device ID is required"))
		}

		if originDeviceName == "" {
			logger.Warn("origin device name not provided, snapshot creation may cause kernel issues")
		}

		snapshotName := req.Msg.SnapshotName
		if snapshotName == "" {
			snapshotName = snapshotNameForImage(imageID)
		}

		logger.WithFields(map[string]any{
			"image_id":           imageID,
			"origin_device_id":   originDeviceID,
			"origin_device_name": originDeviceName,
			"snapshot_name":      snapshotName,
		}).Info("creating snapshot for image")

		// Use timeout for snapshot creation
		ctxWithTimeout, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()

		// Snapshot ID must be numeric for devicemapper. Add offset to origin ID to make it unique.
		// DeviceMapper has a 24-bit limit (max 16777215), so we must ensure the snapshot ID stays within this range.
		originIDNum, err := strconv.ParseUint(originDeviceID, 10, 64)
		if err != nil {
			logger.WithError(err).Error("origin device ID is not numeric")
			return nil, fsm.Abort(fmt.Errorf("origin device ID must be numeric: %w", err))
		}

		// Use modulo to ensure snapshot ID stays within 24-bit limit while maintaining uniqueness.
		// We add 1 million as offset, then apply modulo to wrap within the valid range.
		// This ensures snapshot IDs are different from origin IDs while staying under 16777215.
		const maxDeviceID = 16777215 // 2^24 - 1 (devicemapper 24-bit limit)
		snapshotIDNum := (originIDNum + 1000000) % maxDeviceID

		// Ensure we don't get 0 (reserved) or collide with origin ID
		if snapshotIDNum == 0 {
			snapshotIDNum = 1000000
		}
		if snapshotIDNum == originIDNum {
			snapshotIDNum = (snapshotIDNum + 500000) % maxDeviceID
		}

		snapshotID := fmt.Sprintf("%d", snapshotIDNum)
		logger.WithFields(logrus.Fields{
			"origin_id":   originDeviceID,
			"snapshot_id": snapshotID,
		}).Info("calculated snapshot ID within devicemapper limits")

		// Check if snapshot device already exists (idempotency check)
		// This can happen if a previous run created the snapshot but failed to register it in the database
		snapshotExists, err := deps.DeviceMgr.DeviceExists(ctx, snapshotName)
		if err != nil {
			logger.WithError(err).Error("failed to check snapshot device existence")
			return nil, fmt.Errorf("snapshot existence check failed: %w", err)
		}

		var info *devicemapper.DeviceInfo
		if !snapshotExists {
			// Create new snapshot in thin pool metadata
			// CRITICAL: Use CreateSnapshotSafe which suspends/resumes the origin device
			// Per kernel documentation: "If the origin device that you wish to snapshot is active,
			// you must suspend it before creating the snapshot to avoid corruption."
			if originDeviceName != "" {
				logger.Info("using safe snapshot creation with origin device suspend/resume")
				info, err = deps.DeviceMgr.CreateSnapshotSafe(ctxWithTimeout, deps.PoolName, originDeviceName, originDeviceID, snapshotID)
			} else {
				// Fallback to unsafe method if device name not available
				logger.Warn("falling back to unsafe snapshot creation (no device name)")
				info, err = deps.DeviceMgr.CreateSnapshot(ctxWithTimeout, deps.PoolName, originDeviceID, snapshotID)
			}
			if err != nil {
				logger.WithError(err).Error("failed to create snapshot")
				if devicemapper.IsPoolFullError(err) {
					return nil, fsm.Abort(fmt.Errorf("devicemapper pool full: %w", err))
				}
				if devicemapper.IsDeviceNotFoundError(err) {
					return nil, fsm.Abort(fmt.Errorf("origin device not found: %w", err))
				}
				return nil, fmt.Errorf("failed to create snapshot: %w", err)
			}

			// CRITICAL: Stabilize pool after snapshot creation to prevent kernel panics.
			// CreateSnapshot does create_snap which modifies pool metadata - needs time to commit.
			logger.Debug("stabilizing pool after snapshot creation")
			stabilizePool(deps.PoolName)
		} else {
			logger.WithField("snapshot_name", snapshotName).Info("snapshot already exists in thin pool, will activate")
			info = &devicemapper.DeviceInfo{
				DeviceID: snapshotID,
				Active:   false,
			}
		}

		// Always activate the snapshot device to ensure it has a proper table loaded
		// This is idempotent - if already activated, dmsetup will return success
		// Get the size from the unpacked_images table
		unpackedImage, err := deps.DB.GetUnpackedImageByID(ctxWithTimeout, imageID)
		if err != nil {
			logger.WithError(err).Error("failed to get unpacked image size")
			return nil, fmt.Errorf("failed to get unpacked image: %w", err)
		}

		logger.WithFields(logrus.Fields{
			"snapshot_name": snapshotName,
			"snapshot_id":   snapshotID,
			"size_bytes":    unpackedImage.SizeBytes,
		}).Info("activating snapshot device")

		err = deps.DeviceMgr.ActivateDevice(ctxWithTimeout, deps.PoolName, snapshotName, snapshotID, unpackedImage.SizeBytes)
		if err != nil {
			logger.WithError(err).Error("failed to activate snapshot device")
			return nil, fmt.Errorf("failed to activate snapshot: %w", err)
		}

		// CRITICAL: Stabilize pool after snapshot activation to prevent kernel panics.
		// ActivateDevice does dmsetup create which loads a new device table - needs time to commit.
		logger.Debug("stabilizing pool after snapshot activation")
		stabilizePool(deps.PoolName)

		// Use snapshotName instead of info.Name because CreateSnapshot doesn't set the Name field
		devicePath := deps.DeviceMgr.GetDevicePath(snapshotName)

		logger.WithFields(map[string]any{
			"snapshot_id":   info.DeviceID,
			"snapshot_name": snapshotName,
			"device_path":   devicePath,
		}).Info("snapshot created and activated successfully")

		resp := &ImageActivateResponse{
			ImageID:      imageID,
			SnapshotID:   info.DeviceID,
			SnapshotName: snapshotName,
			DevicePath:   devicePath,
			Active:       true,
			Activated:    true,
			ActivatedAt:  time.Now(),
		}

		return fsm.NewResponse(resp), nil
	}
}

// registerSnapshot records the snapshot in SQLite and updates image activation status.
func registerSnapshot(deps *Dependencies) fsm.Transition[ImageActivateRequest, ImageActivateResponse] {
	return func(ctx context.Context, req *fsm.Request[ImageActivateRequest, ImageActivateResponse]) (*fsm.Response[ImageActivateResponse], error) {
		logger := req.Log().WithField("transition", "register")
		retryCount := fsm.RetryFromContext(ctx)

		// Enforce retry limit for database operations
		if retryCount > MaxRetriesRegister {
			return nil, fsm.Abort(fmt.Errorf("exceeded maximum retries (%d) for register transition", MaxRetriesRegister))
		}

		if retryCount > 0 {
			logger.WithField("retry_count", retryCount).Info("retrying register transition")
		}

		imageID := req.Msg.ImageID

		snapshotID := req.W.Msg.SnapshotID
		snapshotName := req.W.Msg.SnapshotName
		devicePath := req.W.Msg.DevicePath
		originDeviceID := req.Msg.DeviceID

		logger.WithFields(map[string]any{
			"image_id":      imageID,
			"snapshot_id":   snapshotID,
			"snapshot_name": snapshotName,
			"device_path":   devicePath,
			"origin_device": originDeviceID,
		}).Info("registering snapshot in database")

		// Use timeout for database operations
		ctxWithTimeout, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		if err := deps.DB.StoreSnapshot(ctxWithTimeout, imageID, snapshotID, snapshotName, devicePath, originDeviceID); err != nil {
			logger.WithError(err).Error("failed to store snapshot in database")
			return nil, fmt.Errorf("database update failed: %w", err)
		}

		if err := deps.DB.UpdateImageActivationStatus(ctxWithTimeout, imageID, database.ActivationStatusActive); err != nil {
			logger.WithError(err).Error("failed to update image activation status")
			return nil, fmt.Errorf("failed to update image activation status: %w", err)
		}

		logger.Info("snapshot registered and image marked as active")

		return nil, nil
	}
}

// Register registers the Activate FSM with the manager.
func Register(ctx context.Context, manager *fsm.Manager, deps *Dependencies) (fsm.Start[ImageActivateRequest, ImageActivateResponse], fsm.Resume, error) {
	return fsm.Register[ImageActivateRequest, ImageActivateResponse](manager, "activate-image").
		Start("check-snapshot", checkSnapshot(deps)).
		To("create-snapshot", createSnapshot(deps)).
		To("register", registerSnapshot(deps)).
		End("complete").
		Build(ctx)
}
