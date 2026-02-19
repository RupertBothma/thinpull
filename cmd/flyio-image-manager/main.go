// Package main implements the Fly.io Container Image Management System daemon.
//
// This application orchestrates the Download, Unpack, and Activate FSMs to process
// container images from S3, extract them into devicemapper devices, and create
// copy-on-write snapshots for activation.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	stdlog "log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sirupsen/logrus"

	fsm "github.com/superfly/fsm"
	"github.com/superfly/fsm/activate"
	"github.com/superfly/fsm/database"
	"github.com/superfly/fsm/devicemapper"
	"github.com/superfly/fsm/download"
	"github.com/superfly/fsm/extraction"
	"github.com/superfly/fsm/s3"
	"github.com/superfly/fsm/safeguards"
	"github.com/superfly/fsm/tui"
	"github.com/superfly/fsm/unpack"
)

// Config holds application configuration.
type Config struct {
	// S3 Configuration
	S3Bucket string
	S3Region string

	// Database Configuration
	DBPath string

	// FSM Configuration
	FSMDBPath string

	// DeviceMapper Configuration
	PoolName  string
	MountRoot string

	// Storage Configuration
	LocalDir string

	// Queue Configuration
	DownloadQueueSize int
	UnpackQueueSize   int

	// Timeout Configuration
	DownloadTimeout time.Duration
	UnpackTimeout   time.Duration

	// Logging
	LogLevel string

	// Command-specific flags
	S3Key      string
	ImageID    string
	AutoDerive bool // Auto-derive image ID from S3 key

	// TUI flags
	Quiet  bool // Suppress progress output
	Inline bool // Run TUI inline (no alt-screen) for monitor command
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{
		S3Bucket:          "flyio-container-images",
		S3Region:          "us-east-1",
		DBPath:            "/var/lib/flyio/images.db",
		FSMDBPath:         "/var/lib/flyio/fsm",
		PoolName:          "pool",
		MountRoot:         "/mnt/flyio",
		LocalDir:          "/var/lib/flyio/images",
		DownloadQueueSize: 5,
		UnpackQueueSize:   1, // serialize devicemapper-heavy unpack operations
		DownloadTimeout:   5 * time.Minute,
		UnpackTimeout:     30 * time.Minute,
		LogLevel:          "info",
	}
}

var (
	// Global logger
	log = logrus.New()

	// Global operation guard for serializing devicemapper operations
	operationGuard *safeguards.OperationGuard

	// Global pool manager for pool lifecycle management
	poolManager *devicemapper.PoolManager

	// Command flags
	processCmd    = flag.NewFlagSet("process-image", flag.ExitOnError)
	listImagesCmd = flag.NewFlagSet("list-images", flag.ExitOnError)
	listSnapsCmd  = flag.NewFlagSet("list-snapshots", flag.ExitOnError)
	daemonCmd     = flag.NewFlagSet("daemon", flag.ExitOnError)
	gcCmd         = flag.NewFlagSet("gc", flag.ExitOnError)
	monitorCmd    = flag.NewFlagSet("monitor", flag.ExitOnError)
	setupPoolCmd  = flag.NewFlagSet("setup-pool", flag.ExitOnError)
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	// Parse global flags
	config := DefaultConfig()

	switch os.Args[1] {
	case "process-image":
		parseProcessImageFlags(&config, processCmd, os.Args[2:])
		if err := runProcessImage(config); err != nil {
			log.WithError(err).Fatal("failed to process image")
		}
	case "list-images":
		parseListImagesFlags(&config, listImagesCmd, os.Args[2:])
		if err := runListImages(config); err != nil {
			log.WithError(err).Fatal("failed to list images")
		}
	case "list-snapshots":
		parseListSnapshotsFlags(&config, listSnapsCmd, os.Args[2:])
		if err := runListSnapshots(config); err != nil {
			log.WithError(err).Fatal("failed to list snapshots")
		}
	case "daemon":
		parseDaemonFlags(&config, daemonCmd, os.Args[2:])
		if err := runDaemon(config); err != nil {
			log.WithError(err).Fatal("daemon failed")
		}
	case "gc":
		parseGCFlags(&config, gcCmd, os.Args[2:])
		if err := runGC(config); err != nil {
			log.WithError(err).Fatal("garbage collection failed")
		}
	case "monitor":
		parseMonitorFlags(&config, monitorCmd, os.Args[2:])
		if err := runMonitor(config); err != nil {
			log.WithError(err).Fatal("monitor failed")
		}
	case "setup-pool":
		parseSetupPoolFlags(&config, setupPoolCmd, os.Args[2:])
		if err := runSetupPool(config); err != nil {
			log.WithError(err).Fatal("pool setup failed")
		}
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Fly.io Container Image Management System")
	fmt.Println()
	fmt.Println("Usage: flyio-image-manager <command> [options]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  process-image     Process a container image (download → unpack → activate)")
	fmt.Println("  list-images       List downloaded images")
	fmt.Println("  list-snapshots    List active snapshots")
	fmt.Println("  daemon            Run as a daemon (future: API server)")
	fmt.Println("  gc                Garbage collect orphaned devices")
	fmt.Println("  monitor           Interactive TUI dashboard for live FSM tracking")
	fmt.Println("  setup-pool        Setup or recreate the devicemapper thin-pool")
	fmt.Println()
	fmt.Println("Run 'flyio-image-manager <command> --help' for more information on a command.")
}

// parseProcessImageFlags parses flags for the process-image command.
func parseProcessImageFlags(cfg *Config, fs *flag.FlagSet, args []string) {
	fs.StringVar(&cfg.S3Key, "s3-key", "", "S3 object key (required)")
	fs.StringVar(&cfg.ImageID, "image-id", "", "Image identifier (auto-derived from s3-key if omitted)")
	fs.BoolVar(&cfg.AutoDerive, "auto-derive", true, "Auto-derive image ID from S3 key")
	fs.StringVar(&cfg.S3Bucket, "bucket", cfg.S3Bucket, "S3 bucket name")
	fs.StringVar(&cfg.S3Region, "region", cfg.S3Region, "S3 region")
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "Database path")
	fs.StringVar(&cfg.FSMDBPath, "fsm-db", cfg.FSMDBPath, "FSM database directory")
	fs.StringVar(&cfg.PoolName, "pool", cfg.PoolName, "DeviceMapper pool name")
	fs.StringVar(&cfg.MountRoot, "mount-root", cfg.MountRoot, "Mount root directory")
	fs.StringVar(&cfg.LocalDir, "local-dir", cfg.LocalDir, "Local storage directory")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level (debug, info, warn, error)")
	fs.BoolVar(&cfg.Quiet, "quiet", false, "Suppress progress output (for scripting)")

	fs.Parse(args)

	if cfg.S3Key == "" {
		fmt.Println("Error: --s3-key is required")
		fs.Usage()
		os.Exit(1)
	}

	// Auto-derive image ID from S3 key if not provided
	if cfg.ImageID == "" && cfg.AutoDerive {
		cfg.ImageID = fsm.DeriveImageIDFromS3Key(cfg.S3Key)
	}

	if cfg.ImageID == "" {
		fmt.Println("Error: --image-id is required (or use --auto-derive)")
		fs.Usage()
		os.Exit(1)
	}
}

