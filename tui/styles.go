// Package tui provides Terminal User Interface components for the Fly.io Image Manager.
package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Color palette for consistent theming
var (
	// Primary colors
	ColorPrimary    = lipgloss.Color("#7D56F4") // Purple
	ColorSecondary  = lipgloss.Color("#6C757D") // Gray
	ColorSuccess    = lipgloss.Color("#28A745") // Green
	ColorWarning    = lipgloss.Color("#FFC107") // Yellow
	ColorError      = lipgloss.Color("#DC3545") // Red
	ColorInfo       = lipgloss.Color("#17A2B8") // Blue
	ColorMuted      = lipgloss.Color("#6C757D") // Muted gray
	ColorBackground = lipgloss.Color("#1E1E2E") // Dark background
	ColorForeground = lipgloss.Color("#CDD6F4") // Light foreground
)

// Status indicator symbols
const (
	SymbolSuccess    = "✓"
	SymbolError      = "✗"
	SymbolWarning    = "⚠"
	SymbolInProgress = "⟳"
	SymbolPending    = "○"
	SymbolArrow      = "→"
	SymbolBullet     = "•"
)

// Styles provides consistent styling across the TUI
type Styles struct {
	// Title styles
	Title       lipgloss.Style
	Subtitle    lipgloss.Style
	SectionHead lipgloss.Style

	// Status styles
	Success lipgloss.Style
	Error   lipgloss.Style
	Warning lipgloss.Style
	Info    lipgloss.Style
	Muted   lipgloss.Style

	// Component styles
	Box         lipgloss.Style
	Panel       lipgloss.Style
	ActivePanel lipgloss.Style

	// Table styles
	TableHeader lipgloss.Style
	TableRow    lipgloss.Style
	TableCell   lipgloss.Style

	// Progress bar styles
	ProgressBar lipgloss.Style

	// Help text
	Help     lipgloss.Style
	HelpKey  lipgloss.Style
	HelpDesc lipgloss.Style
}

// DefaultStyles returns the default style configuration
func DefaultStyles() *Styles {
	return &Styles{
		Title: lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorPrimary).
			MarginBottom(1),

		Subtitle: lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorForeground),

		SectionHead: lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorInfo).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(ColorSecondary).
			MarginBottom(1),

		Success: lipgloss.NewStyle().
			Foreground(ColorSuccess),

		Error: lipgloss.NewStyle().
			Foreground(ColorError),

		Warning: lipgloss.NewStyle().
			Foreground(ColorWarning),

		Info: lipgloss.NewStyle().
			Foreground(ColorInfo),

		Muted: lipgloss.NewStyle().
			Foreground(ColorMuted),

		Box: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorSecondary).
			Padding(1, 2),

		Panel: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorSecondary).
			Padding(0, 1),

		ActivePanel: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorPrimary).
			Padding(0, 1),

		TableHeader: lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorPrimary).
			BorderStyle(lipgloss.NormalBorder()).
			BorderBottom(true).
			BorderForeground(ColorSecondary),

		TableRow: lipgloss.NewStyle().
			Foreground(ColorForeground),

		TableCell: lipgloss.NewStyle().
			PaddingRight(2),

		ProgressBar: lipgloss.NewStyle().
			Foreground(ColorPrimary),

		Help: lipgloss.NewStyle().
			Foreground(ColorMuted),

		HelpKey: lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorInfo),

		HelpDesc: lipgloss.NewStyle().
			Foreground(ColorMuted),
	}
}

// StatusIcon returns a styled status icon based on the status
func (s *Styles) StatusIcon(status string) string {
	switch status {
	case "success", "completed", "done", "active":
		return s.Success.Render(SymbolSuccess)
	case "error", "failed", "aborted":
		return s.Error.Render(SymbolError)
	case "warning":
		return s.Warning.Render(SymbolWarning)
	case "in_progress", "doing", "running", "downloading", "unpacking", "activating":
		return s.Info.Render(SymbolInProgress)
	case "pending", "todo", "queued":
		return s.Muted.Render(SymbolPending)
	default:
		return s.Muted.Render(SymbolBullet)
	}
}

// FormatBytes formats bytes into a human-readable string
func FormatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// FormatDuration formats duration into a human-readable string
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}
