// Package safeguards provides concurrency control and recovery mechanisms
// for FSM operations to prevent kernel panics and system instability.
package safeguards

import (
	"context"
	"fmt"
	"os/exec"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// OperationGuard provides serialized access to devicemapper operations.
// This prevents concurrent FSM operations from overwhelming the dm-thin pool.
type OperationGuard struct {
	mu              sync.Mutex
	semaphore       chan struct{}
	maxConcurrent   int
	activeOps       int
	logger          logrus.FieldLogger
	healthCheckFunc func(context.Context) error
}

// GuardConfig configures the operation guard.
type GuardConfig struct {
	// MaxConcurrent is the maximum number of concurrent dm operations (default: 1)
	MaxConcurrent int
	// Logger for logging operations
	Logger logrus.FieldLogger
	// HealthCheckFunc is called before each operation to verify system health
	HealthCheckFunc func(context.Context) error
}

// NewOperationGuard creates a new operation guard.
func NewOperationGuard(cfg GuardConfig) *OperationGuard {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1 // Default to serialized operations
	}
	if cfg.Logger == nil {
		cfg.Logger = logrus.StandardLogger()
	}
	return &OperationGuard{
		semaphore:       make(chan struct{}, cfg.MaxConcurrent),
		maxConcurrent:   cfg.MaxConcurrent,
		logger:          cfg.Logger.WithField("component", "operation-guard"),
		healthCheckFunc: cfg.HealthCheckFunc,
	}
}

// Acquire acquires a slot for a devicemapper operation.
// It performs health checks before allowing the operation to proceed.
func (g *OperationGuard) Acquire(ctx context.Context, opName string) error {
	g.logger.WithField("operation", opName).Debug("acquiring operation slot")

	// Try to acquire semaphore with context timeout
	select {
	case g.semaphore <- struct{}{}:
		// Got a slot
	case <-ctx.Done():
		return fmt.Errorf("context cancelled while waiting for operation slot: %w", ctx.Err())
	}

	g.mu.Lock()
	g.activeOps++
	activeOps := g.activeOps
	g.mu.Unlock()

	g.logger.WithFields(logrus.Fields{
		"operation":  opName,
		"active_ops": activeOps,
	}).Debug("acquired operation slot")

	// Perform health check before allowing operation
	if g.healthCheckFunc != nil {
		if err := g.healthCheckFunc(ctx); err != nil {
			g.Release(opName)
			return fmt.Errorf("health check failed before operation %s: %w", opName, err)
		}
	}

	return nil
}

// Release releases an operation slot.
func (g *OperationGuard) Release(opName string) {
	g.mu.Lock()
	g.activeOps--
	activeOps := g.activeOps
	g.mu.Unlock()

	<-g.semaphore

	g.logger.WithFields(logrus.Fields{
		"operation":  opName,
		"active_ops": activeOps,
	}).Debug("released operation slot")
}

// ActiveOperations returns the number of active operations.
func (g *OperationGuard) ActiveOperations() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.activeOps
}

// WithOperation executes a function with operation guard protection.
func (g *OperationGuard) WithOperation(ctx context.Context, opName string, fn func() error) error {
	if err := g.Acquire(ctx, opName); err != nil {
		return err
	}
	defer g.Release(opName)
	return fn()
}

// RecoverableOperation wraps a function with panic recovery.
func RecoverableOperation(logger logrus.FieldLogger, opName string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			logger.WithFields(logrus.Fields{
				"operation": opName,
				"panic":     r,
				"stack":     string(stack),
			}).Error("recovered from panic in operation")
			err = fmt.Errorf("panic in operation %s: %v", opName, r)
		}
	}()
	return fn()
}

// SystemHealthChecker provides comprehensive system health checks.
type SystemHealthChecker struct {
	logger   logrus.FieldLogger
	poolName string
}

// NewSystemHealthChecker creates a new health checker.
func NewSystemHealthChecker(poolName string, logger logrus.FieldLogger) *SystemHealthChecker {
	if logger == nil {
		logger = logrus.StandardLogger()
	}
	return &SystemHealthChecker{
		logger:   logger.WithField("component", "health-checker"),
		poolName: poolName,
	}
}