// parseListImagesFlags parses flags for the list-images command.
func parseListImagesFlags(cfg *Config, fs *flag.FlagSet, args []string) {
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "Database path")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level")
	fs.Parse(args)
}

// parseListSnapshotsFlags parses flags for the list-snapshots command.
func parseListSnapshotsFlags(cfg *Config, fs *flag.FlagSet, args []string) {
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "Database path")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level")
	fs.Parse(args)
}

// parseDaemonFlags parses flags for the daemon command.
func parseDaemonFlags(cfg *Config, fs *flag.FlagSet, args []string) {
	fs.StringVar(&cfg.S3Bucket, "bucket", cfg.S3Bucket, "S3 bucket name")
	fs.StringVar(&cfg.S3Region, "region", cfg.S3Region, "S3 region")
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "Database path")
	fs.StringVar(&cfg.FSMDBPath, "fsm-db", cfg.FSMDBPath, "FSM database directory")
	fs.StringVar(&cfg.PoolName, "pool", cfg.PoolName, "DeviceMapper pool name")
	fs.StringVar(&cfg.MountRoot, "mount-root", cfg.MountRoot, "Mount root directory")
	fs.StringVar(&cfg.LocalDir, "local-dir", cfg.LocalDir, "Local storage directory")
	fs.IntVar(&cfg.DownloadQueueSize, "download-queue", cfg.DownloadQueueSize, "Download queue size")
	fs.IntVar(&cfg.UnpackQueueSize, "unpack-queue", cfg.UnpackQueueSize, "Unpack queue size")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level")
	fs.Parse(args)
}

// parseGCFlags parses flags for the gc command.
func parseGCFlags(cfg *Config, fs *flag.FlagSet, args []string) {
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "Database path")
	fs.StringVar(&cfg.PoolName, "pool", cfg.PoolName, "DeviceMapper pool name")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level")
	fs.Parse(args)
}

// parseMonitorFlags parses flags for the monitor command.
func parseMonitorFlags(cfg *Config, fs *flag.FlagSet, args []string) {
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "Database path")
	fs.StringVar(&cfg.FSMDBPath, "fsm-db", cfg.FSMDBPath, "FSM database directory")
	fs.StringVar(&cfg.PoolName, "pool", cfg.PoolName, "DeviceMapper pool name")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level")
	fs.BoolVar(&cfg.Inline, "inline", false, "Run inline (no alt-screen, for SSH/scripting)")
	fs.Parse(args)
}

// parseSetupPoolFlags parses flags for the setup-pool command.
func parseSetupPoolFlags(cfg *Config, fs *flag.FlagSet, args []string) {
	fs.StringVar(&cfg.DBPath, "db", cfg.DBPath, "Database path (pool files stored in same directory)")
	fs.StringVar(&cfg.PoolName, "pool", cfg.PoolName, "DeviceMapper pool name")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level")
	fs.Parse(args)
}

// runSetupPool creates or recreates the devicemapper thin-pool.
func runSetupPool(cfg Config) error {
	if err := setupLogger(cfg.LogLevel); err != nil {
		return err
	}

	ctx := context.Background()

	// Initialize pool manager
	poolConfig := devicemapper.DefaultPoolConfig(filepath.Dir(cfg.DBPath))
	poolConfig.PoolName = cfg.PoolName
	pm := devicemapper.NewPoolManager(poolConfig, log)

	// Check current status
	status, err := pm.GetPoolStatus(ctx)
	if err != nil {
		return fmt.Errorf("failed to get pool status: %w", err)
	}

	if status.Exists {
		log.WithFields(logrus.Fields{
			"pool_name":   cfg.PoolName,
			"needs_check": status.NeedsCheck,
			"read_only":   status.ReadOnly,
		}).Info("pool already exists")

		if status.NeedsCheck || status.ReadOnly || status.ErrorState != "" {
			log.Warn("pool has issues, consider destroying and recreating")
			fmt.Println("Pool exists but has issues. To recreate:")
			fmt.Printf("  1. sudo dmsetup remove %s\n", cfg.PoolName)
			fmt.Printf("  2. sudo flyio-image-manager setup-pool --db %s\n", cfg.DBPath)
		} else {
			fmt.Printf("Pool '%s' is healthy and ready.\n", cfg.PoolName)
		}
		return nil
	}

	// Pool doesn't exist - create it
	log.Info("creating thin-pool")
	if err := pm.CreatePool(ctx); err != nil {
		return fmt.Errorf("failed to create pool: %w", err)
	}

	fmt.Printf("Pool '%s' created successfully.\n", cfg.PoolName)
	fmt.Println("Pool files created in:", filepath.Dir(cfg.DBPath))
	return nil
}

// setupLogger configures the global logger.
func setupLogger(level string) error {
	log.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: time.RFC3339Nano,
	})

	lvl, err := logrus.ParseLevel(level)
	if err != nil {
		return fmt.Errorf("invalid log level: %w", err)
	}
	log.SetLevel(lvl)

	return nil
}

// lockFileInfo contains metadata written to the manager lock file.
type lockFileInfo struct {
	PID       int    `json:"pid"`
	Timestamp int64  `json:"timestamp"`
	Command   string `json:"command"`
}

// acquireManagerLock creates a lock file to prevent concurrent manager processes.
// This prevents multiple flyio-image-manager processes from running simultaneously,
// which could cause concurrent devicemapper operations and kernel panics.
//
// The lock file contains the process ID, timestamp, and command name for debugging.
// Returns an error if the lock file already exists (another process is running).
//
// CRITICAL: Uses O_EXCL flag for atomic lock acquisition to prevent race conditions.
// This is essential for kernel panic prevention - without atomic locking, two processes
// can both pass the existence check and start concurrent devicemapper operations.
func acquireManagerLock(fsmDBPath string) error {
	lockPath := filepath.Join(fsmDBPath, "flyio-manager.lock")

	// Ensure the FSMDBPath directory exists
	if err := os.MkdirAll(fsmDBPath, 0755); err != nil {
		return fmt.Errorf("failed to create FSM DB directory: %w", err)
	}

	// Create lock file with process metadata
	info := lockFileInfo{
		PID:       os.Getpid(),
		Timestamp: time.Now().Unix(),
		Command:   filepath.Base(os.Args[0]),
	}
	if len(os.Args) > 1 {
		info.Command = os.Args[1] // Use subcommand name (process-image, daemon, etc.)
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal lock file info: %w", err)
	}

	// CRITICAL: Use O_EXCL for atomic lock acquisition - prevents TOCTOU race condition
	// This ensures only ONE process can create the lock file, even if multiple processes
	// attempt acquisition simultaneously (which caused the kernel panic earlier).
	file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		if os.IsExist(err) {
			// Lock file exists - read it for diagnostic info
			existingData, readErr := os.ReadFile(lockPath)
			if readErr == nil {
				var existingInfo lockFileInfo
				if json.Unmarshal(existingData, &existingInfo) == nil {
					// Check if the process is still running
					if isProcessRunning(existingInfo.PID) {
						return fmt.Errorf("another flyio-image-manager process is running (PID %d, command: %s, started: %s). Wait for it to complete or remove the lock file at %s",
							existingInfo.PID, existingInfo.Command, time.Unix(existingInfo.Timestamp, 0).Format(time.RFC3339), lockPath)
					}
					// Process is dead - stale lock file
					log.WithFields(logrus.Fields{
						"stale_pid":   existingInfo.PID,
						"lock_path":   lockPath,
						"stale_since": time.Unix(existingInfo.Timestamp, 0).Format(time.RFC3339),
					}).Warn("removing stale lock file from dead process")
					if removeErr := os.Remove(lockPath); removeErr != nil {
						return fmt.Errorf("failed to remove stale lock file: %w", removeErr)
					}
					// Retry lock acquisition after removing stale lock
					return acquireManagerLock(fsmDBPath)
				}
			}
			return fmt.Errorf("another flyio-image-manager process is running (lock file exists at %s). Wait for it to complete or remove the lock file manually", lockPath)
		}
		return fmt.Errorf("failed to create lock file: %w", err)
	}

	// Write lock info to file
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(lockPath) // Clean up on failure
		return fmt.Errorf("failed to write lock file: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(lockPath) // Clean up on failure
		return fmt.Errorf("failed to close lock file: %w", err)
	}

	log.WithFields(logrus.Fields{
		"lock_path": lockPath,
		"pid":       info.PID,
		"command":   info.Command,
	}).Info("acquired manager lock (atomic)")

	return nil
}

