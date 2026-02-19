package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// FSMRun represents an active FSM run
type FSMRun struct {
	ID          string
	Type        string // download, unpack, activate
	ImageID     string
	State       string
	Progress    float64
	CurrentStep string
	StartedAt   time.Time
	UpdatedAt   time.Time
	Error       string
}

// SystemStatus represents the current system status
type SystemStatus struct {
	PoolName      string
	PoolDataUsed  int64
	PoolDataTotal int64
	PoolMetaUsed  int64
	PoolMetaTotal int64
	PoolError     string // Error message if pool status fetch failed
	TotalImages   int
	UnpackedCount int
	ActiveSnaps   int
	FSMDBPath     string
	DBPath        string // Path to SQLite database
	DBError       string // Error from database connection/query
	DBConnected   bool   // Whether database is connected
}

// LogEntry represents a log entry
type LogEntry struct {
	Timestamp time.Time
	Level     string // info, warn, error
	Message   string
	Fields    map[string]string
}

// DashboardUpdateMsg is sent when dashboard data is updated
type DashboardUpdateMsg struct {
	ActiveRuns     []FSMRun
	SystemStatus   *SystemStatus
	RecentActivity []LogEntry
}

// LogUpdateMsg is sent when new log entries arrive
type LogUpdateMsg struct {
	Entries []LogEntry
}

// TickMsg is sent periodically to update the dashboard
type TickMsg time.Time

// ViewMode represents the current view mode
type ViewMode int

const (
	ViewModeDashboard ViewMode = iota // Default monitoring dashboard
	ViewModeS3Browser                 // S3 image browser for selection/unpack
)

// DashboardModel is the main TUI dashboard model
type DashboardModel struct {
	// Configuration
	title           string
	width           int
	height          int
	refreshInterval time.Duration

	// Components
	spinner    spinner.Model
	logView    viewport.Model
	helpHeight int

	// Data fetcher
	fetcher *DataFetcher

	// Data
	activeRuns      []FSMRun
	registeredFSMs  []string
	systemStatus    *SystemStatus
	logs            []LogEntry
	maxLogs         int
	lastRefresh     time.Time
	connectionError error

	// S3 Browser state
	s3Browser      *S3BrowserState
	s3BrowserError error

	// View mode
	viewMode ViewMode

	// FSM operation state
	processingImage string // S3 key of image being processed (if any)
	processError    error

	// Real-time processing progress
	processingProgress *ProcessingProgressMsg

	// State
	focused   string // "runs", "status", "logs", "s3list"
	styles    *Styles
	startTime time.Time
	quitting  bool
	err       error
}

// DashboardConfig holds configuration for the dashboard.
type DashboardConfig struct {
	Title           string
	RefreshInterval time.Duration
	Fetcher         *DataFetcher
}

// DefaultDashboardConfig returns default dashboard configuration.
func DefaultDashboardConfig() DashboardConfig {
	return DashboardConfig{
		Title:           "Fly.io Image Manager Dashboard",
		RefreshInterval: time.Second,
	}
}

// NewDashboardModel creates a new dashboard model
func NewDashboardModel() *DashboardModel {
	return NewDashboardModelWithConfig(DefaultDashboardConfig())
}

// NewDashboardModelWithConfig creates a new dashboard model with custom configuration.
func NewDashboardModelWithConfig(cfg DashboardConfig) *DashboardModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(ColorPrimary)

	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = time.Second
	}
	if cfg.Title == "" {
		cfg.Title = "Fly.io Image Manager Dashboard"
	}

	return &DashboardModel{
		title:           cfg.Title,
		refreshInterval: cfg.RefreshInterval,
		fetcher:         cfg.Fetcher,
		spinner:         s,
		logView:         viewport.New(80, 10),
		helpHeight:      2,
		activeRuns:      []FSMRun{},
		registeredFSMs:  []string{},
		systemStatus: &SystemStatus{
			PoolName: "pool",
		},
		logs:      []LogEntry{},
		maxLogs:   100,
		s3Browser: NewS3BrowserState(),
		viewMode:  ViewModeDashboard,
		focused:   "runs",
		styles:    DefaultStyles(),
		startTime: time.Now(),
	}
}

