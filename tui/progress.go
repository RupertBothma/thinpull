package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// OperationPhase represents a phase of the image processing pipeline
type OperationPhase string

const (
	PhaseDownload OperationPhase = "download"
	PhaseUnpack   OperationPhase = "unpack"
	PhaseActivate OperationPhase = "activate"
)

// ProgressUpdate represents a progress update message
type ProgressUpdate struct {
	Phase       OperationPhase
	Percent     float64 // 0.0 to 1.0
	Current     int64   // Current progress (bytes for download, files for unpack)
	Total       int64   // Total (bytes for download, files for unpack)
	Status      string  // Status message
	Speed       string  // Speed indicator (e.g., "2.5MB/s")
	Error       error   // Error if any
	StartedAt   time.Time
	CompletedAt time.Time
}

// PhaseCompleteMsg indicates a phase has completed
type PhaseCompleteMsg struct {
	Phase   OperationPhase
	Success bool
	Error   error
}

// AllCompleteMsg indicates all operations are complete
type AllCompleteMsg struct {
	ImageID      string
	SnapshotID   string
	SnapshotName string
	DevicePath   string
	TotalTime    time.Duration
	Error        error
}

// ProgressModel is the Bubble Tea model for progress display
type ProgressModel struct {
	// Configuration
	ImageID string
	S3Key   string
	Quiet   bool

	// Progress bars for each phase
	downloadProgress progress.Model
	unpackProgress   progress.Model
	activateProgress progress.Model

	// Spinners for indeterminate progress
	downloadSpinner spinner.Model
	unpackSpinner   spinner.Model
	activateSpinner spinner.Model

	// Current state
	currentPhase OperationPhase
	phases       map[OperationPhase]*PhaseState

	// Styles
	styles *Styles

	// Timing
	startTime time.Time
	width     int
	done      bool
	err       error

	// Final result
	result *AllCompleteMsg
}

// PhaseState tracks the state of each phase
type PhaseState struct {
	Status      string
	Progress    float64
	Current     int64
	Total       int64
	Speed       string
	Started     bool
	Completed   bool
	Error       error
	StartedAt   time.Time
	CompletedAt time.Time
}

// NewProgressModel creates a new progress model
func NewProgressModel(imageID, s3Key string, quiet bool) *ProgressModel {
	// Download progress bar
	downloadProg := progress.New(
		progress.WithDefaultGradient(),
		progress.WithWidth(40),
	)

	// Unpack progress bar
	unpackProg := progress.New(
		progress.WithDefaultGradient(),
		progress.WithWidth(40),
	)

	// Activate progress bar
	activateProg := progress.New(
		progress.WithDefaultGradient(),
		progress.WithWidth(40),
	)

	// Spinners
	downloadSpin := spinner.New()
	downloadSpin.Spinner = spinner.Dot
	downloadSpin.Style = lipgloss.NewStyle().Foreground(ColorInfo)

	unpackSpin := spinner.New()
	unpackSpin.Spinner = spinner.Dot
	unpackSpin.Style = lipgloss.NewStyle().Foreground(ColorInfo)

	activateSpin := spinner.New()
	activateSpin.Spinner = spinner.Dot
	activateSpin.Style = lipgloss.NewStyle().Foreground(ColorInfo)

	return &ProgressModel{
		ImageID:          imageID,
		S3Key:            s3Key,
		Quiet:            quiet,
		downloadProgress: downloadProg,
		unpackProgress:   unpackProg,
		activateProgress: activateProg,
		downloadSpinner:  downloadSpin,
		unpackSpinner:    unpackSpin,
		activateSpinner:  activateSpin,
		currentPhase:     PhaseDownload,
		phases: map[OperationPhase]*PhaseState{
			PhaseDownload: {Status: "Pending"},
			PhaseUnpack:   {Status: "Pending"},
			PhaseActivate: {Status: "Pending"},
		},
		styles:    DefaultStyles(),
		startTime: time.Now(),
		width:     80,
	}
}

// Init initializes the model
func (m *ProgressModel) Init() tea.Cmd {
	return tea.Batch(
		m.downloadSpinner.Tick,
		m.unpackSpinner.Tick,
		m.activateSpinner.Tick,
	)
}

