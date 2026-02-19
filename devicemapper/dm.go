// Package devicemapper provides utilities for managing devicemapper thin devices and snapshots.
//
// This package wraps Linux devicemapper operations for creating, managing, and cleaning up
// thin-provisioned devices and copy-on-write snapshots. It is used by the Unpack and Activate
// FSMs to extract container images into devices and create snapshot instances.
//
// # Overview
//
// DeviceMapper thin provisioning allows:
//   - Thin devices: Dynamically allocated storage from a pool
//   - Snapshots: Copy-on-write clones of thin devices
//   - Space efficiency: Multiple snapshots share unchanged data
//
// # Prerequisites
//
// Requires:
//   - Linux with device-mapper support
//   - Root/sudo privileges
//   - devicemapper thin pool already created (e.g., "pool")
//   - Tools: dmsetup, mkfs.ext4
//
// # Usage Example
//
//	client := devicemapper.New()
//	client.SetLogger(logger)
//
//	// Create a thin device (10GB)
//	info, err := client.CreateThinDevice(ctx, "pool", "device123", 10*1024*1024*1024)
//	if err != nil {
//		log.Fatal(err)
//	}
//	fmt.Printf("Created device: %s at %s\n", info.Name, info.DevicePath)
//
//	// Mount and use the device
//	err = client.MountDevice(ctx, info.DevicePath, "/mnt/container")
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	// ... extract files to /mnt/container ...
//
//	// Unmount when done
//	err = client.UnmountDevice(ctx, "/mnt/container")
//
//	// Create a snapshot for activation
//	snapshot, err := client.CreateSnapshot(ctx, "pool", info.DeviceID, "snap123")
//	if err != nil {
//		log.Fatal(err)
//	}
//	fmt.Printf("Snapshot created: %s\n", snapshot.DevicePath)
//
// # Error Handling
//
// The package defines custom error types for common conditions:
//   - DeviceExistsError: Device already exists in pool
//   - PoolFullError: Thin pool has no free space
//   - DeviceNotFoundError: Device does not exist
//
// These errors can be checked with errors.As() for specific handling.
//
// # Cleanup Policy (CRITICAL)
//
// IMPORTANT: Production code paths (FSMs, CreateThinDevice) should NEVER automatically call
// UnmountDevice, DeactivateDevice, or DeleteDevice on error paths. These cleanup operations
// have been observed to trigger kernel-level D-state hangs and kernel panics when executed
// on a stressed or buggy dm-thin stack.
//
// Instead, follow the "fail-dumb" pattern:
//  1. Log the failure with full context (pool name, device ID, device path, error details)
//  2. Add a warning that the device is being left active for manual/GC cleanup
//  3. Return the error without attempting cleanup operations
//
// Cleanup should ONLY happen via:
//   - A separate garbage collection process when the system is idle
//   - Manual administrative intervention
//   - Explicit cleanup commands (not automatic error handling)
//
// See the cleanupDevice function in unpack/fsm.go for the reference pattern.
//
// Rationale: Attempting to clean up devices that just failed operations (mkfs, mount, etc.)
// on a stressed dm-thin stack is exactly the operation pattern that triggers D-state hangs
// and kernel panics. We accept resource leakage to prevent system instability.
package devicemapper

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Client wraps devicemapper operations.
type Client struct {
	logger *logrus.Logger
	mu     sync.Mutex // serialize devicemapper operations per process
}

// New creates a new devicemapper client.
func New() *Client {
	return &Client{
		logger: logrus.New(),
	}
}

// SetLogger sets a custom logger for the client.
func (c *Client) SetLogger(logger *logrus.Logger) {
	c.logger = logger
}

// SuppressLogs disables all log output from the devicemapper client.
// This is useful when running in TUI mode where logs would interfere with the display.
func (c *Client) SuppressLogs() {
	c.logger.SetOutput(io.Discard)
}

// DeviceInfo contains information about a devicemapper device.
type DeviceInfo struct {
	Name       string
	DeviceID   string
	DevicePath string
	Active     bool
	SizeBytes  int64
}

