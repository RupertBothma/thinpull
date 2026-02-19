package tui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// CLIProgress provides simple CLI progress display without full TUI
type CLIProgress struct {
	mu sync.Mutex
	w  io.Writer

	// Configuration
	quiet   bool
	noColor bool

	// Current state
	currentPhase OperationPhase
	phaseStates  map[OperationPhase]*cliPhaseState

	// Styles
	styles *Styles

	// Timing
	startTime time.Time
}

type cliPhaseState struct {
	started   bool
	completed bool
	current   int64
	total     int64
	speed     string
	message   string
	error     error
	startTime time.Time
	elapsed   time.Duration
}

// NewCLIProgress creates a new CLI progress display
func NewCLIProgress(quiet, noColor bool) *CLIProgress {
	p := &CLIProgress{
		w:       os.Stdout,
		quiet:   quiet,
		noColor: noColor,
		phaseStates: map[OperationPhase]*cliPhaseState{
			PhaseDownload: {},
			PhaseUnpack:   {},
			PhaseActivate: {},
		},
		styles:    DefaultStyles(),
		startTime: time.Now(),
	}

	if noColor {
		// Reset styles to plain text
		p.styles = &Styles{
			Title:   lipgloss.NewStyle(),
			Success: lipgloss.NewStyle(),
			Error:   lipgloss.NewStyle(),
			Warning: lipgloss.NewStyle(),
			Info:    lipgloss.NewStyle(),
			Muted:   lipgloss.NewStyle(),
		}
	}

	return p
}

// SetWriter sets the output writer
func (p *CLIProgress) SetWriter(w io.Writer) {
	p.w = w
}