// Update handles messages
func (m *ProgressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.downloadProgress.Width = msg.Width - 20
		m.unpackProgress.Width = msg.Width - 20
		m.activateProgress.Width = msg.Width - 20

	case ProgressUpdate:
		phase := m.phases[msg.Phase]
		phase.Progress = msg.Percent
		phase.Current = msg.Current
		phase.Total = msg.Total
		phase.Speed = msg.Speed
		if msg.Status != "" {
			phase.Status = msg.Status
		}
		if !phase.Started {
			phase.Started = true
			phase.StartedAt = msg.StartedAt
			if msg.StartedAt.IsZero() {
				phase.StartedAt = time.Now()
			}
		}
		m.currentPhase = msg.Phase

	case PhaseCompleteMsg:
		phase := m.phases[msg.Phase]
		phase.Completed = true
		phase.CompletedAt = time.Now()
		if msg.Success {
			phase.Status = "Completed"
			phase.Progress = 1.0
		} else {
			phase.Status = "Failed"
			phase.Error = msg.Error
		}

	case AllCompleteMsg:
		m.done = true
		m.result = &msg
		if msg.Error != nil {
			m.err = msg.Error
		}
		return m, tea.Quit

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.downloadSpinner, cmd = m.downloadSpinner.Update(msg)
		cmds = append(cmds, cmd)
		m.unpackSpinner, cmd = m.unpackSpinner.Update(msg)
		cmds = append(cmds, cmd)
		m.activateSpinner, cmd = m.activateSpinner.Update(msg)
		cmds = append(cmds, cmd)

	case progress.FrameMsg:
		var cmd tea.Cmd
		progressModel, cmd := m.downloadProgress.Update(msg)
		m.downloadProgress = progressModel.(progress.Model)
		cmds = append(cmds, cmd)

		progressModel, cmd = m.unpackProgress.Update(msg)
		m.unpackProgress = progressModel.(progress.Model)
		cmds = append(cmds, cmd)

		progressModel, cmd = m.activateProgress.Update(msg)
		m.activateProgress = progressModel.(progress.Model)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

// View renders the model
func (m *ProgressModel) View() string {
	if m.Quiet {
		return ""
	}

	var b strings.Builder

	// Title
	title := m.styles.Title.Render("Fly.io Image Manager")
	b.WriteString(title + "\n\n")

	// Image info
	b.WriteString(fmt.Sprintf("  %s %s\n", m.styles.Muted.Render("Image:"), m.ImageID))
	b.WriteString(fmt.Sprintf("  %s %s\n\n", m.styles.Muted.Render("S3 Key:"), m.S3Key))

	// Render each phase
	b.WriteString(m.renderPhase(PhaseDownload, "Download", m.downloadSpinner, m.downloadProgress))
	b.WriteString(m.renderPhase(PhaseUnpack, "Unpack", m.unpackSpinner, m.unpackProgress))
	b.WriteString(m.renderPhase(PhaseActivate, "Activate", m.activateSpinner, m.activateProgress))

	// Elapsed time
	elapsed := time.Since(m.startTime)
	b.WriteString(fmt.Sprintf("\n  %s %s\n",
		m.styles.Muted.Render("Elapsed:"),
		FormatDuration(elapsed)))

	// Final result if complete
	if m.done && m.result != nil {
		b.WriteString("\n")
		if m.result.Error != nil {
			b.WriteString(m.styles.Error.Render(fmt.Sprintf("  %s Error: %v\n", SymbolError, m.result.Error)))
		} else {
			b.WriteString(m.styles.Success.Render(fmt.Sprintf("  %s Image processed successfully!\n", SymbolSuccess)))
			b.WriteString(fmt.Sprintf("    Image ID:      %s\n", m.result.ImageID))
			b.WriteString(fmt.Sprintf("    Snapshot ID:   %s\n", m.result.SnapshotID))
			b.WriteString(fmt.Sprintf("    Snapshot Name: %s\n", m.result.SnapshotName))
			b.WriteString(fmt.Sprintf("    Device Path:   %s\n", m.result.DevicePath))
			b.WriteString(fmt.Sprintf("    Total Time:    %s\n", FormatDuration(m.result.TotalTime)))
		}
	}

	// Help
	b.WriteString(fmt.Sprintf("\n  %s\n", m.styles.Help.Render("Press q to quit")))

	return b.String()
}

func (m *ProgressModel) renderPhase(phase OperationPhase, name string, spin spinner.Model, prog progress.Model) string {
	state := m.phases[phase]
	var b strings.Builder

	// Phase icon
	var icon string
	if state.Completed {
		if state.Error != nil {
			icon = m.styles.Error.Render(SymbolError)
		} else {
			icon = m.styles.Success.Render(SymbolSuccess)
		}
	} else if state.Started {
		icon = spin.View()
	} else {
		icon = m.styles.Muted.Render(SymbolPending)
	}

	// Phase name
	phaseName := fmt.Sprintf("%-10s", name)
	if m.currentPhase == phase && !state.Completed {
		phaseName = m.styles.Info.Render(phaseName)
	} else if state.Completed && state.Error == nil {
		phaseName = m.styles.Success.Render(phaseName)
	} else if state.Error != nil {
		phaseName = m.styles.Error.Render(phaseName)
	} else {
		phaseName = m.styles.Muted.Render(phaseName)
	}

	b.WriteString(fmt.Sprintf("  %s %s ", icon, phaseName))

	// Progress bar or status
	if state.Started && !state.Completed {
		// Show progress bar
		b.WriteString(prog.ViewAs(state.Progress))

		// Show details
		details := ""
		switch phase {
		case PhaseDownload:
			if state.Total > 0 {
				details = fmt.Sprintf(" %s/%s", FormatBytes(state.Current), FormatBytes(state.Total))
				if state.Speed != "" {
					details += fmt.Sprintf(" %s", state.Speed)
				}
			}
		case PhaseUnpack:
			if state.Total > 0 {
				details = fmt.Sprintf(" %d/%d files", state.Current, state.Total)
			} else if state.Current > 0 {
				details = fmt.Sprintf(" %d files", state.Current)
			}
		case PhaseActivate:
			if state.Status != "" && state.Status != "Pending" {
				details = fmt.Sprintf(" %s", state.Status)
			}
		}
		b.WriteString(m.styles.Muted.Render(details))
	} else if state.Completed {
		// Show completion time
		duration := state.CompletedAt.Sub(state.StartedAt)
		b.WriteString(m.styles.Muted.Render(fmt.Sprintf("(%s)", FormatDuration(duration))))
	}

	b.WriteString("\n")
	return b.String()
}

// SetProgress sends a progress update
func SetProgress(phase OperationPhase, percent float64, current, total int64, status, speed string) tea.Cmd {
	return func() tea.Msg {
		return ProgressUpdate{
			Phase:   phase,
			Percent: percent,
			Current: current,
			Total:   total,
			Status:  status,
			Speed:   speed,
		}
	}
}

// PhaseComplete sends a phase completion message
func PhaseComplete(phase OperationPhase, success bool, err error) tea.Cmd {
	return func() tea.Msg {
		return PhaseCompleteMsg{
			Phase:   phase,
			Success: success,
			Error:   err,
		}
	}
}

// AllComplete sends the final completion message
func AllComplete(imageID, snapshotID, snapshotName, devicePath string, totalTime time.Duration, err error) tea.Cmd {
	return func() tea.Msg {
		return AllCompleteMsg{
			ImageID:      imageID,
			SnapshotID:   snapshotID,
			SnapshotName: snapshotName,
			DevicePath:   devicePath,
			TotalTime:    totalTime,
			Error:        err,
		}
	}
}

// Done returns whether the model is done
func (m *ProgressModel) Done() bool {
	return m.done
}

// Error returns any error that occurred
func (m *ProgressModel) Error() error {
	return m.err
}

// Result returns the final result
func (m *ProgressModel) Result() *AllCompleteMsg {
	return m.result
}

// CreateTeaCallback creates a callback that sends progress events to a Bubble Tea program.
// This bridges the ProgressTracker callback system with the Bubble Tea event loop.
func CreateTeaCallback(p *tea.Program) ProgressCallback {
	return func(event ProgressEvent) {
		switch event.Type {
		case EventDownloadStart:
			p.Send(ProgressUpdate{
				Phase:     PhaseDownload,
				Status:    "Starting download",
				Total:     event.Total,
				StartedAt: event.StartTime,
			})
		case EventDownloadProgress:
			p.Send(ProgressUpdate{
				Phase:   PhaseDownload,
				Percent: event.Percent,
				Current: event.Current,
				Total:   event.Total,
				Speed:   event.SpeedStr,
				Status:  "Downloading",
			})
		case EventDownloadComplete:
			p.Send(PhaseCompleteMsg{
				Phase:   PhaseDownload,
				Success: true,
			})
		case EventUnpackStart:
			p.Send(ProgressUpdate{
				Phase:     PhaseUnpack,
				Status:    "Starting extraction",
				Total:     event.Total,
				StartedAt: event.StartTime,
			})
		case EventUnpackProgress:
			p.Send(ProgressUpdate{
				Phase:   PhaseUnpack,
				Percent: event.Percent,
				Current: event.Current,
				Total:   event.Total,
				Status:  event.Message,
			})
		case EventUnpackComplete:
			p.Send(PhaseCompleteMsg{
				Phase:   PhaseUnpack,
				Success: true,
			})
		case EventActivateStart:
			p.Send(ProgressUpdate{
				Phase:     PhaseActivate,
				Status:    "Creating snapshot",
				StartedAt: event.StartTime,
			})
		case EventActivateProgress:
			p.Send(ProgressUpdate{
				Phase:   PhaseActivate,
				Percent: event.Percent,
				Current: event.Current,
				Total:   event.Total,
				Status:  event.Message,
			})
		case EventActivateComplete:
			p.Send(PhaseCompleteMsg{
				Phase:   PhaseActivate,
				Success: true,
			})
		case EventError:
			p.Send(PhaseCompleteMsg{
				Phase:   event.Phase,
				Success: false,
				Error:   event.Error,
			})
		}
	}
}

// SendAllComplete sends the final completion message to a Bubble Tea program.
func SendAllComplete(p *tea.Program, imageID, snapshotID, snapshotName, devicePath string, totalTime time.Duration, err error) {
	p.Send(AllCompleteMsg{
		ImageID:      imageID,
		SnapshotID:   snapshotID,
		SnapshotName: snapshotName,
		DevicePath:   devicePath,
		TotalTime:    totalTime,
		Error:        err,
	})
}