// CreateThinDevice creates a new thin device in the specified pool.
//
// This function performs three operations:
//  1. Creates the thin device in the pool (dmsetup message)
//  2. Activates the device with a device-mapper table (dmsetup create)
//  3. Formats the device with ext4 filesystem (mkfs.ext4)
//
// The device is immediately ready for mounting and use after this call succeeds.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - poolName: Name of the devicemapper pool (e.g., "pool")
//   - deviceID: Unique device identifier, typically 8-character hex string
//   - sizeBytes: Device size in bytes (max 100GB)
//
// Returns:
//   - *DeviceInfo: Information about the created device (name, path, size)
//   - error: Any error during creation, activation, or formatting
//
// Errors:
//   - DeviceExistsError: If a device with this ID already exists
//   - PoolFullError: If the thin pool has no free space
//   - Validation errors for invalid inputs
//
// IMPORTANT: This function does NOT perform automatic cleanup on failure. If any step fails,
// the device may be left in a partially-created state (created but not activated, or activated
// but not formatted). This is intentional to prevent kernel panics caused by cleanup operations
// on a stressed dm-thin stack. See the package-level "Cleanup Policy" documentation.
//
// Example:
//
//	// Create 10GB device
//	info, err := client.CreateThinDevice(ctx, "pool", "abc12345", 10*1024*1024*1024)
//	if err != nil {
//		var poolFull *devicemapper.PoolFullError
//		if errors.As(err, &poolFull) {
//			log.Fatal("Pool is full, cannot create device")
//		}
//		log.Fatal(err)
//	}
//	// Device is ready at /dev/mapper/thin-abc12345
//	fmt.Printf("Device ready: %s\n", info.DevicePath)
func (c *Client) CreateThinDevice(ctx context.Context, poolName, deviceID string, sizeBytes int64) (*DeviceInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Validate inputs
	if err := validateDeviceID(deviceID); err != nil {
		return nil, fmt.Errorf("invalid device ID: %w", err)
	}

	if err := validatePoolName(poolName); err != nil {
		return nil, fmt.Errorf("invalid pool name: %w", err)
	}

	if sizeBytes <= 0 {
		return nil, fmt.Errorf("size must be positive: %d", sizeBytes)
	}

	// Enforce max size (100GB)
	const maxSize = 100 * 1024 * 1024 * 1024 // 100GB
	if sizeBytes > maxSize {
		return nil, fmt.Errorf("size too large: %d bytes (max %d)", sizeBytes, maxSize)
	}

	logger := c.logger.WithFields(logrus.Fields{
		"pool":      poolName,
		"device_id": deviceID,
		"size":      sizeBytes,
	})

	// Pre-flight check: Verify pool has capacity before attempting operation
	// This prevents kernel panics caused by operating on a nearly-full pool
	if _, err := c.checkPoolCapacityUnlocked(ctx, poolName, sizeBytes); err != nil {
		return nil, err
	}

	logger.Info("creating thin device")

	// Step 1: Create thin device using dmsetup message
	// Format: dmsetup message <pool> 0 "create_thin <device_id>"
	cmdArgs := []string{"message", poolName, "0", fmt.Sprintf("create_thin %s", deviceID)}
	logger.WithFields(logrus.Fields{
		"command": "dmsetup",
		"args":    cmdArgs,
	}).Debug("executing dmsetup message create_thin")

	startTime := time.Now()
	cmd := exec.CommandContext(ctx, "dmsetup", cmdArgs...)
	output, err := cmd.CombinedOutput()
	duration := time.Since(startTime)

	logger.WithFields(logrus.Fields{
		"command":     "dmsetup message create_thin",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
	}).Debug("dmsetup message create_thin completed")

	if err != nil {
		// Check for specific errors
		outputStr := string(output)
		logger.WithFields(logrus.Fields{
			"error":  err.Error(),
			"output": outputStr,
		}).Error("failed to create thin device")

		if strings.Contains(outputStr, "File exists") || strings.Contains(outputStr, "already exists") {
			return nil, &DeviceExistsError{DeviceID: deviceID}
		}
		if strings.Contains(outputStr, "No space") || strings.Contains(outputStr, "pool full") {
			return nil, &PoolFullError{PoolName: poolName}
		}
		return nil, fmt.Errorf("failed to create thin device: %w (output: %s)", err, outputStr)
	}

	// Generate device name
	deviceName := fmt.Sprintf("thin-%s", deviceID)

	// Calculate sectors (512 bytes per sector)
	sectors := sizeBytes / 512

	// Step 2: Activate the device
	// Format: dmsetup create <name> --table "0 <sectors> thin /dev/mapper/<pool> <device_id>"
	table := fmt.Sprintf("0 %d thin /dev/mapper/%s %s", sectors, poolName, deviceID)
	cmdArgs = []string{"create", deviceName, "--table", table}
	logger.WithFields(logrus.Fields{
		"command":     "dmsetup",
		"args":        cmdArgs,
		"device_name": deviceName,
	}).Debug("executing dmsetup create")

	startTime = time.Now()
	cmd = exec.CommandContext(ctx, "dmsetup", cmdArgs...)
	output, err = cmd.CombinedOutput()
	duration = time.Since(startTime)

	logger.WithFields(logrus.Fields{
		"command":     "dmsetup create",
		"device_name": deviceName,
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
	}).Debug("dmsetup create completed")

	if err != nil {
		// CRITICAL: Do NOT attempt cleanup here. Calling deleteThinDevice on a device
		// that just failed activation can trigger kernel panics. Leave the device
		// for manual/GC cleanup.
		logger.WithFields(logrus.Fields{
			"error":       err.Error(),
			"output":      string(output),
			"device_name": deviceName,
			"device_id":   deviceID,
		}).Warn("failed to activate device; leaving device for manual/GC cleanup (no automatic cleanup to prevent kernel panic)")

		return nil, fmt.Errorf("failed to activate device: %w (output: %s)", err, string(output))
	}

	devicePath := fmt.Sprintf("/dev/mapper/%s", deviceName)

	// Step 3: Create ext4 filesystem WITHOUT journaling
	// CRITICAL: We disable the ext4 journal (-O ^has_journal) to prevent jbd2 hangs.
	// The journal can cause kernel panics when:
	// - The dm-thin pool is under stress
	// - Multiple thin devices are active
	// - Unmount tries to flush pending journal writes
	// Since these are temporary extraction targets, we don't need crash consistency.
	logger.WithField("device_path", devicePath).Info("creating ext4 filesystem (no journal)")

	cmdArgs = []string{"-F", "-O", "^has_journal", devicePath}
	logger.WithFields(logrus.Fields{
		"command":     "mkfs.ext4",
		"args":        cmdArgs,
		"device_path": devicePath,
	}).Debug("executing mkfs.ext4")

	startTime = time.Now()
	cmd = exec.CommandContext(ctx, "mkfs.ext4", cmdArgs...)
	output, err = cmd.CombinedOutput()
	duration = time.Since(startTime)

	logger.WithFields(logrus.Fields{
		"command":     "mkfs.ext4",
		"device_path": devicePath,
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
	}).Debug("mkfs.ext4 completed")

	if err != nil {
		// CRITICAL: Do NOT attempt cleanup here. This is the exact failure scenario
		// that triggers kernel panics when followed by DeactivateDevice/DeleteDevice.
		// The device is left active and formatted (or partially formatted) for manual
		// cleanup or GC when the system is stable.
		logger.WithFields(logrus.Fields{
			"error":       err.Error(),
			"output":      string(output),
			"device_path": devicePath,
			"device_name": deviceName,
			"device_id":   deviceID,
			"pool_name":   poolName,
		}).Warn("failed to create filesystem; leaving device active for manual/GC cleanup (no automatic cleanup to prevent kernel panic)")

		return nil, fmt.Errorf("failed to create filesystem: %w", err)
	}

	logger.WithField("device_path", devicePath).Info("thin device created successfully")

	return &DeviceInfo{
		Name:       deviceName,
		DeviceID:   deviceID,
		DevicePath: devicePath,
		Active:     true,
		SizeBytes:  sizeBytes,
	}, nil
}

