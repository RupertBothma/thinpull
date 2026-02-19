package tui

import (
	"sync"
	"time"
)

// ProgressCallback is a function called with progress updates
type ProgressCallback func(event ProgressEvent)

// ProgressEventType identifies the type of progress event
type ProgressEventType string

const (
	EventDownloadStart    ProgressEventType = "download_start"
	EventDownloadProgress ProgressEventType = "download_progress"
	EventDownloadComplete ProgressEventType = "download_complete"
	EventUnpackStart      ProgressEventType = "unpack_start"
	EventUnpackProgress   ProgressEventType = "unpack_progress"
	EventUnpackComplete   ProgressEventType = "unpack_complete"
	EventActivateStart    ProgressEventType = "activate_start"
	EventActivateProgress ProgressEventType = "activate_progress"
	EventActivateComplete ProgressEventType = "activate_complete"
	EventError            ProgressEventType = "error"
)

// ProgressEvent represents a progress event
type ProgressEvent struct {
	Type      ProgressEventType
	Phase     OperationPhase
	Timestamp time.Time

	// Progress metrics
	Current int64   // Current progress (bytes or files)
	Total   int64   // Total expected
	Percent float64 // 0.0 to 1.0

	// Speed/rate (for downloads)
	Speed    float64 // bytes/second
	SpeedStr string  // human-readable speed

	// Timing
	StartTime time.Time
	Elapsed   time.Duration
	ETA       time.Duration

	// Status message
	Message string

	// Error (if EventError)
	Error error
}

// ProgressTracker tracks progress and notifies callbacks
type ProgressTracker struct {
	mu        sync.RWMutex
	callbacks []ProgressCallback

	// Current state
	phase     OperationPhase
	current   int64
	total     int64
	startTime time.Time
}

// NewProgressTracker creates a new progress tracker
func NewProgressTracker() *ProgressTracker {
	return &ProgressTracker{
		callbacks: []ProgressCallback{},
	}
}

// Subscribe adds a callback to receive progress updates
func (p *ProgressTracker) Subscribe(callback ProgressCallback) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.callbacks = append(p.callbacks, callback)
}

// StartPhase begins tracking a new phase
func (p *ProgressTracker) StartPhase(phase OperationPhase, total int64) {
	p.mu.Lock()
	p.phase = phase
	p.current = 0
	p.total = total
	p.startTime = time.Now()
	p.mu.Unlock()

	p.emit(ProgressEvent{
		Type:      eventTypeForPhaseStart(phase),
		Phase:     phase,
		Timestamp: time.Now(),
		Total:     total,
		StartTime: time.Now(),
	})
}

// Update updates the current progress
func (p *ProgressTracker) Update(current int64) {
	p.mu.Lock()
	p.current = current
	phase := p.phase
	total := p.total
	startTime := p.startTime
	p.mu.Unlock()

	elapsed := time.Since(startTime)
	var percent float64
	var speed float64
	var eta time.Duration

	if total > 0 {
		percent = float64(current) / float64(total)
	}

	if elapsed.Seconds() > 0 {
		speed = float64(current) / elapsed.Seconds()
		if speed > 0 && total > current {
			eta = time.Duration(float64(total-current)/speed) * time.Second
		}
	}

	p.emit(ProgressEvent{
		Type:      eventTypeForPhaseProgress(phase),
		Phase:     phase,
		Timestamp: time.Now(),
		Current:   current,
		Total:     total,
		Percent:   percent,
		Speed:     speed,
		SpeedStr:  FormatBytes(int64(speed)) + "/s",
		StartTime: startTime,
		Elapsed:   elapsed,
		ETA:       eta,
	})
}

