package main

import (
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"go.uber.org/zap"
)

// TableModel represents the table UI model
type TableModel struct {
	table table.Model
	quit  bool
}

// TableOption is a function that configures the table
type TableOption func(*TableModel)

// WithColumns sets the table columns
func WithColumns(columns []table.Column) TableOption {
	return func(m *TableModel) {
		m.table.SetColumns(columns)
	}
}

// WithRows sets the table rows
func WithRows(rows []table.Row) TableOption {
	return func(m *TableModel) {
		m.table.SetRows(rows)
	}
}

// WithTitle sets the table title
func WithTitle(title string) TableOption {
	return func(m *TableModel) {
		// Title is handled in rendering
	}
}

// NewTableModel creates a new table model with the given options
func NewTableModel(opts ...TableOption) TableModel {
	log := zap.L()
	log.Debug("NewTableModel: building interactive table", zap.Int("options", len(opts)))
	t := table.New(
		table.WithColumns([]table.Column{
			{Title: "Node", Width: 15},
		}),
		table.WithStyles(table.DefaultStyles()),
	)

	m := TableModel{
		table: t,
	}

	for _, opt := range opts {
		opt(&m)
	}

	log.Debug("NewTableModel: done", zap.Int("rows", len(m.table.Rows())))
	return m
}

// Init implements tea.Model
func (m TableModel) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model
func (m TableModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			zap.L().Debug("TableModel.Update: quit key pressed", zap.String("key", msg.String()))
			m.quit = true
			return m, tea.Quit
		}
	}

	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

// View implements tea.Model
func (m TableModel) View() string {
	if m.quit {
		return ""
	}

	baseStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("69")).
		Padding(1, 2)

	// Wrap table with border
	t := baseStyle.Render(m.table.View())
	return t
}

// RenderTableToFile renders the table directly to stdout (non-interactive)
func RenderStaticTable(title string, headers []string, rows [][]string, infoLines []string) string {
	log := zap.L()
	log.Debug("RenderStaticTable: rendering table", zap.String("title", title), zap.Strings("headers", headers), zap.Int("rows", len(rows)), zap.Int("info_lines", len(infoLines)))
	// Define styles
	borderColor := lipgloss.Color("69")
	headerColor := lipgloss.Color("230")
	headerBgColor := lipgloss.Color("63")
	primaryColor := lipgloss.Color("229")

	borderStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Padding(0, 1)

	titleStyle := lipgloss.NewStyle().
		Foreground(primaryColor).
		Bold(true).
		Padding(0, 2)

	headerStyle := lipgloss.NewStyle().
		Foreground(headerColor).
		Background(headerBgColor).
		Padding(0, 1)

	// Calculate column widths
	colWidths := make([]int, len(headers))
	for i, h := range headers {
		colWidths[i] = len(h) + 2
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell)+2 > colWidths[i] {
				colWidths[i] = len(cell) + 2
			}
		}
	}

	// Build header row
	var headerCells []string
	for i, h := range headers {
		headerCells = append(headerCells, lipgloss.NewStyle().Width(colWidths[i]).Render(h))
	}
	headerRow := headerStyle.Render(lipgloss.JoinHorizontal(lipgloss.Left, headerCells...))

	// Build data rows
	var dataRows []string
	for _, row := range rows {
		var cells []string
		for i, cell := range row {
			style := lipgloss.NewStyle().Width(colWidths[i])
			cells = append(cells, style.Render(cell))
		}
		dataRows = append(dataRows, lipgloss.JoinHorizontal(lipgloss.Left, cells...))
	}

	// Build info section
	var infoSection string
	if len(infoLines) > 0 {
		var infoCells []string
		for _, line := range infoLines {
			infoCells = append(infoCells, "  "+line+"  ")
		}
		infoSection = strings.Join(infoCells, "\n")
		if len(infoCells) > 0 {
			infoSection = "\n" + infoSection
		}
	}

	// Combine everything
	content := headerRow + "\n" + strings.Join(dataRows, "\n") + infoSection

	// Render title if provided
	if title != "" {
		log.Debug("RenderStaticTable: adding title row", zap.String("title", title))
		titleRow := lipgloss.NewStyle().Width(lipgloss.Width(content)).Render(title)
		titleRendered := titleStyle.Render(titleRow)
		content = titleRendered + "\n" + content
	}

	log.Debug("RenderStaticTable: rendered", zap.Int("rows", len(dataRows)), zap.Ints("col_widths", colWidths))
	return borderStyle.Render(content)
}

// RenderResourceTable renders a resource status table
func RenderResourceTable(resourceName, role string, nodeStates []NodeStateInfo) string {
	log := zap.L()
	log.Debug("RenderResourceTable: rendering resource table", zap.String("resource", resourceName), zap.String("role", role), zap.Int("nodes", len(nodeStates)))
	headers := []string{"Node", "Role", "Disk State", "Replication"}

	var rows [][]string
	for _, ns := range nodeStates {
		icon := "○"
		if ns.Role == "Primary" {
			icon = "●"
		}
		rows = append(rows, []string{
			icon + " " + ns.Node,
			ns.Role,
			ns.DiskState,
			ns.ReplicationState,
		})
	}

	infoLines := []string{} // Empty - no extra info lines

	return RenderStaticTable("", headers, rows, infoLines)
}

// NodeStateInfo represents node state for display
type NodeStateInfo struct {
	Node             string
	Role             string
	DiskState        string
	ReplicationState string
}