// CreateSnapshot creates a snapshot of an existing thin device.
// originID is the device ID of the origin device.
// snapshotID is the device ID for the new snapshot.
func (c *Client) CreateSnapshot(ctx context.Context, poolName, originID, snapshotID string) (*DeviceInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Validate inputs
	if err := validateDeviceID(originID); err != nil {
		return nil, fmt.Errorf("invalid origin ID: %w", err)
	}

	if err := validateDeviceID(snapshotID); err != nil {
		return nil, fmt.Errorf("invalid snapshot ID: %w", err)
	}

	if err := validatePoolName(poolName); err != nil {
		return nil, fmt.Errorf("invalid pool name: %w", err)
	}

	logger := c.logger.WithFields(logrus.Fields{
		"pool":        poolName,
		"origin_id":   originID,
		"snapshot_id": snapshotID,
	})

	// Pre-flight check: Verify pool has capacity before attempting snapshot creation
	// Snapshots require metadata space and potentially data space for CoW blocks
	if _, err := c.checkPoolCapacityUnlocked(ctx, poolName, 0); err != nil {
		return nil, err
	}

	logger.Info("creating snapshot")

	// Create snapshot using dmsetup message
	// Format: dmsetup message <pool> 0 "create_snap <snapshot_id> <origin_id>"
	cmdArgs := []string{"message", poolName, "0", fmt.Sprintf("create_snap %s %s", snapshotID, originID)}
	logger.WithFields(logrus.Fields{
		"command": "dmsetup",
		"args":    cmdArgs,
	}).Debug("executing dmsetup message create_snap")

	startTime := time.Now()
	cmd := exec.CommandContext(ctx, "dmsetup", cmdArgs...)
	output, err := cmd.CombinedOutput()
	duration := time.Since(startTime)

	logger.WithFields(logrus.Fields{
		"command":     "dmsetup message create_snap",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
	}).Debug("dmsetup message create_snap completed")

	if err != nil {
		outputStr := string(output)
		logger.WithFields(logrus.Fields{
			"error":  err.Error(),
			"output": outputStr,
		}).Error("failed to create snapshot")

		if strings.Contains(outputStr, "File exists") || strings.Contains(outputStr, "already exists") {
			return nil, &DeviceExistsError{DeviceID: snapshotID}
		}
		if strings.Contains(outputStr, "No space") || strings.Contains(outputStr, "pool full") {
			return nil, &PoolFullError{PoolName: poolName}
		}
		if strings.Contains(outputStr, "not found") || strings.Contains(outputStr, "No such") {
			return nil, &DeviceNotFoundError{DeviceID: originID}
		}
		return nil, fmt.Errorf("failed to create snapshot: %w (output: %s)", err, outputStr)
	}

	logger.Info("snapshot created successfully")

	return &DeviceInfo{
		DeviceID: snapshotID,
		Active:   false, // Not activated yet
	}, nil
}

// SuspendDevice suspends a devicemapper device.
// This is CRITICAL for snapshot creation - the kernel documentation states:
// "If the origin device that you wish to snapshot is active, you must suspend
// it before creating the snapshot to avoid corruption."
//
// The device should be resumed after the snapshot is created.
func (c *Client) SuspendDevice(ctx context.Context, deviceName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.suspendDeviceUnlocked(ctx, deviceName)
}

func (c *Client) suspendDeviceUnlocked(ctx context.Context, deviceName string) error {
	logger := c.logger.WithField("device_name", deviceName)
	logger.Info("suspending device for safe snapshot creation")

	cmdArgs := []string{"suspend", deviceName}
	startTime := time.Now()
	cmd := exec.CommandContext(ctx, "dmsetup", cmdArgs...)
	output, err := cmd.CombinedOutput()
	duration := time.Since(startTime)

	logger.WithFields(logrus.Fields{
		"command":     "dmsetup suspend",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
	}).Debug("dmsetup suspend completed")

	if err != nil {
		// Not a fatal error - device may not exist or may already be suspended
		logger.WithFields(logrus.Fields{
			"error":  err.Error(),
			"output": string(output),
		}).Warn("failed to suspend device (may be inactive or already suspended)")
		return fmt.Errorf("failed to suspend device: %w (output: %s)", err, string(output))
	}

	logger.Info("device suspended successfully")
	return nil
}

// ResumeDevice resumes a suspended devicemapper device.
// This should be called after snapshot creation to restore normal I/O.
func (c *Client) ResumeDevice(ctx context.Context, deviceName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.resumeDeviceUnlocked(ctx, deviceName)
}

func (c *Client) resumeDeviceUnlocked(ctx context.Context, deviceName string) error {
	logger := c.logger.WithField("device_name", deviceName)
	logger.Info("resuming device after snapshot creation")

	cmdArgs := []string{"resume", deviceName}
	startTime := time.Now()
	cmd := exec.CommandContext(ctx, "dmsetup", cmdArgs...)
	output, err := cmd.CombinedOutput()
	duration := time.Since(startTime)

	logger.WithFields(logrus.Fields{
		"command":     "dmsetup resume",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
	}).Debug("dmsetup resume completed")

	if err != nil {
		logger.WithFields(logrus.Fields{
			"error":  err.Error(),
			"output": string(output),
		}).Error("failed to resume device")
		return fmt.Errorf("failed to resume device: %w (output: %s)", err, string(output))
	}

	logger.Info("device resumed successfully")
	return nil
}