// isProcessRunning checks if a process with the given PID is still running.
// Used to detect stale lock files from crashed processes.
func isProcessRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds, so we need to send signal 0 to check
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

// preFlightStabilize ensures any previous devicemapper operations are fully settled
// before allowing a new operation to proceed. This is called BEFORE health checks
// to give the kernel time to fully process previous operations.
//
// PERFORMANCE OPTIMIZATION: With ext4 journaling disabled, we can reduce these delays.
func preFlightStabilize(ctx context.Context, poolName string) {
	deviceMgr := devicemapper.New()

	// PERFORMANCE OPTIMIZED: Minimal pre-flight stabilization
	// Just sync pool metadata - udev settle and sleep are unnecessary overhead
	_ = deviceMgr.SyncPoolMetadata(ctx, poolName)
}

// checkSystemHealth performs pre-flight checks before devicemapper operations.
// This prevents operations when the system is in a state that could cause kernel panics.
//
// Checks performed:
// 1. D-state processes: Indicates kernel-level I/O issues (dm-thin stuck)
// 2. High load average: System under stress, operations may timeout/hang
// 3. Kernel dm-thin errors: Check dmesg for recent devicemapper errors
// 4. Pool health: Check if thin-pool needs_check flag is set
// 5. Memory pressure: Check for OOM conditions
//
// This is CRITICAL for kernel panic prevention - the D-state buildup we observed
// before panics can be detected early and operations refused.
func checkSystemHealth() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check 1: D-state processes (uninterruptible sleep)
	// These indicate kernel-level issues, often with devicemapper
	dStateCount, err := countDmRelatedDState(ctx)
	if err != nil {
		log.WithError(err).Warn("failed to check D-state processes, continuing anyway")
	} else if dStateCount > 0 {
		return fmt.Errorf("system unstable: %d devicemapper-related D-state processes detected. "+
			"This indicates kernel-level I/O issues. Reboot recommended before proceeding", dStateCount)
	}

	// Check 2: Load average - high load can cause timeouts and cascading failures
	loadAvg, err := getLoadAverage()
	if err != nil {
		log.WithError(err).Warn("failed to check load average, continuing anyway")
	} else if loadAvg > 4.0 {
		log.WithField("load_avg", loadAvg).Warn("high system load detected, operations may be slow")
		// Don't fail, just warn - high load alone isn't dangerous
	}

	// Check 3: Recent kernel dm-thin errors in dmesg
	// Only flag truly critical errors, not old messages
	dmErrors, err := checkDmesgForDmErrors(ctx)
	if err != nil {
		log.WithError(err).Warn("failed to check dmesg for dm errors, continuing anyway")
	} else if dmErrors > 2 {
		// Only block if there are multiple critical errors (indicates active issue)
		return fmt.Errorf("system unstable: %d critical devicemapper errors in recent kernel log. "+
			"This indicates active dm-thin issues. Wait 30 seconds or reboot before proceeding", dmErrors)
	} else if dmErrors > 0 {
		// Warn but don't block for 1-2 errors (could be transient)
		log.WithField("dm_errors", dmErrors).Warn("detected devicemapper messages in dmesg, proceeding with caution")
	}

	// Check 4: Memory pressure (OOM conditions can cause dm hangs)
	memPressure, err := checkMemoryPressure()
	if err != nil {
		log.WithError(err).Warn("failed to check memory pressure, continuing anyway")
	} else if memPressure {
		return fmt.Errorf("system unstable: high memory pressure detected. " +
			"This can cause devicemapper operations to hang. Free memory or reboot")
	}

	// Check 5: Any I/O wait percentage > 50% indicates storage bottleneck
	ioWait, err := getIOWait(ctx)
	if err != nil {
		log.WithError(err).Warn("failed to check I/O wait, continuing anyway")
	} else if ioWait > 50.0 {
		return fmt.Errorf("system unstable: I/O wait at %.1f%% indicates storage bottleneck. "+
			"Wait for I/O to settle or reboot", ioWait)
	}

	log.Debug("system health check passed")
	return nil
}

// countDmRelatedDState counts D-state processes related to devicemapper.
// These are the dangerous ones that indicate dm-thin stack issues.
func countDmRelatedDState(ctx context.Context) (int, error) {
	// More robust D-state detection - look for ANY D-state process that mentions dm, thin, or jbd2
	// in the entire line (not just specific columns). This catches kernel threads that appear
	// in brackets like [kworker/u128:0+dm-thin] or [jbd2/dm-1-8]
	cmd := exec.CommandContext(ctx, "sh", "-c",
		`ps aux 2>/dev/null | awk '$8 ~ /^D/ && ($0 ~ /dm/ || $0 ~ /thin/ || $0 ~ /jbd2/ || $0 ~ /kworker.*\+/) {count++} END {print count+0}'`)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("D-state check timed out (system may be hung): %w", ctx.Err())
		}
		return 0, fmt.Errorf("failed to check D-state processes: %w", err)
	}

	countStr := strings.TrimSpace(string(output))
	var count int
	if countStr != "" && countStr != "0" {
		fmt.Sscanf(countStr, "%d", &count)
	}
	return count, nil
}

