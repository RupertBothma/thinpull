// Package devicemapper pool management utilities.
//
// This file provides pool setup, validation, and recovery functions
// for managing dm-thin pools, especially after reboots or kernel panics.
package devicemapper

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// PoolConfig contains configuration for pool setup.
type PoolConfig struct {
	// PoolName is the name of the thin pool (default: "pool")
	PoolName string
	// DataDir is the directory where pool files are stored
	DataDir string
	// DataSizeBytes is the size of the data device (default: 2GB)
	DataSizeBytes int64
	// MetaSizeBytes is the size of the metadata device (default: 1MB)
	MetaSizeBytes int64
	// DataBlockSize is the block size for data in sectors (default: 2048 = 1MB)
	DataBlockSize int
	// LowWaterMark is the low water mark in blocks (default: 32768)
	LowWaterMark int
}

// DefaultPoolConfig returns the default pool configuration.
func DefaultPoolConfig(dataDir string) PoolConfig {
	return PoolConfig{
		PoolName:      "pool",
		DataDir:       dataDir,
		DataSizeBytes: 2 * 1024 * 1024 * 1024, // 2GB
		MetaSizeBytes: 1 * 1024 * 1024,         // 1MB
		DataBlockSize: 2048,                    // 1MB blocks
		LowWaterMark:  32768,
	}
}

// PoolStatus represents the status of a thin pool.
type PoolStatus struct {
	Exists         bool
	NeedsCheck     bool
	MetadataUsed   int64
	MetadataTotal  int64
	DataUsed       int64
	DataTotal      int64
	ReadOnly       bool
	ErrorState     string
	LoopDataDevice string
	LoopMetaDevice string
}

// PoolManager manages thin pool lifecycle.
type PoolManager struct {
	config PoolConfig
	logger logrus.FieldLogger
}

// NewPoolManager creates a new pool manager.
func NewPoolManager(config PoolConfig, logger logrus.FieldLogger) *PoolManager {
	if logger == nil {
		logger = logrus.StandardLogger()
	}
	return &PoolManager{
		config: config,
		logger: logger.WithField("component", "pool-manager"),
	}
}

// GetPoolStatus returns the current status of the thin pool.
func (pm *PoolManager) GetPoolStatus(ctx context.Context) (*PoolStatus, error) {
	status := &PoolStatus{}

	// Check if pool exists
	cmd := exec.CommandContext(ctx, "dmsetup", "status", pm.config.PoolName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "Device does not exist") {
			status.Exists = false
			return status, nil
		}
		return nil, fmt.Errorf("failed to check pool status: %w (output: %s)", err, output)
	}

	status.Exists = true
	outputStr := string(output)
	if strings.Contains(outputStr, "needs_check") {
		status.NeedsCheck = true
	}
	if strings.Contains(outputStr, "ro ") || strings.Contains(outputStr, " ro") {
		status.ReadOnly = true
	}
	if strings.Contains(outputStr, "Error") || strings.Contains(outputStr, "error") {
		status.ErrorState = "error detected in pool status"
	}

	// Try to find loop devices
	status.LoopDataDevice = pm.findLoopDevice(ctx, filepath.Join(pm.config.DataDir, "pool_data"))
	status.LoopMetaDevice = pm.findLoopDevice(ctx, filepath.Join(pm.config.DataDir, "pool_meta"))

	return status, nil
}

// findLoopDevice finds the loop device for a given file.
func (pm *PoolManager) findLoopDevice(ctx context.Context, filePath string) string {
	cmd := exec.CommandContext(ctx, "losetup", "-j", filePath)
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	parts := strings.Split(string(output), ":")
	if len(parts) > 0 && strings.HasPrefix(parts[0], "/dev/loop") {
		return strings.TrimSpace(parts[0])
	}
	return ""
}

// EnsurePoolExists checks if the pool exists and creates it if needed.
func (pm *PoolManager) EnsurePoolExists(ctx context.Context) error {
	pm.logger.Info("checking pool status")

	status, err := pm.GetPoolStatus(ctx)
	if err != nil {
		return fmt.Errorf("failed to get pool status: %w", err)
	}

	if status.Exists {
		pm.logger.WithFields(logrus.Fields{
			"needs_check": status.NeedsCheck,
			"read_only":   status.ReadOnly,
		}).Info("pool exists")

		if status.NeedsCheck {
			return fmt.Errorf("pool exists but needs_check flag is set - manual intervention required")
		}
		if status.ReadOnly {
			return fmt.Errorf("pool exists but is read-only - may need recreation")
		}
		if status.ErrorState != "" {
			return fmt.Errorf("pool exists but has error: %s", status.ErrorState)
		}
		return nil
	}

	pm.logger.Warn("pool does not exist, attempting to create")
	return pm.CreatePool(ctx)
}