// HandleEvent handles a progress event
func (p *CLIProgress) HandleEvent(event ProgressEvent) {
	if p.quiet {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	state := p.phaseStates[event.Phase]
	if state == nil {
		state = &cliPhaseState{}
		p.phaseStates[event.Phase] = state
	}

	switch event.Type {
	case EventDownloadStart, EventUnpackStart, EventActivateStart:
		p.currentPhase = event.Phase
		state.started = true
		state.startTime = event.StartTime
		state.total = event.Total
		p.printPhaseStart(event.Phase, event.Total)

	case EventDownloadProgress, EventUnpackProgress, EventActivateProgress:
		state.current = event.Current
		state.total = event.Total
		state.speed = event.SpeedStr
		state.message = event.Message
		p.updateProgressLine(event)

	case EventDownloadComplete, EventUnpackComplete, EventActivateComplete:
		state.completed = true
		state.current = event.Total
		state.elapsed = event.Elapsed
		p.printPhaseComplete(event.Phase, event.Elapsed)

	case EventError:
		state.error = event.Error
		p.printError(event.Error)
	}
}

func (p *CLIProgress) printPhaseStart(phase OperationPhase, total int64) {
	phaseName := p.phaseName(phase)
	var sizeInfo string
	if total > 0 && phase == PhaseDownload {
		sizeInfo = fmt.Sprintf(" (%s)", FormatBytes(total))
	}
	line := fmt.Sprintf("%s %s%s...",
		p.styles.Info.Render(SymbolInProgress),
		phaseName,
		sizeInfo)
	fmt.Fprintln(p.w, line)
}

func (p *CLIProgress) updateProgressLine(event ProgressEvent) {
	// Build progress bar
	barWidth := 30
	var percent float64
	if event.Total > 0 {
		percent = float64(event.Current) / float64(event.Total)
	}
	filled := int(percent * float64(barWidth))
	empty := barWidth - filled

	bar := "[" + strings.Repeat("=", filled)
	if filled < barWidth {
		bar += ">"
		empty--
	}
	bar += strings.Repeat(" ", empty) + "]"

	// Build progress text
	var progressText string
	switch event.Phase {
	case PhaseDownload:
		progressText = fmt.Sprintf("%s %3.0f%% %s/%s",
			bar,
			percent*100,
			FormatBytes(event.Current),
			FormatBytes(event.Total))
		if event.SpeedStr != "" && event.SpeedStr != "0 B/s" {
			progressText += fmt.Sprintf(" %s", event.SpeedStr)
		}
	case PhaseUnpack:
		if event.Total > 0 {
			progressText = fmt.Sprintf("%s %3.0f%% %d/%d files",
				bar,
				percent*100,
				event.Current,
				event.Total)
		} else {
			progressText = fmt.Sprintf("%d files extracted", event.Current)
		}
	case PhaseActivate:
		if event.Message != "" {
			progressText = event.Message
		} else {
			progressText = "Creating snapshot..."
		}
	}

	// Print with carriage return to overwrite previous line
	fmt.Fprintf(p.w, "\r  %s", progressText)
}

func (p *CLIProgress) printPhaseComplete(phase OperationPhase, elapsed time.Duration) {
	// Clear the progress line and print completion
	fmt.Fprint(p.w, "\r\033[K") // Clear line
	phaseName := p.phaseName(phase)
	line := fmt.Sprintf("%s %s completed (%s)",
		p.styles.Success.Render(SymbolSuccess),
		phaseName,
		FormatDuration(elapsed))
	fmt.Fprintln(p.w, line)
}

func (p *CLIProgress) printError(err error) {
	fmt.Fprint(p.w, "\r\033[K") // Clear line
	line := fmt.Sprintf("%s Error: %v",
		p.styles.Error.Render(SymbolError),
		err)
	fmt.Fprintln(p.w, line)
}

func (p *CLIProgress) phaseName(phase OperationPhase) string {
	switch phase {
	case PhaseDownload:
		return "Downloading"
	case PhaseUnpack:
		return "Unpacking"
	case PhaseActivate:
		return "Activating"
	default:
		return string(phase)
	}
}

// PrintHeader prints a header for the operation
func (p *CLIProgress) PrintHeader(imageID, s3Key string) {
	if p.quiet {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	fmt.Fprintln(p.w)
	fmt.Fprintln(p.w, p.styles.Title.Render("Fly.io Image Manager"))
	fmt.Fprintln(p.w)
	fmt.Fprintf(p.w, "  %s %s\n", p.styles.Muted.Render("Image:"), imageID)
	fmt.Fprintf(p.w, "  %s %s\n", p.styles.Muted.Render("S3 Key:"), s3Key)
	fmt.Fprintln(p.w)
}

// PrintSummary prints a summary at the end
func (p *CLIProgress) PrintSummary(result *ProcessResult) {
	if p.quiet {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	fmt.Fprintln(p.w)
	if result.Error != nil {
		fmt.Fprintf(p.w, "%s Processing failed: %v\n",
			p.styles.Error.Render(SymbolError),
			result.Error)
	} else {
		fmt.Fprintln(p.w, p.styles.Success.Render(SymbolSuccess+" Image processed successfully!"))
		fmt.Fprintln(p.w)

		// Print result table
		fmt.Fprintf(p.w, "  %-16s %s\n", "Image ID:", result.ImageID)
		fmt.Fprintf(p.w, "  %-16s %s\n", "Snapshot ID:", result.SnapshotID)
		fmt.Fprintf(p.w, "  %-16s %s\n", "Snapshot Name:", result.SnapshotName)
		fmt.Fprintf(p.w, "  %-16s %s\n", "Device Path:", result.DevicePath)
		fmt.Fprintf(p.w, "  %-16s %s\n", "Total Time:", FormatDuration(result.TotalTime))
	}
	fmt.Fprintln(p.w)
}

// ProcessResult contains the result of the process-image command
type ProcessResult struct {
	ImageID      string
	SnapshotID   string
	SnapshotName string
	DevicePath   string
	TotalTime    time.Duration
	Error        error
}

// CreateProgressCallback creates a callback for the progress tracker
func (p *CLIProgress) CreateProgressCallback() ProgressCallback {
	return func(event ProgressEvent) {
		p.HandleEvent(event)
	}
}