// checkDmesgForDmErrors checks kernel log for recent devicemapper errors.
// Returns count of CRITICAL dm-related errors that indicate active issues.
// Only looks at very recent messages (last 30 lines) to avoid false positives from old errors.
func checkDmesgForDmErrors(ctx context.Context) (int, error) {
	// Only check the last 30 lines of dmesg (very recent messages)
	// and only look for critical errors that indicate active dm-thin issues:
	// - "needs_check" - pool corruption
	// - "I/O error" combined with dm - active I/O failures
	// - "metadata operation failed" - metadata corruption
	// Exclude general "device-mapper: thin" messages as those can be informational
	cmd := exec.CommandContext(ctx, "sh", "-c",
		`dmesg 2>/dev/null | tail -30 | grep -ciE '(needs_check|I/O error.*dm|dm.*I/O error|metadata operation failed)' || echo 0`)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("dmesg check timed out: %w", ctx.Err())
		}
		// dmesg might fail if not root, that's ok
		return 0, nil
	}

	countStr := strings.TrimSpace(string(output))
	var count int
	fmt.Sscanf(countStr, "%d", &count)
	return count, nil
}

// checkMemoryPressure checks if system is under memory pressure.
// Returns true if available memory is below 5% or swap is heavily used.
func checkMemoryPressure() (bool, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return false, err
	}

	lines := strings.Split(string(data), "\n")
	var memTotal, memAvailable, swapTotal, swapFree int64

	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var value int64
		fmt.Sscanf(fields[1], "%d", &value)

		switch fields[0] {
		case "MemTotal:":
			memTotal = value
		case "MemAvailable:":
			memAvailable = value
		case "SwapTotal:":
			swapTotal = value
		case "SwapFree:":
			swapFree = value
		}
	}

	// Check if available memory is below 5%
	if memTotal > 0 && memAvailable > 0 {
		availPercent := float64(memAvailable) / float64(memTotal) * 100
		if availPercent < 5.0 {
			log.WithFields(logrus.Fields{
				"mem_available_pct": availPercent,
				"mem_available_kb":  memAvailable,
				"mem_total_kb":      memTotal,
			}).Warn("low memory condition detected")
			return true, nil
		}
	}

	// Check if swap is more than 80% used (indicates memory pressure)
	if swapTotal > 0 {
		swapUsedPercent := float64(swapTotal-swapFree) / float64(swapTotal) * 100
		if swapUsedPercent > 80.0 {
			log.WithFields(logrus.Fields{
				"swap_used_pct": swapUsedPercent,
				"swap_free_kb":  swapFree,
				"swap_total_kb": swapTotal,
			}).Warn("high swap usage detected")
			return true, nil
		}
	}

	return false, nil
}

// getIOWait returns the current I/O wait percentage from /proc/stat.
func getIOWait(ctx context.Context) (float64, error) {
	// Use vmstat to get current I/O wait percentage
	cmd := exec.CommandContext(ctx, "sh", "-c",
		`vmstat 1 2 2>/dev/null | tail -1 | awk '{print $16}'`)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("vmstat timed out: %w", ctx.Err())
		}
		// vmstat might not be available, that's ok
		return 0, nil
	}

	ioWaitStr := strings.TrimSpace(string(output))
	var ioWait float64
	fmt.Sscanf(ioWaitStr, "%f", &ioWait)
	return ioWait, nil
}

// getLoadAverage returns the 1-minute load average.
func getLoadAverage() (float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	var load1 float64
	fmt.Sscanf(string(data), "%f", &load1)
	return load1, nil
}

// initializeSafeguards sets up the operation guard and pool manager.
// This should be called early in the application startup.
func initializeSafeguards(cfg Config) error {
	// Initialize pool manager
	poolConfig := devicemapper.DefaultPoolConfig(filepath.Dir(cfg.DBPath))
	poolConfig.PoolName = cfg.PoolName
	poolManager = devicemapper.NewPoolManager(poolConfig, log)

	// Initialize health checker
	healthChecker := safeguards.NewSystemHealthChecker(cfg.PoolName, log)

	// Initialize operation guard with health check integration
	operationGuard = safeguards.NewOperationGuard(safeguards.GuardConfig{
		MaxConcurrent:   1, // Serialize all dm operations
		Logger:          log,
		HealthCheckFunc: healthChecker.CheckAll,
	})

	log.Info("safeguards initialized")
	return nil
}

// ensurePoolReady checks if the pool exists and creates it if needed.
// This is the main entry point for startup pool validation.
func ensurePoolReady(ctx context.Context, cfg Config) error {
	if poolManager == nil {
		if err := initializeSafeguards(cfg); err != nil {
			return fmt.Errorf("failed to initialize safeguards: %w", err)
		}
	}

	// Try to ensure pool exists (will auto-create if missing)
	if err := poolManager.EnsurePoolExists(ctx); err != nil {
		return fmt.Errorf("pool not ready: %w", err)
	}

	return nil
}

// checkPoolExists verifies that the devicemapper thin-pool exists.
// This is critical after a kernel panic or reboot - the pool may need to be recreated.
func checkPoolExists(ctx context.Context, poolName string) error {
	// If pool manager is initialized, use it for validation
	if poolManager != nil {
		return poolManager.ValidatePoolHealth(ctx)
	}

	// Fallback to direct check
	cmd := exec.CommandContext(ctx, "dmsetup", "status", poolName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "Device does not exist") {
			return fmt.Errorf("thin-pool %q does not exist. "+
				"This typically happens after a kernel panic or reboot. "+
				"Please recreate the pool using the setup commands in the README", poolName)
		}
		return fmt.Errorf("failed to check pool status: %w (output: %s)", err, string(output))
	}

	// Check for needs_check flag in the output
	if strings.Contains(string(output), "needs_check") {
		return fmt.Errorf("thin-pool %q has needs_check flag set - pool may be corrupted. "+
			"Run: sudo dmsetup message %s 0 reserve_metadata_snap && "+
			"sudo thin_check /dev/mapper/%s_tmeta", poolName, poolName, poolName)
	}

	return nil
}

// releaseManagerLock removes the manager lock file.
// This is idempotent - it does not error if the lock file doesn't exist.
// Should be called via defer after successful lock acquisition.
func releaseManagerLock(fsmDBPath string) error {
	lockPath := filepath.Join(fsmDBPath, "flyio-manager.lock")

	if err := os.Remove(lockPath); err != nil {
		if os.IsNotExist(err) {
			// Lock file already removed - this is fine (idempotent)
			return nil
		}
		log.WithError(err).WithField("lock_path", lockPath).Error("failed to release manager lock")
		return fmt.Errorf("failed to remove lock file: %w", err)
	}

	log.WithField("lock_path", lockPath).Info("released manager lock")
	return nil
}