// CreateSnapshotSafe creates a snapshot of an existing thin device with proper
// suspend/resume of the origin device as recommended by kernel documentation.
// This is the PREFERRED method for creating snapshots of active devices.
//
// originDeviceName is the dm device name (e.g., "thin-abc123") to suspend.
// originID is the device ID of the origin device.
// snapshotID is the device ID for the new snapshot.
//
// The origin device is suspended before snapshot creation and resumed after,
// ensuring data consistency and preventing kernel corruption/panics.
func (c *Client) CreateSnapshotSafe(ctx context.Context, poolName, originDeviceName, originID, snapshotID string) (*DeviceInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	logger := c.logger.WithFields(logrus.Fields{
		"pool":               poolName,
		"origin_device_name": originDeviceName,
		"origin_id":          originID,
		"snapshot_id":        snapshotID,
	})

	// Validate inputs
	if err := validateDeviceID(originID); err != nil {
		return nil, fmt.Errorf("invalid origin ID: %w", err)
	}

	if err := validateDeviceID(snapshotID); err != nil {
		return nil, fmt.Errorf("invalid snapshot ID: %w", err)
	}

	if err := validatePoolName(poolName); err != nil {
		return nil, fmt.Errorf("invalid pool name: %w", err)
	}

	// Pre-flight check: Verify pool has capacity
	if _, err := c.checkPoolCapacityUnlocked(ctx, poolName, 0); err != nil {
		return nil, err
	}

	// Check if origin device is active (exists in /dev/mapper)
	// If not active, we don't need to suspend/resume - just create the snapshot
	originActive := false
	if _, err := os.Stat("/dev/mapper/" + originDeviceName); err == nil {
		originActive = true
	}

	// Step 1: Suspend the origin device (only if active)
	// Per kernel docs: "you must suspend it before creating the snapshot to avoid corruption"
	if originActive {
		logger.Info("origin device is active, suspending before snapshot creation")
		if err := c.suspendDeviceUnlocked(ctx, originDeviceName); err != nil {
			logger.WithError(err).Warn("could not suspend origin device, attempting snapshot anyway")
		}
	} else {
		logger.Info("origin device is not active (deactivated), no suspend needed")
	}

	// Step 2: Create the snapshot
	logger.Info("creating snapshot")
	cmdArgs := []string{"message", poolName, "0", fmt.Sprintf("create_snap %s %s", snapshotID, originID)}

	startTime := time.Now()
	cmd := exec.CommandContext(ctx, "dmsetup", cmdArgs...)
	output, err := cmd.CombinedOutput()
	duration := time.Since(startTime)

	logger.WithFields(logrus.Fields{
		"command":     "dmsetup message create_snap",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
	}).Debug("dmsetup message create_snap completed")

	// Step 3: Resume the origin device (only if we suspended it)
	if originActive {
		logger.Info("resuming origin device after snapshot")
		resumeErr := c.resumeDeviceUnlocked(ctx, originDeviceName)
		if resumeErr != nil {
			logger.WithError(resumeErr).Warn("failed to resume origin device after snapshot")
		}
	}

	// Now check if snapshot creation failed
	if err != nil {
		outputStr := string(output)
		logger.WithFields(logrus.Fields{
			"error":  err.Error(),
			"output": outputStr,
		}).Error("failed to create snapshot")

		if strings.Contains(outputStr, "File exists") || strings.Contains(outputStr, "already exists") {
			return nil, &DeviceExistsError{DeviceID: snapshotID}
		}
		if strings.Contains(outputStr, "No space") || strings.Contains(outputStr, "pool full") {
			return nil, &PoolFullError{PoolName: poolName}
		}
		if strings.Contains(outputStr, "not found") || strings.Contains(outputStr, "No such") {
			return nil, &DeviceNotFoundError{DeviceID: originID}
		}
		return nil, fmt.Errorf("failed to create snapshot: %w (output: %s)", err, string(output))
	}

	logger.Info("snapshot created successfully with safe suspend/resume")

	return &DeviceInfo{
		DeviceID: snapshotID,
		Active:   false, // Not activated yet
	}, nil
}

// ActivateDevice activates a thin device or snapshot.
// deviceName is the name to use for the activated device.
// deviceID is the thin device ID.
// sizeBytes is the size of the device in bytes.
func (c *Client) ActivateDevice(ctx context.Context, poolName, deviceName, deviceID string, sizeBytes int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := validateDeviceName(deviceName); err != nil {
		return fmt.Errorf("invalid device name: %w", err)
	}

	if err := validateDeviceID(deviceID); err != nil {
		return fmt.Errorf("invalid device ID: %w", err)
	}

	logger := c.logger.WithFields(logrus.Fields{
		"pool":        poolName,
		"device_name": deviceName,
		"device_id":   deviceID,
	})

	logger.Info("activating device")

	// Calculate sectors
	sectors := sizeBytes / 512

	// Activate the device
	table := fmt.Sprintf("0 %d thin /dev/mapper/%s %s", sectors, poolName, deviceID)
	cmdArgs := []string{"create", deviceName, "--table", table}
	logger.WithFields(logrus.Fields{
		"command": "dmsetup",
		"args":    cmdArgs,
	}).Debug("executing dmsetup create")

	startTime := time.Now()
	cmd := exec.CommandContext(ctx, "dmsetup", cmdArgs...)
	output, err := cmd.CombinedOutput()
	duration := time.Since(startTime)

	logger.WithFields(logrus.Fields{
		"command":     "dmsetup create",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
	}).Debug("dmsetup create completed")

	if err != nil {
		logger.WithFields(logrus.Fields{
			"error":  err.Error(),
			"output": string(output),
		}).Error("failed to activate device")
		return fmt.Errorf("failed to activate device: %w (output: %s)", err, string(output))
	}

	logger.Info("device activated successfully")
	return nil
}

