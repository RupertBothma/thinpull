// Package perf provides performance measurement utilities for the FSM pipeline.
package perf

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Timer tracks operation timing for performance analysis.
type Timer struct {
	name      string
	startTime time.Time
	logger    logrus.FieldLogger
}

// Start begins timing an operation.
func Start(name string, logger logrus.FieldLogger) *Timer {
	return &Timer{
		name:      name,
		startTime: time.Now(),
		logger:    logger,
	}
}

// Stop ends timing and logs the duration.
func (t *Timer) Stop() time.Duration {
	duration := time.Since(t.startTime)
	if t.logger != nil {
		t.logger.WithFields(logrus.Fields{
			"operation":   t.name,
			"duration_ms": duration.Milliseconds(),
		}).Info("operation completed")
	}
	return duration
}

// StopWithThreshold logs a warning if duration exceeds threshold.
func (t *Timer) StopWithThreshold(threshold time.Duration) time.Duration {
	duration := time.Since(t.startTime)
	fields := logrus.Fields{
		"operation":   t.name,
		"duration_ms": duration.Milliseconds(),
	}
	if t.logger != nil {
		if duration > threshold {
			t.logger.WithFields(fields).Warn("operation exceeded threshold")
		} else {
			t.logger.WithFields(fields).Debug("operation completed")
		}
	}
	return duration
}

// PipelineMetrics tracks timing for the entire image processing pipeline.
type PipelineMetrics struct {
	mu sync.Mutex

	// Phase timings
	DownloadDuration  time.Duration
	UnpackDuration    time.Duration
	ActivateDuration  time.Duration
	TotalDuration     time.Duration

	// Sub-operation timings
	S3HeadDuration       time.Duration
	S3DownloadDuration   time.Duration
	ChecksumDuration     time.Duration
	CreateDeviceDuration time.Duration
	MkfsDuration         time.Duration
	MountDuration        time.Duration
	ExtractDuration      time.Duration
	VerifyDuration       time.Duration
	UnmountDuration      time.Duration
	SnapshotDuration     time.Duration
	DBWriteDuration      time.Duration

	// Wait/stabilization timings (optimization targets)
	StabilizePoolDuration    time.Duration
	UdevSettleDuration       time.Duration
	SleepDuration            time.Duration
	StabilizeAfterOpDuration time.Duration

	// Counts
	StabilizePoolCount int
	UdevSettleCount    int
	SleepCount         int
}

// NewPipelineMetrics creates a new metrics tracker.
func NewPipelineMetrics() *PipelineMetrics {
	return &PipelineMetrics{}
}

// RecordSleep records a sleep operation.
func (m *PipelineMetrics) RecordSleep(duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.SleepDuration += duration
	m.SleepCount++
}

// RecordStabilize records a stabilizePool call.
func (m *PipelineMetrics) RecordStabilize(duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.StabilizePoolDuration += duration
	m.StabilizePoolCount++
}

// RecordUdevSettle records a udev settle call.
func (m *PipelineMetrics) RecordUdevSettle(duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.UdevSettleDuration += duration
	m.UdevSettleCount++
}

// Summary returns a formatted summary of the metrics.
func (m *PipelineMetrics) Summary() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	totalWaitTime := m.StabilizePoolDuration + m.UdevSettleDuration + m.SleepDuration + m.StabilizeAfterOpDuration

	var waitPercent float64
	if m.TotalDuration > 0 {
		waitPercent = float64(totalWaitTime) / float64(m.TotalDuration) * 100
	}

	return fmt.Sprintf(`
=== Pipeline Performance Metrics ===
Total Duration:        %v

Phase Durations:
  Download:            %v
  Unpack:              %v
  Activate:            %v

Wait/Stabilization (optimization targets):
  stabilizePool:       %v (%d calls)
  udevSettle:          %v (%d calls)
  Sleep:               %v (%d calls)
  stabilizeAfterOp:    %v
  TOTAL WAIT TIME:     %v (%.1f%% of total)

Sub-Operation Timings:
  S3 Head:             %v
  S3 Download:         %v
  Checksum:            %v
  Create Device:       %v
  mkfs:                %v
  Mount:               %v
  Extract:             %v
  Verify:              %v
  Unmount:             %v
  Snapshot:            %v
  DB Write:            %v
`,
		m.TotalDuration,
		m.DownloadDuration,
		m.UnpackDuration,
		m.ActivateDuration,
		m.StabilizePoolDuration, m.StabilizePoolCount,
		m.UdevSettleDuration, m.UdevSettleCount,
		m.SleepDuration, m.SleepCount,
		m.StabilizeAfterOpDuration,
		totalWaitTime,
		waitPercent,
		m.S3HeadDuration,
		m.S3DownloadDuration,
		m.ChecksumDuration,
		m.CreateDeviceDuration,
		m.MkfsDuration,
		m.MountDuration,
		m.ExtractDuration,
		m.VerifyDuration,
		m.UnmountDuration,
		m.SnapshotDuration,
		m.DBWriteDuration,
	)
}

// contextKey is used to store metrics in context.
type contextKey struct{}

// WithMetrics adds metrics to context.
func WithMetrics(ctx context.Context, m *PipelineMetrics) context.Context {
	return context.WithValue(ctx, contextKey{}, m)
}

// MetricsFromContext retrieves metrics from context.
func MetricsFromContext(ctx context.Context) *PipelineMetrics {
	m, _ := ctx.Value(contextKey{}).(*PipelineMetrics)
	return m
}