// CheckAll performs all health checks.
func (h *SystemHealthChecker) CheckAll(ctx context.Context) error {
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Check for D-state processes
	if err := h.checkDStateProcesses(checkCtx); err != nil {
		return err
	}

	// Check pool status
	if err := h.checkPoolStatus(checkCtx); err != nil {
		return err
	}

	// Check kernel logs for dm-thin errors
	if err := h.checkKernelLogs(checkCtx); err != nil {
		return err
	}

	// Check memory pressure
	if err := h.checkMemoryPressure(checkCtx); err != nil {
		return err
	}

	return nil
}

func (h *SystemHealthChecker) checkDStateProcesses(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "bash", "-c", "ps aux | awk '$8 ~ /^D/ {print $0}'")
	output, err := cmd.Output()
	if err != nil {
		return nil // Ignore errors in health check
	}

	outputStr := strings.TrimSpace(string(output))
	if outputStr != "" {
		lines := strings.Split(outputStr, "\n")
		// Check if any D-state processes are dm-related
		for _, line := range lines {
			if strings.Contains(line, "dm-") || strings.Contains(line, "thin") ||
				strings.Contains(line, "loop") || strings.Contains(line, "kworker") {
				h.logger.WithField("processes", outputStr).Warn("D-state processes detected")
				return fmt.Errorf("D-state processes detected - system may be unstable: %s", line)
			}
		}
	}
	return nil
}

func (h *SystemHealthChecker) checkPoolStatus(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "dmsetup", "status", h.poolName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "Device does not exist") {
			return fmt.Errorf("pool %q does not exist", h.poolName)
		}
		return fmt.Errorf("failed to check pool status: %w", err)
	}

	outputStr := string(output)
	if strings.Contains(outputStr, "needs_check") {
		return fmt.Errorf("pool has needs_check flag - corruption detected")
	}
	if strings.Contains(outputStr, "Error") || strings.Contains(outputStr, "error") {
		return fmt.Errorf("pool has error state")
	}

	return nil
}

func (h *SystemHealthChecker) checkKernelLogs(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "dmesg", "--time-format=reltime")
	output, err := cmd.Output()
	if err != nil {
		return nil // Ignore errors - dmesg may not be available
	}

	// Check last 50 lines for critical errors only
	lines := strings.Split(string(output), "\n")
	start := len(lines) - 50
	if start < 0 {
		start = 0
	}

	// Only check for critical errors that indicate imminent system failure
	criticalPatterns := []string{
		"BUG:",
		"kernel panic",
		"Out of memory",
		"oom-killer",
	}

	// Warning patterns - log but don't block
	warningPatterns := []string{
		"dm-thin",
		"device-mapper: thin",
	}

	for _, line := range lines[start:] {
		lineLower := strings.ToLower(line)

		// Check for critical errors - these always block
		for _, pattern := range criticalPatterns {
			if strings.Contains(lineLower, strings.ToLower(pattern)) {
				h.logger.WithField("log_line", line).Error("critical kernel error detected")
				return fmt.Errorf("critical kernel error detected: %s", line)
			}
		}

		// Check for warning patterns - only log, don't block
		// The pool health check will catch actual pool issues
		for _, pattern := range warningPatterns {
			if strings.Contains(lineLower, strings.ToLower(pattern)) {
				h.logger.WithField("log_line", line).Debug("dm-thin message in kernel log (informational)")
			}
		}
	}

	return nil
}

func (h *SystemHealthChecker) checkMemoryPressure(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "bash", "-c", "free -m | awk '/^Mem:/ {print $7}'")
	output, err := cmd.Output()
	if err != nil {
		return nil // Ignore errors
	}

	var availableMB int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(output)), "%d", &availableMB); err != nil {
		return nil
	}

	// Warn if less than 256MB available
	if availableMB < 256 {
		h.logger.WithField("available_mb", availableMB).Warn("low memory detected")
		return fmt.Errorf("low memory: only %dMB available", availableMB)
	}

	return nil
}