// runProcessImage processes a single image through the complete pipeline.
func runProcessImage(cfg Config) error {
	if err := setupLogger(cfg.LogLevel); err != nil {
		return err
	}

	startTime := time.Now()

	// Initialize progress tracking
	tracker := tui.NewProgressTracker()

	// Use Bubble Tea TUI for interactive progress display, or CLIProgress for quiet mode
	if cfg.Quiet {
		// Quiet mode: use simple CLI progress
		cliProgress := tui.NewCLIProgress(cfg.Quiet, false)
		cliProgress.PrintHeader(cfg.ImageID, cfg.S3Key)
		tracker.Subscribe(cliProgress.CreateProgressCallback())

		result, err := runFSMPipeline(cfg, tracker, false) // CLI mode: don't suppress logs
		if err != nil {
			tracker.ReportError(err)
			cliProgress.PrintSummary(&tui.ProcessResult{Error: err, TotalTime: time.Since(startTime)})
			return err
		}

		cliProgress.PrintSummary(&tui.ProcessResult{
			ImageID:      result.ImageID,
			SnapshotID:   result.SnapshotID,
			SnapshotName: result.SnapshotName,
			DevicePath:   result.DevicePath,
			TotalTime:    time.Since(startTime),
		})
		return nil
	}

	// Interactive mode: use Bubble Tea TUI
	// Suppress all log output to avoid mixing with TUI output
	log.SetOutput(io.Discard)    // Suppress logrus
	stdlog.SetOutput(io.Discard) // Suppress standard library log (used by database)

	model := tui.NewProgressModel(cfg.ImageID, cfg.S3Key, false)
	program := tea.NewProgram(model)

	// Subscribe the TUI to progress events
	tracker.Subscribe(tui.CreateTeaCallback(program))

	// Run FSM pipeline in a goroutine
	go func() {
		result, err := runFSMPipeline(cfg, tracker, true) // TUI mode: suppress logs
		if err != nil {
			tui.SendAllComplete(program, "", "", "", "", time.Since(startTime), err)
			return
		}
		tui.SendAllComplete(program, result.ImageID, result.SnapshotID, result.SnapshotName, result.DevicePath, time.Since(startTime), nil)
	}()

	// Run the TUI (blocks until AllCompleteMsg is received)
	finalModel, err := program.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	// Check if the model has an error
	if pm, ok := finalModel.(*tui.ProgressModel); ok {
		if pm.Error() != nil {
			return pm.Error()
		}
	}

	return nil
}

// pipelineResult holds the result of the FSM pipeline
type pipelineResult struct {
	ImageID      string
	SnapshotID   string
	SnapshotName string
	DevicePath   string
}