// DeactivateDevice deactivates a device using a 2-stage fallback strategy:
// 1. Standard remove with 10s timeout
// 2. Force remove (--force) with 10s timeout
//
// WARNING: This operation can trigger kernel-level D-state hangs and panics when called
// on devices that are in a bad state or on a stressed dm-thin stack. Use with extreme caution.
// See package-level "Cleanup Policy" documentation.
func (c *Client) DeactivateDevice(ctx context.Context, deviceName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := validateDeviceName(deviceName); err != nil {
		return fmt.Errorf("invalid device name: %w", err)
	}

	logger := c.logger.WithField("device_name", deviceName)
	logger.Info("deactivating device")

	// Check if device exists first
	exists, err := c.DeviceExists(ctx, deviceName)
	if err != nil {
		logger.WithError(err).Warn("failed to check device existence")
	}
	if !exists {
		logger.Info("device not found, already deactivated")
		return nil
	}

	// Strategy 1: Remove with --verifyudev for proper udev synchronization
	// This prevents race conditions that can lead to kernel panics
	ctxWithTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmdArgs := []string{"remove", "--verifyudev", deviceName}
	logger.WithFields(logrus.Fields{
		"command": "dmsetup",
		"args":    cmdArgs,
		"timeout": "10s",
	}).Debug("executing dmsetup remove --verifyudev")

	startTime := time.Now()
	cmd := exec.CommandContext(ctxWithTimeout, "dmsetup", cmdArgs...)
	output, err := cmd.CombinedOutput()
	duration := time.Since(startTime)
	timedOut := ctxWithTimeout.Err() != nil

	logger.WithFields(logrus.Fields{
		"command":     "dmsetup remove",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
		"timed_out":   timedOut,
	}).Debug("dmsetup remove completed")

	if err == nil {
		logger.Info("device deactivated successfully")
		return nil
	}

	outputStr := string(output)
	if strings.Contains(outputStr, "not found") || strings.Contains(outputStr, "No such") {
		logger.Warn("device not found, already deactivated")
		return nil
	}

	// Strategy 2: Force remove with --verifyudev
	logger.Warn("standard remove failed, trying force remove with udev sync")
	ctxWithTimeout2, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()

	cmdArgs = []string{"remove", "--verifyudev", "--force", deviceName}
	logger.WithFields(logrus.Fields{
		"command": "dmsetup",
		"args":    cmdArgs,
		"timeout": "10s",
	}).Debug("executing dmsetup remove --force")

	startTime = time.Now()
	cmd = exec.CommandContext(ctxWithTimeout2, "dmsetup", cmdArgs...)
	output2, err2 := cmd.CombinedOutput()
	duration = time.Since(startTime)
	timedOut = ctxWithTimeout2.Err() != nil

	logger.WithFields(logrus.Fields{
		"command":     "dmsetup remove --force",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output2),
		"timed_out":   timedOut,
	}).Debug("dmsetup remove --force completed")

	if err2 == nil {
		logger.Info("device force-deactivated successfully")
		return nil
	}

	// If both strategies fail and timeout, it's likely a kernel deadlock
	logger.WithFields(logrus.Fields{
		"error":     err2.Error(),
		"output":    string(output2),
		"timed_out": timedOut,
	}).Error("all deactivation strategies failed (possible kernel deadlock)")

	return fmt.Errorf("failed to deactivate device (possible kernel deadlock): %w (output: %s)", err, outputStr)
}

// DeleteDevice deletes a thin device or snapshot from the pool.
//
// WARNING: This operation can trigger kernel-level D-state hangs and panics when called
// on devices that are still active or on a stressed dm-thin stack. Use with extreme caution.
// See package-level "Cleanup Policy" documentation.
func (c *Client) DeleteDevice(ctx context.Context, poolName, deviceID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := validateDeviceID(deviceID); err != nil {
		return fmt.Errorf("invalid device ID: %w", err)
	}

	logger := c.logger.WithFields(logrus.Fields{
		"pool":      poolName,
		"device_id": deviceID,
	})

	logger.Info("deleting device")

	// Delete using dmsetup message
	cmdArgs := []string{"message", poolName, "0", fmt.Sprintf("delete %s", deviceID)}
	logger.WithFields(logrus.Fields{
		"command": "dmsetup",
		"args":    cmdArgs,
	}).Debug("executing dmsetup message delete")

	startTime := time.Now()
	cmd := exec.CommandContext(ctx, "dmsetup", cmdArgs...)
	output, err := cmd.CombinedOutput()
	duration := time.Since(startTime)

	logger.WithFields(logrus.Fields{
		"command":     "dmsetup message delete",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
	}).Debug("dmsetup message delete completed")

	if err != nil {
		// Ignore "not found" errors
		outputStr := string(output)
		if strings.Contains(outputStr, "not found") || strings.Contains(outputStr, "No such") {
			logger.Warn("device not found, already deleted")
			return nil
		}
		logger.WithFields(logrus.Fields{
			"error":  err.Error(),
			"output": outputStr,
		}).Error("failed to delete device")
		return fmt.Errorf("failed to delete device: %w (output: %s)", err, outputStr)
	}

	logger.Info("device deleted successfully")
	return nil
}

// deleteThinDevice is an internal helper to delete a thin device.
//
// NOTE: This function is intentionally unused in production code paths to prevent kernel panics.
// It is kept for potential future use in explicit cleanup/GC commands when the system is idle.
// See package-level "Cleanup Policy" documentation for details.
//
// WARNING: Do NOT call this function from error handling paths or automatic cleanup logic.
func (c *Client) deleteThinDevice(ctx context.Context, poolName, deviceID string) {
	cmd := exec.CommandContext(ctx, "dmsetup", "message", poolName, "0", fmt.Sprintf("delete %s", deviceID))
	cmd.Run() // Ignore errors
}

// DeviceExists checks if a device exists and is active with timeout protection.
func (c *Client) DeviceExists(ctx context.Context, deviceName string) (bool, error) {
	// Add 5-second timeout to prevent hanging on bad devicemapper state
	ctxWithTimeout, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	logger := c.logger.WithField("device_name", deviceName)
	cmdArgs := []string{"info", deviceName}
	logger.WithFields(logrus.Fields{
		"command": "dmsetup",
		"args":    cmdArgs,
		"timeout": "5s",
	}).Debug("executing dmsetup info")

	startTime := time.Now()
	cmd := exec.CommandContext(ctxWithTimeout, "dmsetup", cmdArgs...)
	output, err := cmd.CombinedOutput()
	duration := time.Since(startTime)
	timedOut := ctxWithTimeout.Err() != nil

	logger.WithFields(logrus.Fields{
		"command":     "dmsetup info",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
		"timed_out":   timedOut,
	}).Debug("dmsetup info completed")

	if err != nil {
		// Check for timeout
		if ctxErr := ctxWithTimeout.Err(); ctxErr != nil {
			logger.WithError(ctxErr).Error("device existence check timed out (devicemapper may be hung)")
			return false, fmt.Errorf("device existence check timed out (devicemapper may be hung): %w", ctxErr)
		}
		// Check if it's a "not found" error
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			logger.Debug("device not found")
			return false, nil
		}
		logger.WithError(err).Error("failed to check device existence")
		return false, fmt.Errorf("failed to check device existence: %w", err)
	}
	logger.Debug("device exists")
	return true, nil
}

