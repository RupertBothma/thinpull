package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/superfly/fsm/database"
	"github.com/superfly/fsm/devicemapper"
)

var (
	// GC command flags (gcCmd is declared in main.go)
	gcDryRun     *bool
	gcForce      *bool
	gcVerbose    *bool
	gcIgnoreLock *bool
)

func init() {
	// Initialize GC flags
	gcDryRun = gcCmd.Bool("dry-run", false, "Show what would be cleaned without actually cleaning")
	gcForce = gcCmd.Bool("force", false, "Actually perform cleanup (required for non-dry-run)")
	gcVerbose = gcCmd.Bool("verbose", false, "Enable verbose logging")
	gcIgnoreLock = gcCmd.Bool("ignore-lock", false, "Ignore manager lock file (DANGEROUS - may cause kernel panics if FSMs are running)")
}

// runGC implements the garbage collection command for cleaning up orphaned devices.
func runGC(cfg Config) error {
	ctx := context.Background()

	// Validate flags
	if !*gcDryRun && !*gcForce {
		return fmt.Errorf("must specify either --dry-run or --force")
	}

	if *gcDryRun && *gcForce {
		return fmt.Errorf("cannot specify both --dry-run and --force")
	}

	// Set log level
	if *gcVerbose {
		logrus.SetLevel(logrus.DebugLevel)
	}

	logger := logrus.WithField("command", "gc")

	if *gcDryRun {
		logger.Info("Running in DRY RUN mode - no changes will be made")
	} else {
		logger.Warn("Running in FORCE mode - orphaned devices will be deleted")
	}

	// Check for manager lock file to prevent GC while FSMs are running
	// This prevents concurrent devicemapper operations that can cause kernel panics.
	lockPath := filepath.Join(cfg.FSMDBPath, "flyio-manager.lock")
	if _, err := os.Stat(lockPath); err == nil {
		// Lock file exists - another process may be running
		if !*gcIgnoreLock {
			return fmt.Errorf("FSM manager may be running (lock file exists at %s). Stop all flyio-image-manager processes first, or use --ignore-lock to override (DANGEROUS)", lockPath)
		}
		logger.Warn("WARNING: --ignore-lock specified, proceeding with GC despite active lock file. This may cause kernel panics if FSMs are running!")
	}

	// CRITICAL: Check for D-state processes before GC - these indicate kernel deadlock risk
	// D-state processes are "uninterruptible sleep" - often caused by stuck I/O operations
	// GC operations on a system with D-state processes can trigger kernel panics
	if dStateCount, err := countDStateProcesses(); err == nil && dStateCount > 0 {
		logger.WithField("d_state_count", dStateCount).Error("D-state processes detected - system may be experiencing kernel deadlock")
		if !*gcIgnoreLock {
			return fmt.Errorf("detected %d D-state processes - system may be unstable. Reboot recommended before GC. Use --ignore-lock to override (VERY DANGEROUS)", dStateCount)
		}
		logger.Warn("WARNING: D-state processes detected but --ignore-lock specified. This is VERY DANGEROUS and may cause kernel panic!")
	}

	// Initialize database
	db, err := database.New(database.Config{
		Path:            cfg.DBPath,
		MaxOpenConns:    10,
		MaxIdleConns:    5,
		ConnMaxLifetime: time.Hour,
	})
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	// Initialize devicemapper client
	dmClient := devicemapper.New()
	dmClient.SetLogger(logrus.StandardLogger())

	// Pre-flight check: Verify pool is healthy before GC
	// A corrupted or inaccessible pool can cause kernel panics during GC
	poolStatus, err := dmClient.GetPoolStatus(ctx, cfg.PoolName)
	if err != nil {
		logger.WithError(err).Error("Pool health check failed - pool may be corrupted or inaccessible")
		if !*gcIgnoreLock {
			return fmt.Errorf("pool %q health check failed: %w. Use --ignore-lock to override (DANGEROUS)", cfg.PoolName, err)
		}
		logger.Warn("WARNING: Pool health check failed but --ignore-lock specified. Proceeding anyway (DANGEROUS)")
	} else {
		logger.WithField("pool_status", strings.TrimSpace(poolStatus)).Info("Pool health check passed")
	}

	// Warn the user
	logger.Warn("IMPORTANT: Ensure no FSMs are currently running before proceeding")
	logger.Warn("IMPORTANT: This command should only be run when the system is idle")

	// Run garbage collection
	result, err := garbageCollectOrphanedDevices(ctx, db, dmClient, cfg.PoolName, *gcDryRun)
	if err != nil {
		return fmt.Errorf("garbage collection failed: %w", err)
	}

	// Print summary
	logger.Info("=== Garbage Collection Summary ===")
	logger.WithFields(logrus.Fields{
		"total_devices": result.TotalDevices,
		"orphaned":      result.OrphanedCount,
		"cleaned":       result.CleanedCount,
		"failed":        result.FailedCount,
		"skipped":       result.SkippedCount,
	}).Info("Summary")

	if *gcDryRun {
		logger.Info("DRY RUN complete - no changes were made")
		logger.Info("Run with --force to actually clean up orphaned devices")
	} else {
		logger.Info("Garbage collection complete")
	}

	if result.FailedCount > 0 {
		logger.Warn("Some devices could not be cleaned - manual intervention may be required")
		logger.Warn("Consider rebooting the system if devices are stuck in D-state")
	}

	return nil
}