// CreatePool creates a new thin pool from scratch.
func (pm *PoolManager) CreatePool(ctx context.Context) error {
	pm.logger.WithFields(logrus.Fields{
		"data_dir":  pm.config.DataDir,
		"data_size": pm.config.DataSizeBytes,
		"meta_size": pm.config.MetaSizeBytes,
		"pool_name": pm.config.PoolName,
	}).Info("creating new thin pool")

	if err := os.MkdirAll(pm.config.DataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}

	metaPath := filepath.Join(pm.config.DataDir, "pool_meta")
	dataPath := filepath.Join(pm.config.DataDir, "pool_data")

	pm.cleanupExistingLoops(ctx, metaPath, dataPath)

	if err := pm.createPoolFile(metaPath, pm.config.MetaSizeBytes); err != nil {
		return fmt.Errorf("failed to create metadata file: %w", err)
	}
	if err := pm.createPoolFile(dataPath, pm.config.DataSizeBytes); err != nil {
		return fmt.Errorf("failed to create data file: %w", err)
	}

	metaDev, err := pm.setupLoopDevice(ctx, metaPath)
	if err != nil {
		return fmt.Errorf("failed to setup metadata loop device: %w", err)
	}
	pm.logger.WithField("device", metaDev).Info("metadata loop device created")

	dataDev, err := pm.setupLoopDevice(ctx, dataPath)
	if err != nil {
		return fmt.Errorf("failed to setup data loop device: %w", err)
	}
	pm.logger.WithField("device", dataDev).Info("data loop device created")

	poolSectors := pm.config.DataSizeBytes / 512
	table := fmt.Sprintf("0 %d thin-pool %s %s %d %d",
		poolSectors, metaDev, dataDev, pm.config.DataBlockSize, pm.config.LowWaterMark)

	cmd := exec.CommandContext(ctx, "dmsetup", "create", "--verifyudev", pm.config.PoolName, "--table", table)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to create pool: %w (output: %s)", err, output)
	}

	pm.logger.Info("thin pool created successfully")
	return pm.verifyPool(ctx)
}

func (pm *PoolManager) createPoolFile(path string, size int64) error {
	os.Remove(path)
	cmd := exec.Command("fallocate", "-l", fmt.Sprintf("%d", size), path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("fallocate failed: %w (output: %s)", err, output)
	}
	return nil
}

func (pm *PoolManager) setupLoopDevice(ctx context.Context, filePath string) (string, error) {
	cmd := exec.CommandContext(ctx, "losetup", "-f", "--show", filePath)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("losetup failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func (pm *PoolManager) cleanupExistingLoops(ctx context.Context, paths ...string) {
	for _, path := range paths {
		dev := pm.findLoopDevice(ctx, path)
		if dev != "" {
			exec.CommandContext(ctx, "losetup", "-d", dev).Run()
		}
	}
}

func (pm *PoolManager) verifyPool(ctx context.Context) error {
	status, err := pm.GetPoolStatus(ctx)
	if err != nil {
		return fmt.Errorf("failed to verify pool: %w", err)
	}
	if !status.Exists {
		return fmt.Errorf("pool verification failed: pool does not exist after creation")
	}
	if status.NeedsCheck {
		return fmt.Errorf("pool verification failed: needs_check flag set")
	}
	pm.logger.Info("pool verified successfully")
	return nil
}

// ValidatePoolHealth performs comprehensive pool health checks.
func (pm *PoolManager) ValidatePoolHealth(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	status, err := pm.GetPoolStatus(checkCtx)
	if err != nil {
		return fmt.Errorf("pool health check failed: %w", err)
	}
	if !status.Exists {
		return fmt.Errorf("pool does not exist")
	}
	if status.NeedsCheck {
		return fmt.Errorf("pool needs_check flag is set - corruption detected")
	}
	if status.ReadOnly {
		return fmt.Errorf("pool is in read-only mode")
	}
	if status.ErrorState != "" {
		return fmt.Errorf("pool error: %s", status.ErrorState)
	}
	return nil
}

// DestroyPool removes the thin pool and cleans up resources.
func (pm *PoolManager) DestroyPool(ctx context.Context) error {
	pm.logger.Warn("destroying thin pool")

	cmd := exec.CommandContext(ctx, "dmsetup", "remove", pm.config.PoolName)
	if output, err := cmd.CombinedOutput(); err != nil {
		pm.logger.WithError(err).WithField("output", string(output)).Warn("failed to remove pool device")
	}

	metaPath := filepath.Join(pm.config.DataDir, "pool_meta")
	dataPath := filepath.Join(pm.config.DataDir, "pool_data")
	pm.cleanupExistingLoops(ctx, metaPath, dataPath)

	os.Remove(metaPath)
	os.Remove(dataPath)

	pm.logger.Info("pool destroyed")
	return nil
}