// GetDevicePath returns the device path for a device name.
func (c *Client) GetDevicePath(deviceName string) string {
	return fmt.Sprintf("/dev/mapper/%s", deviceName)
}

// MountDevice mounts a device to a mount point with pre-mount validation and timeout protection.
// It performs the following steps:
// 1. Check if already mounted (idempotency)
// 2. Verify device exists and is accessible
// 3. Ensure mount point directory exists
// 4. Attempt mount with 10-second timeout (shorter than FSM transition timeout)
func (c *Client) MountDevice(ctx context.Context, devicePath, mountPoint string) error {
	logger := c.logger.WithFields(logrus.Fields{
		"device": devicePath,
		"mount":  mountPoint,
	})

	// Step 1: Check if already mounted (idempotency)
	mounted, err := c.IsMounted(mountPoint)
	if err != nil {
		logger.WithError(err).Warn("failed to check mount status, continuing anyway")
	} else if mounted {
		logger.Info("device already mounted, skipping")
		return nil
	}

	// Step 2: Verify device exists and is accessible
	if _, err := os.Stat(devicePath); err != nil {
		logger.WithError(err).Error("device not accessible")
		return fmt.Errorf("device not accessible: %w", err)
	}

	// Step 3: Ensure mount point directory exists
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		logger.WithError(err).Error("failed to create mount point")
		return fmt.Errorf("failed to create mount point: %w", err)
	}

	// Step 4: Attempt mount with shorter timeout to fail fast
	// Use 10-second timeout instead of 30s to avoid blocking FSM transitions
	// PERFORMANCE: Use noatime,nodiratime to reduce metadata writes
	logger.Info("mounting device")
	ctxWithTimeout, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmdArgs := []string{"-o", "noatime,nodiratime", devicePath, mountPoint}
	logger.WithFields(logrus.Fields{
		"command": "mount",
		"args":    cmdArgs,
		"timeout": "10s",
	}).Debug("executing mount")

	startTime := time.Now()
	cmd := exec.CommandContext(ctxWithTimeout, "mount", cmdArgs...)
	output, err := cmd.CombinedOutput()
	duration := time.Since(startTime)
	timedOut := ctxWithTimeout.Err() != nil

	logger.WithFields(logrus.Fields{
		"command":     "mount",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
		"timed_out":   timedOut,
	}).Debug("mount completed")

	if err != nil {
		if ctxErr := ctxWithTimeout.Err(); ctxErr != nil {
			logger.WithFields(logrus.Fields{
				"error":     ctxErr.Error(),
				"output":    string(output),
				"timed_out": true,
			}).Error("mount timed out (device may be in bad state)")
			return fmt.Errorf("mount timed out after 10s (device may be in bad state): %w (output: %s)", ctxErr, string(output))
		}
		logger.WithFields(logrus.Fields{
			"error":  err.Error(),
			"output": string(output),
		}).Error("failed to mount device")
		return fmt.Errorf("failed to mount device: %w (output: %s)", err, string(output))
	}

	logger.Info("device mounted successfully")
	return nil
}

// IsMounted checks if a mount point is currently mounted by reading /proc/mounts.
func (c *Client) IsMounted(mountPoint string) (bool, error) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false, fmt.Errorf("failed to read /proc/mounts: %w", err)
	}
	return strings.Contains(string(data), mountPoint), nil
}

// UnmountDevice unmounts a device using lazy unmount to prevent kernel hangs.
//
// CRITICAL: For dm-thin devices, we MUST use lazy unmount (-l) as the primary strategy.
// Standard unmount calls sync_filesystem() internally which waits for all dirty pages
// to be flushed. On a stressed dm-thin pool, this can block indefinitely, leading to
// D-state processes and eventual kernel panic.
//
// Lazy unmount immediately detaches the filesystem from the namespace, allowing the
// device to be deactivated without waiting for pending I/O. Any remaining dirty pages
// are flushed asynchronously in the background.
//
// WARNING: This operation can still trigger issues if called too quickly after writes.
// Always add a small delay before calling DeactivateDevice after unmount.
func (c *Client) UnmountDevice(ctx context.Context, mountPoint string) error {
	logger := c.logger.WithField("mount", mountPoint)
	logger.Info("unmounting device")

	// Check if actually mounted first
	mounted, err := c.IsMounted(mountPoint)
	if err != nil {
		logger.WithError(err).Warn("failed to check mount status")
	}
	if !mounted {
		logger.Info("device not mounted, skipping unmount")
		return nil
	}

	// Strategy 1: Lazy unmount FIRST (safest for dm-thin)
	// This immediately detaches the filesystem without waiting for I/O
	ctxTimeout1, cancel1 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel1()

	cmdArgs := []string{"-l", mountPoint}
	logger.WithFields(logrus.Fields{
		"command": "umount",
		"args":    cmdArgs,
		"timeout": "10s",
	}).Debug("executing umount -l (lazy)")

	startTime := time.Now()
	cmd := exec.CommandContext(ctxTimeout1, "umount", cmdArgs...)
	output, err := cmd.CombinedOutput()
	duration := time.Since(startTime)
	timedOut := ctxTimeout1.Err() != nil

	logger.WithFields(logrus.Fields{
		"command":     "umount -l",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
		"timed_out":   timedOut,
	}).Debug("umount -l completed")

	if err == nil {
		logger.Info("device lazy-unmounted successfully")
		return nil
	}

	outputStr := string(output)
	if strings.Contains(outputStr, "not mounted") {
		logger.Info("device not mounted")
		return nil
	}

	// Strategy 2: Force unmount if lazy unmount fails
	logger.Warn("lazy unmount failed, trying force unmount")
	ctxTimeout2, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	defer cancel2()

	cmdArgs = []string{"-f", mountPoint}
	logger.WithFields(logrus.Fields{
		"command": "umount",
		"args":    cmdArgs,
		"timeout": "10s",
	}).Debug("executing umount -f")

	startTime = time.Now()
	cmd = exec.CommandContext(ctxTimeout2, "umount", cmdArgs...)
	output2, err2 := cmd.CombinedOutput()
	duration = time.Since(startTime)
	timedOut = ctxTimeout2.Err() != nil

	logger.WithFields(logrus.Fields{
		"command":     "umount -f",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output2),
		"timed_out":   timedOut,
	}).Debug("umount -f completed")

	if err2 == nil {
		logger.Info("device force-unmounted successfully")
		return nil
	}

	// Strategy 3: Standard unmount as last resort (may block!)
	logger.Warn("force unmount failed, trying standard unmount (may block)")
	ctxTimeout3, cancel3 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel3()

	cmdArgs = []string{mountPoint}
	logger.WithFields(logrus.Fields{
		"command": "umount",
		"args":    cmdArgs,
		"timeout": "5s",
	}).Debug("executing umount")

	startTime = time.Now()
	cmd = exec.CommandContext(ctxTimeout3, "umount", cmdArgs...)
	output3, err3 := cmd.CombinedOutput()
	duration = time.Since(startTime)
	timedOut = ctxTimeout3.Err() != nil

	logger.WithFields(logrus.Fields{
		"command":     "umount",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output3),
		"timed_out":   timedOut,
	}).Debug("umount completed")

	if err3 == nil {
		logger.Info("device unmounted successfully")
		return nil
	}

	logger.WithFields(logrus.Fields{
		"error":     err.Error(),
		"output":    outputStr,
		"timed_out": timedOut,
	}).Error("all unmount strategies failed")

	return fmt.Errorf("all unmount strategies failed: %w (output: %s)", err, outputStr)
}