// GCResult contains the results of a garbage collection run.
type GCResult struct {
	TotalDevices  int
	OrphanedCount int
	CleanedCount  int
	FailedCount   int
	SkippedCount  int
	Orphans       []OrphanedDevice
}

// OrphanedDevice represents a device that exists in devicemapper but not in the database.
type OrphanedDevice struct {
	DeviceName string
	DeviceID   string
	Mounted    bool
	Cleaned    bool
	Failed     bool
	Skipped    bool
	Error      string
}

// garbageCollectOrphanedDevices identifies and cleans up orphaned devices.
func garbageCollectOrphanedDevices(ctx context.Context, db *database.DB, dmClient *devicemapper.Client, poolName string, dryRun bool) (*GCResult, error) {
	logger := logrus.WithField("function", "garbageCollectOrphanedDevices")

	result := &GCResult{
		Orphans: []OrphanedDevice{},
	}

	// Step 1: Get all thin devices from devicemapper
	logger.Info("Step 1: Querying devicemapper for thin devices")
	dmDevices, err := listThinDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list devicemapper devices: %w", err)
	}

	result.TotalDevices = len(dmDevices)
	logger.WithField("count", result.TotalDevices).Info("Found thin devices in devicemapper")

	// Step 2: Get all device records from database
	logger.Info("Step 2: Querying database for device records")
	dbDevices, err := db.ListUnpackedImages(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list database devices: %w", err)
	}

	logger.WithField("count", len(dbDevices)).Info("Found device records in database")

	// Step 3: Build a map of database device names for quick lookup
	dbDeviceMap := make(map[string]bool)
	for _, dev := range dbDevices {
		dbDeviceMap[dev.DeviceName] = true
	}

	// Step 4: Identify orphaned devices (in devicemapper but not in database)
	logger.Info("Step 3: Identifying orphaned devices")
	for _, dmDevice := range dmDevices {
		if !dbDeviceMap[dmDevice.Name] {
			// This device is orphaned
			orphan := OrphanedDevice{
				DeviceName: dmDevice.Name,
				DeviceID:   dmDevice.ID,
			}

			// Check if device is mounted
			mounted, err := isDeviceMounted(dmDevice.Name)
			if err != nil {
				logger.WithError(err).WithField("device", dmDevice.Name).Warn("Failed to check mount status")
			}
			orphan.Mounted = mounted

			result.Orphans = append(result.Orphans, orphan)
			result.OrphanedCount++

			logger.WithFields(logrus.Fields{
				"device_name": dmDevice.Name,
				"device_id":   dmDevice.ID,
				"mounted":     mounted,
			}).Warn("Found orphaned device")
		}
	}

	if result.OrphanedCount == 0 {
		logger.Info("No orphaned devices found")
		return result, nil
	}

	logger.WithField("count", result.OrphanedCount).Warn("Found orphaned devices")

	// Step 5: Clean up orphaned devices (if not dry run)
	if !dryRun {
		// Pre-cleanup: Sync pool metadata and wait for I/O to settle
		// This helps prevent kernel panics by ensuring the pool is in a consistent state
		logger.Info("Step 4a: Syncing pool metadata before cleanup")
		if err := dmClient.SyncPoolMetadata(ctx, poolName); err != nil {
			logger.WithError(err).Warn("Pool metadata sync failed (continuing anyway)")
		}
		time.Sleep(1 * time.Second) // Allow kernel time to process

		logger.Info("Step 4b: Cleaning up orphaned devices (one at a time with delays)")
		for i := range result.Orphans {
			orphan := &result.Orphans[i]
			cleanupOrphanedDevice(ctx, dmClient, poolName, orphan)

			if orphan.Cleaned {
				result.CleanedCount++
				// Wait between successful cleanups to let the kernel settle
				time.Sleep(50 * time.Millisecond)
			} else if orphan.Failed {
				result.FailedCount++
			} else if orphan.Skipped {
				result.SkippedCount++
			}
		}

		// Post-cleanup: Sync pool metadata again
		logger.Info("Step 4c: Syncing pool metadata after cleanup")
		if err := dmClient.SyncPoolMetadata(ctx, poolName); err != nil {
			logger.WithError(err).Warn("Post-cleanup pool metadata sync failed")
		}
	} else {
		logger.Info("DRY RUN: Skipping cleanup")
	}

	return result, nil
}