// UpdateWithTotal updates progress with both current and total values.
// This is useful when the total is not known at phase start (e.g., S3 downloads).
func (p *ProgressTracker) UpdateWithTotal(current, total int64) {
	p.mu.Lock()
	p.current = current
	p.total = total // Update the total dynamically
	phase := p.phase
	startTime := p.startTime
	p.mu.Unlock()

	elapsed := time.Since(startTime)
	var percent float64
	var speed float64
	var eta time.Duration

	if total > 0 {
		percent = float64(current) / float64(total)
	}

	if elapsed.Seconds() > 0 {
		speed = float64(current) / elapsed.Seconds()
		if speed > 0 && total > current {
			eta = time.Duration(float64(total-current)/speed) * time.Second
		}
	}

	p.emit(ProgressEvent{
		Type:      eventTypeForPhaseProgress(phase),
		Phase:     phase,
		Timestamp: time.Now(),
		Current:   current,
		Total:     total,
		Percent:   percent,
		Speed:     speed,
		SpeedStr:  FormatBytes(int64(speed)) + "/s",
		StartTime: startTime,
		Elapsed:   elapsed,
		ETA:       eta,
	})
}

// UpdateWithMessage updates progress with a status message
func (p *ProgressTracker) UpdateWithMessage(current int64, message string) {
	p.mu.Lock()
	p.current = current
	phase := p.phase
	total := p.total
	startTime := p.startTime
	p.mu.Unlock()

	elapsed := time.Since(startTime)
	var percent float64
	if total > 0 {
		percent = float64(current) / float64(total)
	}

	p.emit(ProgressEvent{
		Type:      eventTypeForPhaseProgress(phase),
		Phase:     phase,
		Timestamp: time.Now(),
		Current:   current,
		Total:     total,
		Percent:   percent,
		StartTime: startTime,
		Elapsed:   elapsed,
		Message:   message,
	})
}

// CompletePhase marks the current phase as complete
func (p *ProgressTracker) CompletePhase() {
	p.mu.RLock()
	phase := p.phase
	total := p.total
	startTime := p.startTime
	p.mu.RUnlock()

	elapsed := time.Since(startTime)

	p.emit(ProgressEvent{
		Type:      eventTypeForPhaseComplete(phase),
		Phase:     phase,
		Timestamp: time.Now(),
		Current:   total,
		Total:     total,
		Percent:   1.0,
		StartTime: startTime,
		Elapsed:   elapsed,
	})
}

// ReportError reports an error
func (p *ProgressTracker) ReportError(err error) {
	p.mu.RLock()
	phase := p.phase
	p.mu.RUnlock()

	p.emit(ProgressEvent{
		Type:      EventError,
		Phase:     phase,
		Timestamp: time.Now(),
		Error:     err,
	})
}

func (p *ProgressTracker) emit(event ProgressEvent) {
	p.mu.RLock()
	callbacks := make([]ProgressCallback, len(p.callbacks))
	copy(callbacks, p.callbacks)
	numCallbacks := len(callbacks)
	p.mu.RUnlock()

	debugLog("emit: type=%s phase=%s callbacks=%d percent=%.2f",
		event.Type, event.Phase, numCallbacks, event.Percent)

	for _, cb := range callbacks {
		cb(event)
	}
}

func eventTypeForPhaseStart(phase OperationPhase) ProgressEventType {
	switch phase {
	case PhaseDownload:
		return EventDownloadStart
	case PhaseUnpack:
		return EventUnpackStart
	case PhaseActivate:
		return EventActivateStart
	default:
		return EventDownloadStart
	}
}

func eventTypeForPhaseProgress(phase OperationPhase) ProgressEventType {
	switch phase {
	case PhaseDownload:
		return EventDownloadProgress
	case PhaseUnpack:
		return EventUnpackProgress
	case PhaseActivate:
		return EventActivateProgress
	default:
		return EventDownloadProgress
	}
}

func eventTypeForPhaseComplete(phase OperationPhase) ProgressEventType {
	switch phase {
	case PhaseDownload:
		return EventDownloadComplete
	case PhaseUnpack:
		return EventUnpackComplete
	case PhaseActivate:
		return EventActivateComplete
	default:
		return EventDownloadComplete
	}
}