// GetPoolStatus returns the status of a devicemapper pool.
func (c *Client) GetPoolStatus(ctx context.Context, poolName string) (string, error) {
	logger := c.logger.WithField("pool_name", poolName)
	cmdArgs := []string{"status", poolName}
	logger.WithFields(logrus.Fields{
		"command": "dmsetup",
		"args":    cmdArgs,
	}).Debug("executing dmsetup status")

	startTime := time.Now()
	cmd := exec.CommandContext(ctx, "dmsetup", cmdArgs...)
	output, err := cmd.CombinedOutput()
	duration := time.Since(startTime)

	logger.WithFields(logrus.Fields{
		"command":     "dmsetup status",
		"duration_ms": duration.Milliseconds(),
		"exit_code":   cmd.ProcessState.ExitCode(),
		"stdout":      string(output),
	}).Debug("dmsetup status completed")

	if err != nil {
		logger.WithFields(logrus.Fields{
			"error":  err.Error(),
			"output": string(output),
		}).Error("failed to get pool status")
		return "", fmt.Errorf("failed to get pool status: %w", err)
	}
	return string(output), nil
}

// GetPoolInfo returns detailed information about a pool.
type PoolInfo struct {
	Name              string
	TotalDataBlocks   int64
	UsedDataBlocks    int64
	TotalMetaBlocks   int64
	UsedMetaBlocks    int64
	DataBlockSize     int64
	LowWaterMark      int64
	TransactionID     int64
	MetadataMode      string
	DiscardPassdown   bool
	NoDiscardPassdown bool
}

// PoolCapacityThreshold is the percentage of pool usage above which we refuse new operations.
// This prevents kernel panics caused by operating on a nearly-full thin pool.
// Set conservatively at 70% to leave headroom for CoW operations.
const PoolCapacityThreshold = 70.0

// CheckPoolCapacity checks if the pool has enough free space for an operation.
// It returns a PoolFullError if the pool is above the capacity threshold.
// This is a pre-flight check to prevent kernel panics from operating on a nearly-full pool.
//
// Parameters:
//   - ctx: Context for cancellation and timeouts
//   - poolName: Name of the devicemapper pool
//   - requiredBytes: Minimum bytes needed for the operation (0 to skip size check)
//
// Returns:
//   - *PoolInfo: Pool status information on success
//   - error: PoolFullError if pool is above threshold, other errors on failure
func (c *Client) CheckPoolCapacity(ctx context.Context, poolName string, requiredBytes int64) (*PoolInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checkPoolCapacityUnlocked(ctx, poolName, requiredBytes)
}

// checkPoolCapacityUnlocked is the internal implementation of CheckPoolCapacity.
// It must be called with the mutex already held.
func (c *Client) checkPoolCapacityUnlocked(ctx context.Context, poolName string, requiredBytes int64) (*PoolInfo, error) {
	logger := c.logger.WithFields(logrus.Fields{
		"pool":           poolName,
		"required_bytes": requiredBytes,
		"threshold":      PoolCapacityThreshold,
	})

	logger.Debug("checking pool capacity before operation")

	info, err := c.ParsePoolStatus(ctx, poolName)
	if err != nil {
		logger.WithError(err).Warn("failed to check pool capacity (continuing anyway)")
		// Don't fail the operation if we can't check - let devicemapper handle it
		return nil, nil
	}

	// Calculate usage percentage
	var usedPercent float64
	if info.TotalDataBlocks > 0 {
		usedPercent = (float64(info.UsedDataBlocks) / float64(info.TotalDataBlocks)) * 100.0
	}

	freeBlocks := info.TotalDataBlocks - info.UsedDataBlocks

	logger = logger.WithFields(logrus.Fields{
		"used_blocks":  info.UsedDataBlocks,
		"total_blocks": info.TotalDataBlocks,
		"free_blocks":  freeBlocks,
		"used_percent": usedPercent,
	})

	// Check if pool is above threshold
	if usedPercent >= PoolCapacityThreshold {
		logger.Error("pool capacity threshold exceeded - refusing operation to prevent kernel panic")
		return nil, &PoolFullError{
			PoolName:      poolName,
			UsedPercent:   usedPercent,
			Threshold:     PoolCapacityThreshold,
			UsedBlocks:    info.UsedDataBlocks,
			TotalBlocks:   info.TotalDataBlocks,
			FreeBlocks:    freeBlocks,
			RequiredBytes: requiredBytes,
		}
	}

	logger.Debug("pool has sufficient capacity")
	return info, nil
}