// DeviceInfo represents a devicemapper device.
type DeviceInfo struct {
	Name string
	ID   string
}

// listThinDevices lists all thin-* devices from devicemapper.
func listThinDevices(ctx context.Context) ([]DeviceInfo, error) {
	cmd := exec.CommandContext(ctx, "dmsetup", "ls")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("dmsetup ls failed: %w (output: %s)", err, string(output))
	}

	devices := []DeviceInfo{}
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 1 {
			continue
		}

		deviceName := parts[0]
		if !strings.HasPrefix(deviceName, "thin-") {
			continue
		}

		deviceID := strings.TrimPrefix(deviceName, "thin-")
		devices = append(devices, DeviceInfo{
			Name: deviceName,
			ID:   deviceID,
		})
	}

	return devices, nil
}

// isDeviceMounted checks if a device is currently mounted.
func isDeviceMounted(deviceName string) (bool, error) {
	cmd := exec.Command("grep", "-q", deviceName, "/proc/mounts")
	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// cleanupOrphanedDevice attempts to safely clean up a single orphaned device.
// CRITICAL: This function must be extremely careful to avoid kernel panics.
// We use --verifyudev for udev synchronization and add delays between operations.
func cleanupOrphanedDevice(ctx context.Context, dmClient *devicemapper.Client, poolName string, orphan *OrphanedDevice) {
	logger := logrus.WithFields(logrus.Fields{
		"device_name": orphan.DeviceName,
		"device_id":   orphan.DeviceID,
	})

	logger.Info("Attempting to clean up orphaned device")

	// Skip if device is mounted
	if orphan.Mounted {
		logger.Warn("Device is mounted - skipping cleanup (unmount manually first)")
		orphan.Skipped = true
		orphan.Error = "device is mounted"
		return
	}

	// Pre-cleanup: Wait for any pending I/O to settle
	logger.Debug("Waiting for I/O to settle before cleanup...")
	time.Sleep(500 * time.Millisecond)

	// Step 1: Try to unmount (in case it's mounted but we missed it)
	logger.Debug("Step 1: Attempting unmount")
	if err := unmountDeviceWithTimeout(ctx, orphan.DeviceName, 10*time.Second); err != nil {
		logger.WithError(err).Warn("Unmount failed or timed out (may not have been mounted)")
		// Continue anyway - device might not have been mounted
	}

	// Wait for udev to process unmount event
	time.Sleep(500 * time.Millisecond)

	// Step 2: Suspend the device first (safer than direct remove)
	logger.Debug("Step 2: Suspending device before removal")
	if err := suspendDeviceWithTimeout(ctx, orphan.DeviceName, 10*time.Second); err != nil {
		logger.WithError(err).Warn("Suspend failed (continuing with removal)")
	} else {
		time.Sleep(300 * time.Millisecond) // Wait for suspend to take effect
	}

	// Step 3: Try to deactivate with --verifyudev
	logger.Debug("Step 3: Attempting deactivate with udev sync")
	if err := deactivateDeviceWithTimeout(ctx, dmClient, orphan.DeviceName, 15*time.Second); err != nil {
		logger.WithError(err).Error("Deactivate failed or timed out")
		orphan.Failed = true
		orphan.Error = fmt.Sprintf("deactivate failed: %v", err)
		return
	}

	// Wait for udev to process device removal
	time.Sleep(500 * time.Millisecond)

	// Step 4: Try to delete from thin pool
	logger.Debug("Step 4: Attempting delete from thin pool")
	if err := deleteThinDeviceWithTimeout(ctx, poolName, orphan.DeviceID, 10*time.Second); err != nil {
		logger.WithError(err).Error("Delete failed or timed out")
		orphan.Failed = true
		orphan.Error = fmt.Sprintf("delete failed: %v", err)
		return
	}

	// Post-cleanup: Wait for pool metadata to settle
	time.Sleep(300 * time.Millisecond)

	logger.Info("Successfully cleaned up orphaned device")
	orphan.Cleaned = true
}

// unmountDeviceWithTimeout attempts to unmount a device with a timeout.
func unmountDeviceWithTimeout(ctx context.Context, deviceName string, timeout time.Duration) error {
	ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	devicePath := "/dev/mapper/" + deviceName
	cmd := exec.CommandContext(ctxWithTimeout, "umount", devicePath)
	output, err := cmd.CombinedOutput()

	if ctxWithTimeout.Err() == context.DeadlineExceeded {
		return fmt.Errorf("unmount timed out after %v", timeout)
	}

	if err != nil {
		// Ignore "not mounted" errors
		if strings.Contains(string(output), "not mounted") {
			return nil
		}
		return fmt.Errorf("unmount failed: %w (output: %s)", err, string(output))
	}

	return nil
}

// suspendDeviceWithTimeout suspends a device before removal to prevent I/O races.
// This is safer than directly removing a device that might have pending I/O.
func suspendDeviceWithTimeout(ctx context.Context, deviceName string, timeout time.Duration) error {
	ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctxWithTimeout, "dmsetup", "suspend", "--nolockfs", deviceName)
	output, err := cmd.CombinedOutput()

	if ctxWithTimeout.Err() == context.DeadlineExceeded {
		return fmt.Errorf("suspend timed out after %v", timeout)
	}

	if err != nil {
		outputStr := string(output)
		// Ignore "not found" errors
		if strings.Contains(outputStr, "not found") || strings.Contains(outputStr, "No such") {
			return nil
		}
		return fmt.Errorf("suspend failed: %w (output: %s)", err, outputStr)
	}

	return nil
}