// runFSMPipeline runs the Download → Unpack → Activate FSM pipeline.
// This is extracted from runProcessImage to allow both CLI and TUI modes to share the same logic.
// If suppressLogs is true, S3 client logging is disabled (for TUI mode).
func runFSMPipeline(cfg Config, tracker *tui.ProgressTracker, suppressLogs bool) (*pipelineResult, error) {
	ctx := context.Background()

	// Initialize safeguards if not already done
	if operationGuard == nil {
		if err := initializeSafeguards(cfg); err != nil {
			return nil, fmt.Errorf("failed to initialize safeguards: %w", err)
		}
	}

	// CRITICAL: Pre-flight stabilization before any health checks.
	// This ensures any previous operation's effects are fully settled.
	// Without this, health checks may pass but subsequent operations cause kernel panics.
	preFlightStabilize(ctx, cfg.PoolName)

	// CRITICAL: Pre-flight system health check before devicemapper operations
	// D-state processes indicate kernel-level issues that can cause panics
	if err := checkSystemHealth(); err != nil {
		return nil, fmt.Errorf("system health check failed: %w", err)
	}

	// CRITICAL: Ensure pool exists and is healthy (auto-create if missing after reboot)
	if err := ensurePoolReady(ctx, cfg); err != nil {
		return nil, fmt.Errorf("pool not ready: %w", err)
	}

	log.WithFields(logrus.Fields{
		"s3_key":   cfg.S3Key,
		"image_id": cfg.ImageID,
		"bucket":   cfg.S3Bucket,
	}).Info("processing image")

	// Acquire manager lock to prevent concurrent processes
	// This prevents multiple flyio-image-manager processes from running devicemapper
	// operations concurrently, which can cause kernel panics.
	if err := acquireManagerLock(cfg.FSMDBPath); err != nil {
		return nil, err
	}
	defer releaseManagerLock(cfg.FSMDBPath)

	// Initialize dependencies
	deps, err := initializeDependencies(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize dependencies: %w", err)
	}
	defer deps.Close()

	// Suppress all client logs in TUI mode to avoid mixing with display
	if suppressLogs {
		deps.S3Client.SuppressLogs()
		deps.DeviceMgr.SuppressLogs()
		deps.Extractor.SuppressLogs()
	}

	// Wire up progress callbacks for S3 download
	deps.S3Client.SetProgressFunc(func(downloaded, total int64, speed float64) {
		tracker.UpdateWithTotal(downloaded, total)
	})

	// Wire up progress callbacks for tar extraction
	// Note: We use bytesExtracted for more accurate progress since file count isn't known ahead
	deps.Extractor.SetProgressFunc(func(filesExtracted int, bytesExtracted int64, currentFile string) {
		// For extraction, we don't have a good total, so just update with file count
		// The TUI will show indeterminate progress for this phase
		tracker.Update(int64(filesExtracted))
	})

	// Initialize FSM manager with serial queues for ALL phases.
	// CRITICAL: All devicemapper operations must be serialized to prevent kernel panics.
	// The dm-thin pool cannot handle concurrent operations safely.
	manager, err := fsm.New(fsm.Config{
		Logger: log,
		DBPath: cfg.FSMDBPath,
		Queues: map[string]int{
			"download": cfg.DownloadQueueSize,
			"unpack":   cfg.UnpackQueueSize,
			"activate": 1, // MUST be 1 to serialize snapshot creation
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create FSM manager: %w", err)
	}
	defer manager.Shutdown(5 * time.Second)

	// Register FSMs
	downloadStart, downloadResume, err := registerDownloadFSM(ctx, manager, deps, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to register download FSM: %w", err)
	}

	unpackStart, unpackResume, err := registerUnpackFSM(ctx, manager, deps, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to register unpack FSM: %w", err)
	}

	activateStart, activateResume, err := registerActivateFSM(ctx, manager, deps, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to register activate FSM: %w", err)
	}

	// Resume any in-flight FSMs (in case of crash recovery)
	log.Info("resuming in-flight FSM runs")
	if err := downloadResume(ctx); err != nil {
		log.WithError(err).Warn("failed to resume download FSM runs")
	}
	if err := unpackResume(ctx); err != nil {
		log.WithError(err).Warn("failed to resume unpack FSM runs")
	}
	if err := activateResume(ctx); err != nil {
		log.WithError(err).Warn("failed to resume activate FSM runs")
	}

	// ========== DOWNLOAD PHASE ==========
	downloadReq := &fsm.ImageDownloadRequest{
		S3Key:   cfg.S3Key,
		ImageID: cfg.ImageID,
		Bucket:  cfg.S3Bucket,
		Region:  cfg.S3Region,
	}

	var downloadResp fsm.ImageDownloadResponse
	log.Info("starting download FSM")

	// Start download phase tracking
	tracker.StartPhase(tui.PhaseDownload, 0)

	request := fsm.NewRequest(downloadReq, &downloadResp)
	version, err := downloadStart(ctx, cfg.ImageID, request, fsm.WithQueue("download"))
	if err != nil {
		tracker.ReportError(err)
		return nil, fmt.Errorf("download FSM failed: %w", err)
	}

	if err := manager.Wait(ctx, version); err != nil {
		// HandoffError is not a failure - it means the FSM detected work was already done
		// Check both by type and by error message (backoff wrapping may hide the type)
		var handoffErr *fsm.HandoffError
		isHandoff := errors.As(err, &handoffErr) || strings.Contains(err.Error(), "FSM handoff to")
		if !isHandoff {
			tracker.ReportError(err)
			return nil, fmt.Errorf("failed waiting for download FSM: %w", err)
		}
		log.Info("download FSM handed off (image already downloaded)")
	}

	// Complete download phase
	tracker.CompletePhase()

	// Query database for download results (FSM doesn't populate response variable)
	downloadedImage, err := deps.DB.GetImageByID(ctx, cfg.ImageID)
	if err != nil {
		tracker.ReportError(err)
		return nil, fmt.Errorf("failed to get downloaded image metadata: %w", err)
	}
	if downloadedImage == nil {
		err := fmt.Errorf("image not found in database after download")
		tracker.ReportError(err)
		return nil, err
	}

	log.WithFields(logrus.Fields{
		"image_id":   downloadedImage.ImageID,
		"local_path": downloadedImage.LocalPath,
		"checksum":   downloadedImage.Checksum,
		"size_bytes": downloadedImage.SizeBytes,
	}).Info("download FSM completed")

	// ========== UNPACK PHASE ==========
	unpackReq := &fsm.ImageUnpackRequest{
		ImageID:   downloadedImage.ImageID,
		LocalPath: downloadedImage.LocalPath,
		Checksum:  downloadedImage.Checksum,
		PoolName:  cfg.PoolName,
	}

	var unpackResp fsm.ImageUnpackResponse
	log.Info("starting unpack FSM")

	// Start unpack phase tracking
	tracker.StartPhase(tui.PhaseUnpack, 0)

	unpackRequest := fsm.NewRequest(unpackReq, &unpackResp)
	unpackVersion, err := unpackStart(ctx, cfg.ImageID, unpackRequest, fsm.WithQueue("unpack"))
	if err != nil {
		tracker.ReportError(err)
		return nil, fmt.Errorf("unpack FSM failed: %w", err)
	}

	if err := manager.Wait(ctx, unpackVersion); err != nil {
		// HandoffError is not a failure - it means the FSM detected work was already done
		// Check both by type and by error message (backoff wrapping may hide the type)
		var handoffErr *fsm.HandoffError
		isHandoff := errors.As(err, &handoffErr) || strings.Contains(err.Error(), "FSM handoff to")
		if !isHandoff {
			tracker.ReportError(err)
			return nil, fmt.Errorf("failed waiting for unpack FSM: %w", err)
		}
		log.Info("unpack FSM handed off (image already unpacked)")
	}

	// Complete unpack phase
	tracker.CompletePhase()

	// Query database for unpack results
	unpackedImage, err := deps.DB.CheckImageUnpacked(ctx, cfg.ImageID)
	if err != nil {
		tracker.ReportError(err)
		return nil, fmt.Errorf("failed to get unpacked image metadata: %w", err)
	}
	if unpackedImage == nil {
		err := fmt.Errorf("image not found in unpacked_images table after unpack")
		tracker.ReportError(err)
		return nil, err
	}

	log.WithFields(logrus.Fields{
		"image_id":    unpackedImage.ImageID,
		"device_id":   unpackedImage.DeviceID,
		"device_name": unpackedImage.DeviceName,
		"device_path": unpackedImage.DevicePath,
		"size_bytes":  unpackedImage.SizeBytes,
		"file_count":  unpackedImage.FileCount,
	}).Info("unpack FSM completed")

	// ========== ACTIVATE PHASE ==========
	activateReq := &fsm.ImageActivateRequest{
		ImageID:    unpackedImage.ImageID,
		DeviceID:   unpackedImage.DeviceID,
		DeviceName: unpackedImage.DeviceName,
		PoolName:   cfg.PoolName,
	}

	var activateResp fsm.ImageActivateResponse
	log.Info("starting activate FSM")

	// Start activate phase tracking
	tracker.StartPhase(tui.PhaseActivate, 0)

	activateRequest := fsm.NewRequest(activateReq, &activateResp)
	activateVersion, err := activateStart(ctx, cfg.ImageID, activateRequest, fsm.WithQueue("activate"))
	if err != nil {
		tracker.ReportError(err)
		return nil, fmt.Errorf("activate FSM failed: %w", err)
	}

	if err := manager.Wait(ctx, activateVersion); err != nil {
		// HandoffError is not a failure - it means the FSM detected work was already done
		// Check both by type and by error message (backoff wrapping may hide the type)
		var handoffErr *fsm.HandoffError
		isHandoff := errors.As(err, &handoffErr) || strings.Contains(err.Error(), "FSM handoff to")
		if !isHandoff {
			tracker.ReportError(err)
			return nil, fmt.Errorf("failed waiting for activate FSM: %w", err)
		}
		log.Info("activate FSM handed off (snapshot already exists)")
	}

	// Complete activate phase
	tracker.CompletePhase()

	// Query database for activate results (FSM doesn't populate response variable)
	snapshots, err := deps.DB.GetSnapshotsByImageID(ctx, cfg.ImageID)
	if err != nil {
		tracker.ReportError(err)
		return nil, fmt.Errorf("failed to get snapshot metadata: %w", err)
	}
	if len(snapshots) == 0 {
		err := fmt.Errorf("snapshot not found in database after activation")
		tracker.ReportError(err)
		return nil, err
	}
	snapshot := snapshots[0] // Get the most recent snapshot

	log.WithFields(logrus.Fields{
		"image_id":      snapshot.ImageID,
		"snapshot_id":   snapshot.SnapshotID,
		"snapshot_name": snapshot.SnapshotName,
		"device_path":   snapshot.DevicePath,
		"active":        snapshot.Active,
	}).Info("activate FSM completed")

	return &pipelineResult{
		ImageID:      snapshot.ImageID,
		SnapshotID:   snapshot.SnapshotID,
		SnapshotName: snapshot.SnapshotName,
		DevicePath:   snapshot.DevicePath,
	}, nil
}

// runListImages lists downloaded images.
func runListImages(cfg Config) error {
	if err := setupLogger(cfg.LogLevel); err != nil {
		return err
	}

	ctx := context.Background()

	db, err := database.New(database.Config{Path: cfg.DBPath})
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	images, err := db.ListImages(ctx, "")
	if err != nil {
		return fmt.Errorf("failed to list images: %w", err)
	}

	fmt.Printf("Found %d images:\n\n", len(images))
	for _, img := range images {
		fmt.Printf("Image ID:         %s\n", img.ImageID)
		fmt.Printf("  S3 Key:         %s\n", img.S3Key)
		fmt.Printf("  Local Path:     %s\n", img.LocalPath)
		fmt.Printf("  Size:           %d bytes\n", img.SizeBytes)
		fmt.Printf("  Status:         %s\n", img.DownloadStatus)
		fmt.Printf("  Activation:     %s\n", img.ActivationStatus)
		if img.DownloadedAt != nil {
			fmt.Printf("  Downloaded At:  %s\n", img.DownloadedAt.Format(time.RFC3339))
		} else {
			fmt.Printf("  Downloaded At:  (not completed)\n")
		}
		fmt.Println()
	}

	return nil
}

// runListSnapshots lists active snapshots.
func runListSnapshots(cfg Config) error {
	if err := setupLogger(cfg.LogLevel); err != nil {
		return err
	}

	ctx := context.Background()

	db, err := database.New(database.Config{Path: cfg.DBPath})
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	snapshots, err := db.ListActiveSnapshots(ctx)
	if err != nil {
		return fmt.Errorf("failed to list snapshots: %w", err)
	}

	fmt.Printf("Found %d active snapshots:\n\n", len(snapshots))
	for _, snap := range snapshots {
		fmt.Printf("Snapshot ID:      %s\n", snap.SnapshotID)
		fmt.Printf("  Image ID:       %s\n", snap.ImageID)
		fmt.Printf("  Snapshot Name:  %s\n", snap.SnapshotName)
		fmt.Printf("  Device Path:    %s\n", snap.DevicePath)
		fmt.Printf("  Active:         %v\n", snap.Active)
		fmt.Printf("  Created At:     %s\n", snap.CreatedAt.Format(time.RFC3339))
		fmt.Println()
	}

	return nil
}

// runDaemon runs the application as a daemon with API server (future work).
func runDaemon(cfg Config) error {
	if err := setupLogger(cfg.LogLevel); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log.Info("starting daemon")

	// Acquire manager lock to prevent concurrent processes
	// This prevents multiple flyio-image-manager processes from running devicemapper
	// operations concurrently, which can cause kernel panics.
	if err := acquireManagerLock(cfg.FSMDBPath); err != nil {
		return err
	}
	defer releaseManagerLock(cfg.FSMDBPath)

	// Initialize dependencies
	deps, err := initializeDependencies(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize dependencies: %w", err)
	}
	defer deps.Close()

	// Initialize FSM manager with serial queues for ALL phases.
	// CRITICAL: All devicemapper operations must be serialized to prevent kernel panics.
	manager, err := fsm.New(fsm.Config{
		Logger: log,
		DBPath: cfg.FSMDBPath,
		Queues: map[string]int{
			"download": cfg.DownloadQueueSize,
			"unpack":   cfg.UnpackQueueSize,
			"activate": 1, // MUST be 1 to serialize snapshot creation
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create FSM manager: %w", err)
	}
	defer manager.Shutdown(5 * time.Second)

	// Register FSMs
	_, downloadResume, err := registerDownloadFSM(ctx, manager, deps, cfg)
	if err != nil {
		return fmt.Errorf("failed to register download FSM: %w", err)
	}

	_, unpackResume, err := registerUnpackFSM(ctx, manager, deps, cfg)
	if err != nil {
		return fmt.Errorf("failed to register unpack FSM: %w", err)
	}

	_, activateResume, err := registerActivateFSM(ctx, manager, deps, cfg)
	if err != nil {
		return fmt.Errorf("failed to register activate FSM: %w", err)
	}

	// Resume any in-flight FSMs
	log.Info("resuming in-flight FSM runs")
	if err := downloadResume(ctx); err != nil {
		log.WithError(err).Warn("failed to resume download FSM runs")
	}
	if err := unpackResume(ctx); err != nil {
		log.WithError(err).Warn("failed to resume unpack FSM runs")
	}
	if err := activateResume(ctx); err != nil {
		log.WithError(err).Warn("failed to resume activate FSM runs")
	}

	log.Info("daemon started successfully")

	// Setup signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	log.WithField("signal", sig).Info("received shutdown signal")

	// Graceful shutdown
	log.Info("shutting down gracefully...")
	cancel()

	// Give FSMs time to persist state
	time.Sleep(2 * time.Second)

	log.Info("shutdown complete")
	return nil
}

// runMonitor runs the interactive TUI dashboard for live FSM tracking.
func runMonitor(cfg Config) error {
	// Suppress log output to avoid mixing with TUI
	log.SetOutput(io.Discard)
	stdlog.SetOutput(io.Discard)

	// Open database for reading statistics
	// Track the error for diagnostics display in the TUI
	var dbErr error
	db, dbErr := database.New(database.Config{Path: cfg.DBPath})
	if dbErr != nil {
		// Database might not exist yet - that's OK for monitoring
		// but we'll show the error in the TUI for debugging
		db = nil
	}
	if db != nil {
		defer db.Close()
	}

	// Create FSM admin client (may fail if no daemon running - that's OK)
	adminClient, err := tui.NewAdminClient(cfg.FSMDBPath)
	if err != nil {
		adminClient = nil
	}

	// Create S3 client for browsing images
	s3Client, err := s3.New(context.Background(), s3.Config{
		Region: cfg.S3Region,
	})
	if err != nil {
		// S3 client creation failed - continue without it
		s3Client = nil
	}

	// Create data fetcher with path info for diagnostics
	fetcher := tui.NewDataFetcherWithPath(adminClient, db, cfg.DBPath, cfg.PoolName, dbErr)

	// Set S3 client if available
	if s3Client != nil {
		fetcher.SetS3Client(s3Client)
	}

	// Set image processing function with progress callback
	fetcher.SetImageProcessFuncWithProgress(func(ctx context.Context, s3Key string, progressCh chan<- tui.ProgressEvent) error {
		return runImageProcessFromTUIWithProgress(cfg, s3Key, progressCh)
	})

	// Create dashboard model with configuration
	dashboardCfg := tui.DashboardConfig{
		Title:           "Fly.io Image Manager Dashboard",
		RefreshInterval: time.Second,
		Fetcher:         fetcher,
	}
	model := tui.NewDashboardModelWithConfig(dashboardCfg)

	// Run the TUI - use alt-screen unless --inline flag is set
	var p *tea.Program
	if cfg.Inline {
		p = tea.NewProgram(model)
	} else {
		p = tea.NewProgram(model, tea.WithAltScreen())
	}

	if _, err := p.Run(); err != nil {
		return fmt.Errorf("failed to run dashboard: %w", err)
	}

	return nil
}

// runImageProcessFromTUI runs the image processing pipeline from the TUI.
// This is a simplified version that runs synchronously.
func runImageProcessFromTUI(cfg Config, s3Key string) error {
	// Create a simple progress tracker that does nothing (TUI handles display)
	tracker := &tui.ProgressTracker{}

	// Run the FSM pipeline
	pipelineCfg := cfg
	pipelineCfg.S3Key = s3Key
	// CRITICAL: Derive ImageID from S3 key - this was missing and caused unpack failures
	// when triggered from the TUI dashboard. The ImageID must be derived deterministically
	// from the S3 key for idempotency to work correctly.
	pipelineCfg.ImageID = fsm.DeriveImageIDFromS3Key(s3Key)
	result, err := runFSMPipeline(pipelineCfg, tracker, true)

	// CRITICAL: ALWAYS perform stabilization after ANY devicemapper operation,
	// even on failure. This prevents kernel panics when processing sequential images.
	stabilizeAfterOperation(cfg.PoolName, result != nil)

	return err
}

// stabilizeAfterOperation performs stabilization to prevent kernel panics.
// This MUST be called after ANY devicemapper operation (success OR failure).
//
// PERFORMANCE OPTIMIZED: With ext4 journaling disabled and FSM stabilization
// already handling the critical paths, this function is now minimal.
// The heavy D-state checking is only done on failure to avoid overhead.
func stabilizeAfterOperation(poolName string, wasSuccessful bool) {
	ctx := context.Background()
	deviceMgr := devicemapper.New()

	// Sync pool metadata to force commit
	_ = deviceMgr.SyncPoolMetadata(ctx, poolName)

	// Quick udev settle - just process pending events
	exec.Command("udevadm", "settle", "--timeout=0").Run()

	// Only check for D-state on failure (expensive operation)
	if !wasSuccessful {
		dStateCount, _ := countDmRelatedDState(ctx)
		if dStateCount > 0 {
			logrus.Warnf("Detected %d D-state processes after failed operation", dStateCount)
		}
	}
}

// runImageProcessFromTUIWithProgress runs the image processing pipeline from the TUI with progress updates.
// Progress events are sent to the provided channel for real-time display in the dashboard.
func runImageProcessFromTUIWithProgress(cfg Config, s3Key string, progressCh chan<- tui.ProgressEvent) error {
	// Create a progress tracker that sends events to the channel
	tracker := tui.NewProgressTracker()

	tui.DebugLog("runImageProcessFromTUIWithProgress: created tracker for %s", s3Key)

	// Send an immediate "starting" event so TUI shows progress right away
	select {
	case progressCh <- tui.ProgressEvent{
		Type:    tui.EventDownloadStart,
		Phase:   tui.PhaseDownload,
		Message: "Initializing...",
		Percent: 0,
	}:
	default:
	}

	// Subscribe to progress events and forward them to the channel
	tracker.Subscribe(func(event tui.ProgressEvent) {
		tui.DebugLog("callback: forwarding event type=%s phase=%s to channel", event.Type, event.Phase)

		// Non-blocking send to avoid deadlock if channel is full
		select {
		case progressCh <- event:
			tui.DebugLog("callback: event sent successfully")
		default:
			// Channel full, skip this update
			tui.DebugLog("callback: channel full, event dropped")
		}
	})

	// Run the FSM pipeline
	pipelineCfg := cfg
	pipelineCfg.S3Key = s3Key
	// CRITICAL: Derive ImageID from S3 key for idempotency
	pipelineCfg.ImageID = fsm.DeriveImageIDFromS3Key(s3Key)
	result, err := runFSMPipeline(pipelineCfg, tracker, true)

	// Send completion/error event
	if err != nil {
		select {
		case progressCh <- tui.ProgressEvent{
			Type:    tui.EventError,
			Phase:   tui.PhaseDownload,
			Message: err.Error(),
			Percent: 0,
		}:
		default:
		}
	}

	// CRITICAL: ALWAYS perform stabilization after ANY devicemapper operation,
	// even on failure. This prevents kernel panics when processing sequential images.
	stabilizeAfterOperation(cfg.PoolName, result != nil && err == nil)

	return err
}

// Dependencies holds all external dependencies.
type Dependencies struct {
	DB        *database.DB
	S3Client  *s3.Client
	DeviceMgr *devicemapper.Client
	Extractor *extraction.Extractor
}

// Close closes all dependencies.
func (d *Dependencies) Close() {
	if d.DB != nil {
		d.DB.Close()
	}
}

// initializeDependencies initializes all external dependencies.
func initializeDependencies(ctx context.Context, cfg Config) (*Dependencies, error) {
	// Create directories
	if err := os.MkdirAll(cfg.LocalDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create local directory: %w", err)
	}
	if err := os.MkdirAll(cfg.MountRoot, 0755); err != nil {
		return nil, fmt.Errorf("failed to create mount root: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}
	if err := os.MkdirAll(cfg.FSMDBPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create FSM database directory: %w", err)
	}

	// Initialize database
	db, err := database.New(database.Config{Path: cfg.DBPath})
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Initialize S3 client
	s3Client, err := s3.New(ctx, s3.Config{
		Region: cfg.S3Region,
		Bucket: cfg.S3Bucket,
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create S3 client: %w", err)
	}

	// Initialize DeviceMapper client
	deviceMgr := devicemapper.New()

	// Initialize Extractor
	extractor := extraction.New()

	return &Dependencies{
		DB:        db,
		S3Client:  s3Client,
		DeviceMgr: deviceMgr,
		Extractor: extractor,
	}, nil
}

// registerDownloadFSM registers the Download FSM with the manager.
func registerDownloadFSM(ctx context.Context, manager *fsm.Manager, deps *Dependencies, cfg Config) (fsm.Start[fsm.ImageDownloadRequest, fsm.ImageDownloadResponse], fsm.Resume, error) {
	downloadDeps := &download.Dependencies{
		DB:       deps.DB,
		S3Client: deps.S3Client,
		LocalDir: cfg.LocalDir,
	}

	start, resume, err := download.Register(ctx, manager, downloadDeps)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to register download FSM: %w", err)
	}

	// Configure queue and timeout via FSM options
	// Note: The FSM library typically handles these via WithQueue and WithTimeout
	// at the manager level or during FSM construction

	log.Info("download FSM registered")
	return start, resume, nil
}

// registerUnpackFSM registers the Unpack FSM with the manager.
func registerUnpackFSM(ctx context.Context, manager *fsm.Manager, deps *Dependencies, cfg Config) (fsm.Start[fsm.ImageUnpackRequest, fsm.ImageUnpackResponse], fsm.Resume, error) {
	unpackDeps := &unpack.Dependencies{
		DB:          deps.DB,
		DeviceMgr:   deps.DeviceMgr,
		Extractor:   deps.Extractor,
		PoolName:    cfg.PoolName,
		MountRoot:   cfg.MountRoot,
		DefaultSize: 4 * 1024 * 1024 * 1024, // 4GB - room for large image expansion (node.tar expands to ~1.5GB)
	}

	start, resume, err := unpack.Register(ctx, manager, unpackDeps)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to register unpack FSM: %w", err)
	}

	log.Info("unpack FSM registered")
	return start, resume, nil
}

// registerActivateFSM registers the Activate FSM with the manager.
func registerActivateFSM(ctx context.Context, manager *fsm.Manager, deps *Dependencies, cfg Config) (fsm.Start[fsm.ImageActivateRequest, fsm.ImageActivateResponse], fsm.Resume, error) {
	activateDeps := &activate.Dependencies{
		DB:        deps.DB,
		DeviceMgr: deps.DeviceMgr,
		PoolName:  cfg.PoolName,
	}

	start, resume, err := activate.Register(ctx, manager, activateDeps)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to register activate FSM: %w", err)
	}

	log.Info("activate FSM registered")
	return start, resume, nil
}