// ParsePoolStatus parses the output of dmsetup status for a thin-pool.
func (c *Client) ParsePoolStatus(ctx context.Context, poolName string) (*PoolInfo, error) {
	status, err := c.GetPoolStatus(ctx, poolName)
	if err != nil {
		return nil, err
	}

	// Parse status line
	// Format: 0 <size> thin-pool <transaction_id> <used_meta>/<total_meta> <used_data>/<total_data> <held_meta_root>
	parts := strings.Fields(status)
	if len(parts) < 8 {
		return nil, fmt.Errorf("invalid pool status format: %s", status)
	}

	info := &PoolInfo{
		Name: poolName,
	}

	// Parse transaction ID
	if tid, err := strconv.ParseInt(parts[3], 10, 64); err == nil {
		info.TransactionID = tid
	}

	// Parse metadata blocks (used/total)
	if metaParts := strings.Split(parts[4], "/"); len(metaParts) == 2 {
		if used, err := strconv.ParseInt(metaParts[0], 10, 64); err == nil {
			info.UsedMetaBlocks = used
		}
		if total, err := strconv.ParseInt(metaParts[1], 10, 64); err == nil {
			info.TotalMetaBlocks = total
		}
	}

	// Parse data blocks (used/total)
	if dataParts := strings.Split(parts[5], "/"); len(dataParts) == 2 {
		if used, err := strconv.ParseInt(dataParts[0], 10, 64); err == nil {
			info.UsedDataBlocks = used
		}
		if total, err := strconv.ParseInt(dataParts[1], 10, 64); err == nil {
			info.TotalDataBlocks = total
		}
	}

	return info, nil
}

// Validation functions

var (
	// deviceIDRegex matches valid device IDs (numeric)
	deviceIDRegex = regexp.MustCompile(`^[0-9]+$`)

	// deviceNameRegex matches valid device names (alphanumeric + dash/underscore)
	deviceNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

	// poolNameRegex matches valid pool names
	poolNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

func validateDeviceID(deviceID string) error {
	if deviceID == "" {
		return fmt.Errorf("device ID cannot be empty")
	}

	if len(deviceID) > 64 {
		return fmt.Errorf("device ID too long: %d characters (max 64)", len(deviceID))
	}

	if !deviceIDRegex.MatchString(deviceID) {
		return fmt.Errorf("device ID must be numeric: %s", deviceID)
	}

	return nil
}

func validateDeviceName(name string) error {
	if name == "" {
		return fmt.Errorf("device name cannot be empty")
	}

	if len(name) > 255 {
		return fmt.Errorf("device name too long: %d characters (max 255)", len(name))
	}

	if !deviceNameRegex.MatchString(name) {
		return fmt.Errorf("device name contains invalid characters: %s", name)
	}

	return nil
}

func validatePoolName(name string) error {
	if name == "" {
		return fmt.Errorf("pool name cannot be empty")
	}

	if len(name) > 255 {
		return fmt.Errorf("pool name too long: %d characters (max 255)", len(name))
	}

	if !poolNameRegex.MatchString(name) {
		return fmt.Errorf("pool name contains invalid characters: %s", name)
	}

	return nil
}

// Error types

// DeviceExistsError is returned when a device already exists.
type DeviceExistsError struct {
	DeviceID string
}

func (e *DeviceExistsError) Error() string {
	return fmt.Sprintf("device already exists: %s", e.DeviceID)
}

// PoolFullError is returned when the pool is full or near capacity.
type PoolFullError struct {
	PoolName      string
	UsedPercent   float64
	Threshold     float64
	UsedBlocks    int64
	TotalBlocks   int64
	FreeBlocks    int64
	RequiredBytes int64
}

func (e *PoolFullError) Error() string {
	if e.UsedPercent > 0 {
		return fmt.Sprintf("pool %q is %.1f%% full (threshold: %.0f%%, free: %d blocks, need: %d bytes) - run 'gc --force' to reclaim space",
			e.PoolName, e.UsedPercent, e.Threshold, e.FreeBlocks, e.RequiredBytes)
	}
	return fmt.Sprintf("pool is full: %s", e.PoolName)
}

// DeviceNotFoundError is returned when a device is not found.
type DeviceNotFoundError struct {
	DeviceID string
}

func (e *DeviceNotFoundError) Error() string {
	return fmt.Sprintf("device not found: %s", e.DeviceID)
}

// IsDeviceExistsError checks if an error is a DeviceExistsError.
func IsDeviceExistsError(err error) bool {
	_, ok := err.(*DeviceExistsError)
	return ok
}

// IsPoolFullError checks if an error is a PoolFullError.
func IsPoolFullError(err error) bool {
	_, ok := err.(*PoolFullError)
	return ok
}

// IsDeviceNotFoundError checks if an error is a DeviceNotFoundError.
func IsDeviceNotFoundError(err error) bool {
	_, ok := err.(*DeviceNotFoundError)
	return ok
}

// SyncPoolMetadata forces the thin-pool to commit its metadata to disk.
// This should be called after a sequence of device operations to ensure
// metadata consistency before any subsequent operations.
//
// PERFORMANCE OPTIMIZED: Removed redundant initial release and sleep.
// The reserve/release cycle is sufficient to trigger a metadata commit.
func (c *Client) SyncPoolMetadata(ctx context.Context, poolName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	logger := c.logger.WithField("pool", poolName)

	// Reserve a metadata snapshot (forces metadata commit)
	reserveArgs := []string{"message", poolName, "0", "reserve_metadata_snap"}
	logger.Debug("reserving metadata snapshot to force commit")
	cmd := exec.CommandContext(ctx, "dmsetup", reserveArgs...)
	if output, err := cmd.CombinedOutput(); err != nil {
		// Not fatal - some pools don't support this
		logger.WithFields(logrus.Fields{
			"error":  err.Error(),
			"output": string(output),
		}).Debug("failed to reserve metadata snapshot (may not be supported)")
		return nil
	}

	// Release the metadata snapshot immediately - no pause needed
	releaseArgs := []string{"message", poolName, "0", "release_metadata_snap"}
	logger.Debug("releasing metadata snapshot")
	cmd = exec.CommandContext(ctx, "dmsetup", releaseArgs...)
	if output, err := cmd.CombinedOutput(); err != nil {
		logger.WithFields(logrus.Fields{
			"error":  err.Error(),
			"output": string(output),
		}).Debug("failed to release metadata snapshot")
	}

	return nil
}