// deactivateDeviceWithTimeout attempts to deactivate a device with a timeout.
// Uses --verifyudev to properly synchronize with udev, preventing race conditions
// that can lead to kernel panics.
func deactivateDeviceWithTimeout(ctx context.Context, dmClient *devicemapper.Client, deviceName string, timeout time.Duration) error {
	ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// First try with --verifyudev for proper udev synchronization
	cmd := exec.CommandContext(ctxWithTimeout, "dmsetup", "remove", "--verifyudev", deviceName)
	output, err := cmd.CombinedOutput()

	if err == nil {
		return nil
	}

	outputStr := string(output)
	// Check for "not found" - device already gone
	if strings.Contains(outputStr, "not found") || strings.Contains(outputStr, "No such") {
		return nil
	}

	// If --verifyudev fails, try without it as fallback
	if ctxWithTimeout.Err() == nil {
		logrus.WithField("device", deviceName).Warn("--verifyudev remove failed, trying standard remove")
		cmd = exec.CommandContext(ctxWithTimeout, "dmsetup", "remove", deviceName)
		output, err = cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		outputStr = string(output)
		if strings.Contains(outputStr, "not found") || strings.Contains(outputStr, "No such") {
			return nil
		}
	}

	if ctxWithTimeout.Err() == context.DeadlineExceeded {
		return fmt.Errorf("deactivate timed out after %v", timeout)
	}

	return fmt.Errorf("deactivate failed: %w (output: %s)", err, outputStr)
}

// deleteThinDeviceWithTimeout attempts to delete a thin device with a timeout.
func deleteThinDeviceWithTimeout(ctx context.Context, poolName string, deviceID string, timeout time.Duration) error {
	ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctxWithTimeout, "dmsetup", "message", poolName, "0", fmt.Sprintf("delete %s", deviceID))
	output, err := cmd.CombinedOutput()

	if ctxWithTimeout.Err() == context.DeadlineExceeded {
		return fmt.Errorf("delete timed out after %v", timeout)
	}

	if err != nil {
		return fmt.Errorf("delete failed: %w (output: %s)", err, string(output))
	}

	return nil
}

// countDStateProcesses counts the number of processes in D-state (uninterruptible sleep).
// D-state processes indicate potential kernel deadlock, often caused by stuck I/O operations.
// Running devicemapper operations when D-state processes exist can trigger kernel panics.
func countDStateProcesses() (int, error) {
	// Use ps to find D-state processes (state column contains 'D')
	// Exclude kernel threads (those in brackets like [kworker/...])
	cmd := exec.Command("sh", "-c", "ps aux | awk '$8 ~ /D/ && $11 !~ /^\\[/ {count++} END {print count+0}'")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("failed to check D-state processes: %w", err)
	}

	countStr := strings.TrimSpace(string(output))
	count := 0
	if countStr != "" && countStr != "0" {
		if _, err := fmt.Sscanf(countStr, "%d", &count); err != nil {
			return 0, fmt.Errorf("failed to parse D-state count: %w", err)
		}
	}

	return count, nil
}
