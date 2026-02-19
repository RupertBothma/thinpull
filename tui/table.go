package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Column represents a table column
type Column struct {
	Title string
	Width int
}

// Row represents a table row
type Row []string

// Table renders data in a styled table format
type Table struct {
	columns []Column
	rows    []Row
	styles  *Styles
}

// NewTable creates a new table with the given columns
func NewTable(columns []Column) *Table {
	return &Table{
		columns: columns,
		rows:    []Row{},
		styles:  DefaultStyles(),
	}
}

// AddRow adds a row to the table
func (t *Table) AddRow(row Row) {
	t.rows = append(t.rows, row)
}

// SetRows sets all rows at once
func (t *Table) SetRows(rows []Row) {
	t.rows = rows
}

// Render renders the table as a string
func (t *Table) Render() string {
	var b strings.Builder

	// Header
	headerCells := make([]string, len(t.columns))
	for i, col := range t.columns {
		cell := t.styles.TableHeader.Width(col.Width).Render(col.Title)
		headerCells[i] = cell
	}
	b.WriteString(strings.Join(headerCells, " ") + "\n")

	// Separator
	for _, col := range t.columns {
		b.WriteString(strings.Repeat("─", col.Width) + " ")
	}
	b.WriteString("\n")

	// Rows
	for _, row := range t.rows {
		rowCells := make([]string, len(t.columns))
		for i, col := range t.columns {
			var cell string
			if i < len(row) {
				cell = row[i]
			}
			// Truncate if too long
			if len(cell) > col.Width {
				cell = cell[:col.Width-3] + "..."
			}
			rowCells[i] = t.styles.TableCell.Width(col.Width).Render(cell)
		}
		b.WriteString(strings.Join(rowCells, " ") + "\n")
	}

	return b.String()
}

// RenderSimple renders a simple table without borders
func RenderSimple(headers []string, rows [][]string, styles *Styles) string {
	if styles == nil {
		styles = DefaultStyles()
	}

	var b strings.Builder

	// Calculate column widths
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	// Header
	for i, h := range headers {
		cell := styles.TableHeader.Width(widths[i] + 2).Render(h)
		b.WriteString(cell)
	}
	b.WriteString("\n")

	// Rows
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) {
				styled := styles.TableRow.Width(widths[i] + 2).Render(cell)
				b.WriteString(styled)
			}
		}
		b.WriteString("\n")
	}

	return b.String()
}

// ImageRow represents an image for table display
type ImageRow struct {
	ImageID      string
	S3Key        string
	Size         string
	Status       string
	Activation   string
	DownloadedAt string
}

// RenderImagesTable renders a table of images
func RenderImagesTable(images []ImageRow) string {
	styles := DefaultStyles()
	var b strings.Builder

	// Title
	b.WriteString(styles.Title.Render("Downloaded Images") + "\n\n")

	if len(images) == 0 {
		b.WriteString(styles.Muted.Render("  No images found\n"))
		return b.String()
	}

	// Calculate column widths
	columns := []Column{
		{Title: "STATUS", Width: 8},
		{Title: "IMAGE ID", Width: 16},
		{Title: "S3 KEY", Width: 30},
		{Title: "SIZE", Width: 10},
		{Title: "ACTIVATION", Width: 10},
		{Title: "DOWNLOADED", Width: 20},
	}

	// Header
	var headerLine string
	for _, col := range columns {
		cell := styles.TableHeader.Width(col.Width).Render(col.Title)
		headerLine += cell + " "
	}
	b.WriteString(headerLine + "\n")

	// Separator
	for _, col := range columns {
		b.WriteString(styles.Muted.Render(strings.Repeat("─", col.Width)) + " ")
	}
	b.WriteString("\n")

	// Rows
	for _, img := range images {
		icon := styles.StatusIcon(img.Status)

		// Truncate image ID
		imageID := img.ImageID
		if len(imageID) > 14 {
			imageID = imageID[:14] + ".."
		}

		// Truncate S3 key
		s3Key := img.S3Key
		if len(s3Key) > 28 {
			s3Key = s3Key[:28] + ".."
		}

		cells := []string{icon, imageID, s3Key, img.Size, img.Activation, img.DownloadedAt}
		for i, col := range columns {
			var cell string
			if i < len(cells) {
				cell = cells[i]
			}
			styled := lipgloss.NewStyle().Width(col.Width).Render(cell)
			b.WriteString(styled + " ")
		}
		b.WriteString("\n")
	}

	b.WriteString(fmt.Sprintf("\n%s %d images\n", styles.Muted.Render("Total:"), len(images)))

	return b.String()
}

// SnapshotRow represents a snapshot for table display
type SnapshotRow struct {
	SnapshotID   string
	ImageID      string
	SnapshotName string
	DevicePath   string
	Active       string
	CreatedAt    string
}

// RenderSnapshotsTable renders a table of snapshots
func RenderSnapshotsTable(snapshots []SnapshotRow) string {
	styles := DefaultStyles()
	var b strings.Builder

	// Title
	b.WriteString(styles.Title.Render("Active Snapshots") + "\n\n")

	if len(snapshots) == 0 {
		b.WriteString(styles.Muted.Render("  No active snapshots found\n"))
		return b.String()
	}

	// Calculate column widths
	columns := []Column{
		{Title: "STATUS", Width: 8},
		{Title: "SNAPSHOT ID", Width: 12},
		{Title: "IMAGE ID", Width: 16},
		{Title: "DEVICE PATH", Width: 30},
		{Title: "CREATED", Width: 20},
	}

	// Header
	var headerLine string
	for _, col := range columns {
		cell := styles.TableHeader.Width(col.Width).Render(col.Title)
		headerLine += cell + " "
	}
	b.WriteString(headerLine + "\n")

	// Separator
	for _, col := range columns {
		b.WriteString(styles.Muted.Render(strings.Repeat("─", col.Width)) + " ")
	}
	b.WriteString("\n")

	// Rows
	for _, snap := range snapshots {
		status := "active"
		if snap.Active != "true" && snap.Active != "1" {
			status = "inactive"
		}
		icon := styles.StatusIcon(status)

		// Truncate IDs
		snapshotID := snap.SnapshotID
		if len(snapshotID) > 10 {
			snapshotID = snapshotID[:10] + ".."
		}

		imageID := snap.ImageID
		if len(imageID) > 14 {
			imageID = imageID[:14] + ".."
		}

		// Truncate device path
		devicePath := snap.DevicePath
		if len(devicePath) > 28 {
			devicePath = devicePath[:28] + ".."
		}

		cells := []string{icon, snapshotID, imageID, devicePath, snap.CreatedAt}
		for i, col := range columns {
			var cell string
			if i < len(cells) {
				cell = cells[i]
			}
			styled := lipgloss.NewStyle().Width(col.Width).Render(cell)
			b.WriteString(styled + " ")
		}
		b.WriteString("\n")
	}

	b.WriteString(fmt.Sprintf("\n%s %d snapshots\n", styles.Muted.Render("Total:"), len(snapshots)))

	return b.String()
}