// Init initializes the dashboard
func (m *DashboardModel) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		tickEvery(m.refreshInterval),
		m.fetchData(),
	)
}

// FetchDataMsg is sent when data fetch completes
type FetchDataMsg struct {
	Data  *DashboardUpdateMsg
	Error error
}

// fetchData creates a command to fetch dashboard data
func (m *DashboardModel) fetchData() tea.Cmd {
	return func() tea.Msg {
		if m.fetcher == nil {
			return FetchDataMsg{Error: nil}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		data, err := m.fetcher.FetchDashboardData(ctx)
		return FetchDataMsg{Data: data, Error: err}
	}
}

// tickEvery creates a command that sends a tick message periodically
func tickEvery(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}

// S3ImagesMsg is sent when S3 images are fetched
type S3ImagesMsg struct {
	Images []S3Image
	Error  error
}

// ProcessImageMsg is sent when an image process operation completes
type ProcessImageMsg struct {
	S3Key string
	Error error
}

// ProcessingProgressMsg is sent during image processing to update real-time progress
type ProcessingProgressMsg struct {
	S3Key       string
	Phase       string               // "download", "unpack", "activate"
	Status      string               // Current status message
	Percent     float64              // 0.0 to 1.0
	Current     int64                // Bytes downloaded or files extracted
	Total       int64                // Total bytes or files
	Speed       string               // Download speed (e.g., "2.5 MB/s")
	ElapsedTime string               // Elapsed time string
	progressCh  <-chan ProgressEvent // Channel to continue listening (internal)
}

// Update handles messages
func (m *DashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKeyMsg(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.logView.Width = msg.Width - 4
		m.logView.Height = msg.Height/3 - 2
		// Update S3 browser visible rows based on height
		if m.s3Browser != nil {
			m.s3Browser.VisibleRows = (m.height - 10) / 2
			if m.s3Browser.VisibleRows < 5 {
				m.s3Browser.VisibleRows = 5
			}
		}

	case DashboardUpdateMsg:
		m.activeRuns = msg.ActiveRuns
		if msg.SystemStatus != nil {
			m.systemStatus = msg.SystemStatus
		}

	case LogUpdateMsg:
		for _, entry := range msg.Entries {
			m.logs = append(m.logs, entry)
			if len(m.logs) > m.maxLogs {
				m.logs = m.logs[1:]
			}
		}
		m.logView.SetContent(m.renderLogs())

	case TickMsg:
		cmds = append(cmds, tickEvery(m.refreshInterval))
		cmds = append(cmds, m.fetchData())

	case FetchDataMsg:
		m.lastRefresh = time.Now()
		if msg.Error != nil {
			m.connectionError = msg.Error
		} else {
			m.connectionError = nil
		}
		if msg.Data != nil {
			m.activeRuns = msg.Data.ActiveRuns
			if msg.Data.SystemStatus != nil {
				m.systemStatus = msg.Data.SystemStatus
			}
			if len(msg.Data.RecentActivity) > 0 {
				m.logs = msg.Data.RecentActivity
				m.logView.SetContent(m.renderLogs())
			}
		}

	case S3ImagesMsg:
		m.s3Browser.Loading = false
		if msg.Error != nil {
			m.s3BrowserError = msg.Error
		} else {
			m.s3Browser.Images = msg.Images
			m.s3BrowserError = nil
		}

	case ProcessImageMsg:
		m.AddLog("info", fmt.Sprintf("ProcessImageMsg received for: %s", msg.S3Key), nil)
		m.processingImage = ""
		m.processingProgress = nil // Clear progress when complete
		if msg.Error != nil {
			m.processError = msg.Error
			m.AddLog("error", fmt.Sprintf("Failed to process %s: %v", msg.S3Key, msg.Error), nil)
		} else {
			m.processError = nil
			m.AddLog("info", fmt.Sprintf("Successfully processed %s", msg.S3Key), nil)
			// Refresh dashboard data to show updated counts
			cmds = append(cmds, m.fetchData())
			// Refresh S3 browser to update status
			if m.fetcher != nil {
				cmds = append(cmds, m.fetchS3Images())
			}
		}

	case ProcessingProgressMsg:
		// Update real-time progress during processing
		// Make a copy to avoid issues with the switch case variable
		progressCopy := msg
		m.processingProgress = &progressCopy
		// DEBUG: Always log received progress messages
		m.AddLog("debug", fmt.Sprintf("[PROGRESS] phase=%s percent=%.1f%% status=%s",
			msg.Phase, msg.Percent*100, msg.Status), nil)
		// Log significant progress updates (phase changes)
		if msg.Status != "" && (msg.Percent == 0 || msg.Percent >= 0.99) {
			m.AddLog("info", fmt.Sprintf("[%s] %s", msg.Phase, msg.Status), nil)
		}
		// Continue listening for more progress if we have a channel
		if msg.progressCh != nil {
			cmds = append(cmds, m.listenForProgress(msg.S3Key, msg.progressCh))
		} else {
			m.AddLog("warn", "ProcessingProgressMsg received but progressCh is nil", nil)
		}

	case progressListenCmd:
		// Continue listening for progress events
		cmds = append(cmds, m.listenForProgress(msg.s3Key, msg.progressCh))

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

// handleKeyMsg handles keyboard input based on current view mode
func (m *DashboardModel) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "1":
		// Switch to dashboard view
		m.viewMode = ViewModeDashboard
		m.focused = "runs"
		m.AddLog("info", "Switched to Monitor view (viewMode=0)", nil)

	case "2":
		// Switch to S3 browser view
		m.viewMode = ViewModeS3Browser
		m.focused = "s3list"
		m.AddLog("info", "Switched to Images view (viewMode=1)", nil)
		// Fetch S3 images if not already loaded
		if len(m.s3Browser.Images) == 0 && !m.s3Browser.Loading && m.fetcher != nil {
			m.s3Browser.Loading = true
			m.AddLog("info", "Loading S3 images...", nil)
			cmds = append(cmds, m.fetchS3Images())
		}

	case "tab":
		if m.viewMode == ViewModeDashboard {
			switch m.focused {
			case "runs":
				m.focused = "status"
			case "status":
				m.focused = "logs"
			case "logs":
				m.focused = "runs"
			}
		}

	case "j", "down":
		if m.viewMode == ViewModeS3Browser {
			m.s3Browser.MoveDown()
		} else if m.focused == "logs" {
			m.logView.LineDown(1)
		}

	case "k", "up":
		if m.viewMode == ViewModeS3Browser {
			m.s3Browser.MoveUp()
		} else if m.focused == "logs" {
			m.logView.LineUp(1)
		}

	case "g":
		if m.viewMode == ViewModeS3Browser {
			m.s3Browser.SelectedIdx = 0
			m.s3Browser.ScrollOffset = 0
		} else if m.focused == "logs" {
			m.logView.GotoTop()
		}

	case "G":
		if m.viewMode == ViewModeS3Browser {
			if len(m.s3Browser.Images) > 0 {
				m.s3Browser.SelectedIdx = len(m.s3Browser.Images) - 1
				if m.s3Browser.SelectedIdx >= m.s3Browser.VisibleRows {
					m.s3Browser.ScrollOffset = m.s3Browser.SelectedIdx - m.s3Browser.VisibleRows + 1
				}
			}
		} else if m.focused == "logs" {
			m.logView.GotoBottom()
		}

	case "enter":
		// Debug: Log that Enter was pressed regardless of conditions
		m.AddLog("info", fmt.Sprintf("Enter pressed: viewMode=%d, processingImage=%q, s3Browser=%v, images=%d",
			m.viewMode, m.processingImage, m.s3Browser != nil, len(m.s3Browser.Images)), nil)

		if m.viewMode == ViewModeS3Browser && m.processingImage == "" {
			if img := m.s3Browser.SelectedImage(); img != nil {
				// Trigger image processing
				m.processingImage = img.Key
				m.AddLog("info", fmt.Sprintf("Starting process for %s...", ImageName(img.Key)), nil)
				cmds = append(cmds, m.processImage(img.Key))
			} else {
				m.AddLog("warn", "Enter pressed but no image selected", nil)
			}
		}

	case "r":
		// Manual refresh
		cmds = append(cmds, m.fetchData())
		if m.viewMode == ViewModeS3Browser && m.fetcher != nil {
			m.s3Browser.Loading = true
			cmds = append(cmds, m.fetchS3Images())
		}
	}

	return m, tea.Batch(cmds...)
}

// fetchS3Images creates a command to fetch S3 images
func (m *DashboardModel) fetchS3Images() tea.Cmd {
	return func() tea.Msg {
		if m.fetcher == nil {
			return S3ImagesMsg{Error: fmt.Errorf("fetcher not configured")}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		images, err := m.fetcher.FetchS3Images(ctx)
		return S3ImagesMsg{Images: images, Error: err}
	}
}

// processImage creates a command to trigger image processing with real-time progress updates.
// It returns a batch of commands: one to start processing and one to listen for progress.
func (m *DashboardModel) processImage(s3Key string) tea.Cmd {
	// Log immediately that the command was created
	m.AddLog("info", fmt.Sprintf("processImage cmd created for: %s", s3Key), nil)
	m.AddLog("debug", "Creating progressCh channel (cap=100)", nil)

	// Create a channel for progress updates
	progressCh := make(chan ProgressEvent, 100)

	// Start the processing in a goroutine
	processCmd := func() tea.Msg {
		debugLog("processCmd: starting TriggerImageProcessWithProgress")

		if m.fetcher == nil {
			close(progressCh)
			return ProcessImageMsg{S3Key: s3Key, Error: fmt.Errorf("fetcher not configured")}
		}

		ctx := context.Background()
		err := m.fetcher.TriggerImageProcessWithProgress(ctx, s3Key, progressCh)

		debugLog("processCmd: TriggerImageProcessWithProgress completed, err=%v", err)

		close(progressCh)
		return ProcessImageMsg{S3Key: s3Key, Error: err}
	}

	// Create a command that listens for progress updates and sends them as messages
	listenCmd := m.listenForProgress(s3Key, progressCh)

	m.AddLog("debug", "Returning tea.Batch with processCmd and listenCmd", nil)

	return tea.Batch(processCmd, listenCmd)
}

// listenForProgress creates a command that listens for progress events and converts them to tea messages.
func (m *DashboardModel) listenForProgress(s3Key string, progressCh <-chan ProgressEvent) tea.Cmd {
	return func() tea.Msg {
		debugLog("listenForProgress: waiting for event on channel")

		event, ok := <-progressCh
		if !ok {
			// Channel closed, no more progress
			debugLog("listenForProgress: channel closed")
			return nil
		}

		debugLog("listenForProgress: received event type=%s phase=%s percent=%.2f",
			event.Type, event.Phase, event.Percent)

		// Convert ProgressEvent to ProcessingProgressMsg
		phase := "unknown"
		switch event.Phase {
		case PhaseDownload:
			phase = "download"
		case PhaseUnpack:
			phase = "unpack"
		case PhaseActivate:
			phase = "activate"
		}

		return ProcessingProgressMsg{
			S3Key:      s3Key,
			Phase:      phase,
			Status:     event.Message,
			Percent:    event.Percent,
			Current:    event.Current,
			Total:      event.Total,
			Speed:      event.SpeedStr,
			progressCh: progressCh, // Include channel to continue listening
		}
	}
}

// progressListenCmd is a message to continue listening for progress
type progressListenCmd struct {
	s3Key      string
	progressCh <-chan ProgressEvent
}

// View renders the dashboard
func (m *DashboardModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// Title bar with view mode indicator
	titleStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(ColorPrimary).
		Background(lipgloss.Color("#1E1E2E")).
		Padding(0, 2).
		Width(m.width)

	uptime := time.Since(m.startTime)
	connStatus := m.styles.Success.Render("●")
	if m.connectionError != nil {
		connStatus = m.styles.Error.Render("●")
	}

	// View mode tabs
	tab1 := "[1] Monitor"
	tab2 := "[2] Images"
	if m.viewMode == ViewModeDashboard {
		tab1 = m.styles.Info.Render("[1] Monitor")
		tab2 = m.styles.Muted.Render("[2] Images")
	} else {
		tab1 = m.styles.Muted.Render("[1] Monitor")
		tab2 = m.styles.Info.Render("[2] Images")
	}

	title := fmt.Sprintf("%s  %s %s  %s  %s  Uptime: %s",
		m.spinner.View(),
		m.title,
		connStatus,
		tab1, tab2,
		FormatDuration(uptime))
	b.WriteString(titleStyle.Render(title) + "\n\n")

	// Render based on view mode
	switch m.viewMode {
	case ViewModeS3Browser:
		b.WriteString(m.renderS3BrowserView())
	default:
		b.WriteString(m.renderDashboardView())
	}

	// Help
	help := m.renderHelp()
	b.WriteString(help)

	return b.String()
}

// renderDashboardView renders the default monitoring dashboard
func (m *DashboardModel) renderDashboardView() string {
	var b strings.Builder

	// Calculate section widths
	halfWidth := (m.width - 4) / 2 // Account for borders

	// Active FSM Runs panel
	runsPanel := m.renderRunsPanel(halfWidth)

	// System Status panel
	statusPanel := m.renderStatusPanel(halfWidth)

	// Put runs and status side by side
	topSection := lipgloss.JoinHorizontal(lipgloss.Top, runsPanel, "  ", statusPanel)
	b.WriteString(topSection + "\n\n")

	// Activity Log panel (full width)
	logsPanel := m.renderLogsPanel()
	b.WriteString(logsPanel + "\n")

	return b.String()
}

// renderS3BrowserView renders the S3 image browser view
func (m *DashboardModel) renderS3BrowserView() string {
	var b strings.Builder

	halfWidth := (m.width - 4) / 2

	// S3 Images panel (left)
	s3Panel := m.renderS3ListPanel(halfWidth)

	// System Status panel (right) - same as dashboard view
	statusPanel := m.renderStatusPanel(halfWidth)

	topSection := lipgloss.JoinHorizontal(lipgloss.Top, s3Panel, "  ", statusPanel)
	b.WriteString(topSection + "\n\n")

	// Activity Log panel (full width)
	logsPanel := m.renderLogsPanel()
	b.WriteString(logsPanel + "\n")

	return b.String()
}

// renderS3ListPanel renders the S3 image list panel
func (m *DashboardModel) renderS3ListPanel(width int) string {
	var content strings.Builder

	// Calculate usable content width (panel width minus borders/padding)
	// Panel has 1 char border on each side, so content width = width - 2
	contentWidth := width - 4
	if contentWidth < 20 {
		contentWidth = 20
	}

	if m.s3Browser.Loading {
		content.WriteString(m.styles.Muted.Render("  Loading images from S3...\n"))
	} else if m.s3BrowserError != nil {
		content.WriteString(m.styles.Error.Render(fmt.Sprintf("  Error: %v\n", m.s3BrowserError)))
	} else if len(m.s3Browser.Images) == 0 {
		content.WriteString(m.styles.Muted.Render("  No images found. Press 'r' to refresh.\n"))
	} else {
		// Calculate visible range
		start := m.s3Browser.ScrollOffset
		end := start + m.s3Browser.VisibleRows
		if end > len(m.s3Browser.Images) {
			end = len(m.s3Browser.Images)
		}

		for i := start; i < end; i++ {
			img := m.s3Browser.Images[i]
			isSelected := i == m.s3Browser.SelectedIdx

			// Build compact line WITHOUT any ANSI styling first
			// Format: "  > ● python   v1  [downloaded]" or "    ○ golang   v2"
			// This ensures consistent character widths

			// Cursor: 2 chars total
			cursor := "  "
			if isSelected {
				cursor = "> "
			}

			// Status icon: 1 char (use ASCII for consistency)
			var statusIcon, statusTag string
			switch img.Status {
			case ImageStatusActive:
				statusIcon = "*"
				statusTag = "active"
			case ImageStatusUnpacked:
				statusIcon = "+"
				statusTag = "unpacked"
			case ImageStatusDownloaded:
				statusIcon = "o"
				statusTag = "downloaded"
			default:
				statusIcon = "-"
				statusTag = ""
			}

			// Runtime: 8 chars padded
			runtime := fmt.Sprintf("%-7s", ImageRuntime(img.Key))

			// Version: 3 chars
			version := fmt.Sprintf("v%s", ImageVersion(img.Key))

			// Build the plain text line (no ANSI codes)
			var line string
			if statusTag != "" {
				line = fmt.Sprintf("  %s%s %s %s [%s]", cursor, statusIcon, runtime, version, statusTag)
			} else {
				line = fmt.Sprintf("  %s%s %s %s", cursor, statusIcon, runtime, version)
			}

			// Truncate if too long
			if len(line) > contentWidth {
				line = line[:contentWidth-3] + "..."
			}

			// Pad to consistent width to prevent shifting
			line = fmt.Sprintf("%-*s", contentWidth, line)

			// NOW apply styling to the whole line
			if isSelected && m.processingImage == img.Key {
				line = m.styles.Warning.Render(line)
			} else if isSelected {
				line = m.styles.Info.Render(line)
			} else {
				// Apply subtle styling based on status
				switch img.Status {
				case ImageStatusActive:
					line = m.styles.Success.Render(line)
				case ImageStatusDownloaded, ImageStatusUnpacked:
					// Keep default color
				default:
					line = m.styles.Muted.Render(line)
				}
			}

			content.WriteString(line + "\n")
		}

		// Scroll indicator
		if len(m.s3Browser.Images) > m.s3Browser.VisibleRows {
			content.WriteString(m.styles.Muted.Render(fmt.Sprintf(
				"  [%d-%d of %d]", start+1, end, len(m.s3Browser.Images))))
		}
		// Compact legend
		content.WriteString(m.styles.Muted.Render("\n  - avail  o down  + unpack  * active"))
	}

	// Processing indicator with real-time progress
	if m.processingImage != "" {
		content.WriteString("\n")
		if m.processingProgress != nil {
			// Show detailed progress
			p := m.processingProgress
			phaseIcon := "⏳"
			switch p.Phase {
			case "download":
				phaseIcon = "⬇️"
			case "unpack":
				phaseIcon = "📦"
			case "activate":
				phaseIcon = "🚀"
			}

			// Progress bar
			barWidth := 20
			filled := int(p.Percent * float64(barWidth))
			if filled > barWidth {
				filled = barWidth
			}
			bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

			// Format progress line
			progressLine := fmt.Sprintf("  %s %s [%s] %.0f%%",
				phaseIcon, p.Phase, bar, p.Percent*100)
			content.WriteString(m.styles.Warning.Render(progressLine) + "\n")

			// Status message
			if p.Status != "" {
				content.WriteString(m.styles.Muted.Render(fmt.Sprintf("     %s", p.Status)) + "\n")
			}

			// Speed and size info for download
			if p.Phase == "download" && p.Speed != "" {
				sizeInfo := fmt.Sprintf("     %s / %s @ %s",
					FormatBytes(p.Current), FormatBytes(p.Total), p.Speed)
				content.WriteString(m.styles.Muted.Render(sizeInfo) + "\n")
			}
		} else {
			// Fallback: just show processing indicator with spinner
			content.WriteString(m.styles.Warning.Render(
				fmt.Sprintf("  %s Processing: %s", m.spinner.View(), ImageName(m.processingImage))) + "\n")
			content.WriteString(m.styles.Muted.Render("     Waiting for progress...") + "\n")
		}
	}

	// Show last processing error if any
	if m.processError != nil && m.processingImage == "" {
		content.WriteString("\n")
		errMsg := m.processError.Error()
		// Truncate long error messages
		if len(errMsg) > 60 {
			errMsg = errMsg[:57] + "..."
		}
		content.WriteString(m.styles.Error.Render(fmt.Sprintf("  ❌ Error: %s", errMsg)) + "\n")
		// Add hint for common issues
		if strings.Contains(m.processError.Error(), "D-state") ||
			strings.Contains(m.processError.Error(), "unstable") {
			content.WriteString(m.styles.Warning.Render("     ⚠️  System needs reboot") + "\n")
		}
	}

	panelStyle := m.styles.ActivePanel
	return panelStyle.Width(width).Render(
		m.styles.SectionHead.Render("S3 Images (Enter to process)") + "\n" +
			content.String())
}

func (m *DashboardModel) renderRunsPanel(width int) string {
	var content strings.Builder

	if len(m.activeRuns) == 0 {
		content.WriteString(m.styles.Muted.Render("  No active FSM runs\n"))
	} else {
		for _, run := range m.activeRuns {
			icon := m.styles.StatusIcon(run.State)
			typeLabel := fmt.Sprintf("%-10s", run.Type)
			imageID := run.ImageID
			if len(imageID) > 12 {
				imageID = imageID[:12] + "..."
			}

			line := fmt.Sprintf("  %s %s %s %s\n",
				icon,
				m.styles.Info.Render(typeLabel),
				m.styles.Muted.Render(imageID),
				m.styles.Muted.Render(run.State))
			content.WriteString(line)

			// Progress bar if available
			if run.Progress > 0 && run.Progress < 1 {
				bar := renderProgressBar(run.Progress, width-6)
				content.WriteString(fmt.Sprintf("    %s\n", bar))
			}
		}
	}

	panelStyle := m.styles.Panel
	if m.focused == "runs" {
		panelStyle = m.styles.ActivePanel
	}

	return panelStyle.Width(width).Render(
		m.styles.SectionHead.Render("Active FSM Runs") + "\n" +
			content.String())
}

func (m *DashboardModel) renderStatusPanel(width int) string {
	var content strings.Builder

	status := m.systemStatus

	// Database status - always show for debugging
	dbStatus := m.styles.Success.Render("●")
	if !status.DBConnected {
		dbStatus = m.styles.Error.Render("●")
	}
	dbPath := status.DBPath
	if dbPath == "" {
		dbPath = "(not configured)"
	} else if len(dbPath) > 30 {
		dbPath = "..." + dbPath[len(dbPath)-27:]
	}
	content.WriteString(fmt.Sprintf("  %s %s %s\n",
		m.styles.Muted.Render("DB:"),
		dbStatus,
		dbPath))
	if status.DBError != "" {
		errMsg := status.DBError
		if len(errMsg) > 35 {
			errMsg = errMsg[:35] + "..."
		}
		content.WriteString(m.styles.Error.Render(fmt.Sprintf("    %s\n", errMsg)))
	}

	// Pool usage
	if status.PoolDataTotal > 0 {
		dataUsedPct := float64(status.PoolDataUsed) / float64(status.PoolDataTotal)
		content.WriteString(fmt.Sprintf("  %s %s / %s (%.1f%%)\n",
			m.styles.Muted.Render("Pool Data:"),
			FormatBytes(status.PoolDataUsed),
			FormatBytes(status.PoolDataTotal),
			dataUsedPct*100))

		metaUsedPct := float64(status.PoolMetaUsed) / float64(status.PoolMetaTotal)
		content.WriteString(fmt.Sprintf("  %s %s / %s (%.1f%%)\n",
			m.styles.Muted.Render("Pool Meta:"),
			FormatBytes(status.PoolMetaUsed),
			FormatBytes(status.PoolMetaTotal),
			metaUsedPct*100))
	} else if status.PoolError != "" {
		// Show the actual error for debugging
		errMsg := status.PoolError
		if len(errMsg) > 40 {
			errMsg = errMsg[:40] + "..."
		}
		content.WriteString(m.styles.Error.Render(fmt.Sprintf("  Pool: %s\n", errMsg)))
	} else {
		content.WriteString(m.styles.Muted.Render("  Pool status unavailable\n"))
	}

	content.WriteString("\n")

	// Image statistics
	content.WriteString(fmt.Sprintf("  %s %d\n",
		m.styles.Muted.Render("Total Images:"),
		status.TotalImages))
	content.WriteString(fmt.Sprintf("  %s %d\n",
		m.styles.Muted.Render("Unpacked:"),
		status.UnpackedCount))
	content.WriteString(fmt.Sprintf("  %s %d\n",
		m.styles.Muted.Render("Active Snapshots:"),
		status.ActiveSnaps))

	panelStyle := m.styles.Panel
	if m.focused == "status" {
		panelStyle = m.styles.ActivePanel
	}

	return panelStyle.Width(width).Render(
		m.styles.SectionHead.Render("System Status") + "\n" +
			content.String())
}

func (m *DashboardModel) renderLogsPanel() string {
	panelStyle := m.styles.Panel
	if m.focused == "logs" {
		panelStyle = m.styles.ActivePanel
	}

	content := m.renderLogs()
	if content == "" {
		content = m.styles.Muted.Render("  No log entries yet")
	}

	// Calculate available height for logs
	logsHeight := 10
	if m.height > 0 {
		logsHeight = m.height/3 - 4
		if logsHeight < 5 {
			logsHeight = 5
		}
	}

	m.logView.Height = logsHeight
	m.logView.SetContent(content)

	return panelStyle.Width(m.width - 4).Render(
		m.styles.SectionHead.Render("Activity Log") + "\n" +
			m.logView.View())
}

func (m *DashboardModel) renderLogs() string {
	var b strings.Builder

	for _, entry := range m.logs {
		timestamp := entry.Timestamp.Format("15:04:05")
		var levelStyle lipgloss.Style
		switch entry.Level {
		case "error":
			levelStyle = m.styles.Error
		case "warn":
			levelStyle = m.styles.Warning
		default:
			levelStyle = m.styles.Info
		}

		line := fmt.Sprintf("  %s %s %s",
			m.styles.Muted.Render(timestamp),
			levelStyle.Render(fmt.Sprintf("%-5s", entry.Level)),
			entry.Message)
		b.WriteString(line + "\n")
	}

	return b.String()
}

func (m *DashboardModel) renderHelp() string {
	helpStyle := lipgloss.NewStyle().
		Foreground(ColorMuted).
		Padding(0, 2)

	var keys []struct {
		key  string
		desc string
	}

	// Common keys
	commonKeys := []struct {
		key  string
		desc string
	}{
		{"1", "monitor"},
		{"2", "images"},
		{"r", "refresh"},
		{"q", "quit"},
	}

	// View-specific keys
	if m.viewMode == ViewModeS3Browser {
		keys = []struct {
			key  string
			desc string
		}{
			{"j/k", "navigate"},
			{"g/G", "top/bottom"},
			{"Enter", "process image"},
		}
	} else {
		keys = []struct {
			key  string
			desc string
		}{
			{"Tab", "switch panel"},
			{"j/k", "scroll logs"},
			{"g/G", "top/bottom"},
		}
	}

	// Combine keys
	keys = append(keys, commonKeys...)

	var parts []string
	for _, k := range keys {
		parts = append(parts,
			fmt.Sprintf("%s %s",
				m.styles.HelpKey.Render(k.key),
				m.styles.HelpDesc.Render(k.desc)))
	}

	return helpStyle.Render(strings.Join(parts, "  •  "))
}

// renderProgressBar creates a simple text-based progress bar
func renderProgressBar(progress float64, width int) string {
	filled := int(progress * float64(width))
	empty := width - filled

	bar := strings.Repeat("█", filled) + strings.Repeat("░", empty)
	return lipgloss.NewStyle().Foreground(ColorPrimary).Render(bar)
}

// AddLog adds a log entry to the dashboard
func (m *DashboardModel) AddLog(level, message string, fields map[string]string) {
	m.logs = append(m.logs, LogEntry{
		Timestamp: time.Now(),
		Level:     level,
		Message:   message,
		Fields:    fields,
	})
	if len(m.logs) > m.maxLogs {
		m.logs = m.logs[1:]
	}
}

// UpdateRuns updates the active FSM runs
func (m *DashboardModel) UpdateRuns(runs []FSMRun) {
	m.activeRuns = runs
}

// UpdateStatus updates the system status
func (m *DashboardModel) UpdateStatus(status *SystemStatus) {
	m.systemStatus = status
}
