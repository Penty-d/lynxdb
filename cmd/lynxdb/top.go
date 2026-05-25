package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	zone "github.com/lrstanley/bubblezone/v2"
	"github.com/spf13/cobra"

	"github.com/lynxbase/lynxdb/internal/shell"
	"github.com/lynxbase/lynxdb/internal/ui"
	"github.com/lynxbase/lynxdb/pkg/client"
)

func init() {
	rootCmd.AddCommand(newTopCmd())
}

func newTopCmd() *cobra.Command {
	var interval string

	cmd := &cobra.Command{
		Use:   "top",
		Short: "Live TUI dashboard of server metrics",
		Long:  `Full-screen live dashboard showing ingest, storage, query, memory, and cluster state. Press q to quit.`,
		Example: `  lynxdb top
  lynxdb top --interval 5s`,
		RunE: func(_ *cobra.Command, _ []string) error {
			dur, err := time.ParseDuration(interval)
			if err != nil {
				return fmt.Errorf("invalid --interval: %w", err)
			}
			if dur < 250*time.Millisecond {
				dur = 250 * time.Millisecond
			}

			return runTop(dur)
		},
	}

	cmd.Flags().StringVar(&interval, "interval", "2s", "Refresh interval (e.g., 2s, 5s)")

	return cmd
}

type topFetchedMsg struct {
	snapshot *client.TopSnapshot
	err      error
}

type topCancelMsg struct {
	jobID string
	err   error
}

type topClipboardMsg struct {
	action string
	err    error
}

type topTickMsg struct{}

type topSortMode int

const (
	topSortAge topSortMode = iota
	topSortProgress
	topSortMemory
	topSortSpill
)

var topSortLabels = []string{"age", "progress", "memory", "spill"}

type topModel struct {
	spinner  spinner.Model
	theme    *ui.Theme
	server   string
	client   *client.Client
	interval time.Duration

	snapshot *client.TopSnapshot
	loaded   bool
	err      error
	lastLoad time.Time

	width  int
	height int
	focus  int

	paused bool
	help   bool

	filtering   bool
	filter      string
	filterInput string
	sortMode    topSortMode
	selected    int
	scroll      int

	confirmCancel bool
	cancelStatus  string
	expandedJobID string
	detailMode    string
	actionStatus  string

	histories map[string][]float64
}

func newTopModel(server string, interval time.Duration) topModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = ui.Stdout.Accent

	return topModel{
		spinner:   s,
		theme:     ui.Stdout,
		server:    server,
		client:    apiClient(),
		interval:  interval,
		width:     100,
		height:    30,
		histories: make(map[string][]float64),
	}
}

func (m topModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, fetchTopSnapshotCmd(m.client))
}

func (m topModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case topFetchedMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.snapshot = msg.snapshot
			m.loaded = true
			m.err = nil
			m.lastLoad = time.Now()
			m.updateHistories()
			m.clampSelection()
		}
		return m, m.nextTick()
	case topTickMsg:
		if m.paused {
			return m, m.nextTick()
		}
		return m, fetchTopSnapshotCmd(m.client)
	case topCancelMsg:
		if msg.err != nil {
			m.cancelStatus = fmt.Sprintf("cancel %s failed: %v", msg.jobID, msg.err)
		} else {
			m.cancelStatus = "cancel requested for " + msg.jobID
		}
		m.confirmCancel = false
		return m, fetchTopSnapshotCmd(m.client)
	case topClipboardMsg:
		if msg.err != nil {
			m.actionStatus = fmt.Sprintf("%s failed: %v", msg.action, msg.err)
		} else {
			m.actionStatus = msg.action + " copied"
		}
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)

		return m, cmd
	}

	return m, nil
}

func (m topModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	if m.filtering {
		switch key {
		case "esc":
			m.filtering = false
			m.filterInput = m.filter
		case "enter":
			m.filter = strings.TrimSpace(m.filterInput)
			m.filtering = false
			m.selected = 0
			m.scroll = 0
			m.clampSelection()
		case "backspace", "ctrl+h":
			r := []rune(m.filterInput)
			if len(r) > 0 {
				m.filterInput = string(r[:len(r)-1])
			}
		default:
			if len([]rune(key)) == 1 {
				m.filterInput += key
			}
		}

		return m, nil
	}

	if m.confirmCancel {
		switch key {
		case "y", "Y":
			row, ok := m.selectedRow()
			if !ok {
				m.confirmCancel = false
				return m, nil
			}
			return m, cancelTopJobCmd(m.client, row.JobID)
		case "n", "N", "esc":
			m.confirmCancel = false
			return m, nil
		}
	}

	switch key {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "?":
		m.help = !m.help
	case "p":
		m.paused = !m.paused
	case "r":
		m.err = nil
		return m, fetchTopSnapshotCmd(m.client)
	case "tab":
		m.focus = (m.focus + 1) % 8
	case "shift+tab":
		m.focus = (m.focus + 7) % 8
	case "up", "k":
		if m.selected > 0 {
			m.selected--
			if m.selected < m.scroll {
				m.scroll = m.selected
			}
		}
	case "down", "j":
		rows := m.sortedFilteredRows()
		if m.selected+1 < len(rows) {
			m.selected++
			visible := m.visibleQueryRows()
			if visible > 0 && m.selected >= m.scroll+visible {
				m.scroll = m.selected - visible + 1
			}
		}
	case "s":
		m.sortMode = (m.sortMode + 1) % topSortMode(len(topSortLabels))
		m.selected = 0
		m.scroll = 0
	case "enter", "d":
		row, ok := m.selectedRow()
		if ok {
			m.toggleExpanded(row.JobID, "detail")
		}
	case "c":
		row, ok := m.selectedRow()
		if ok {
			return m, copyTopTextCmd("query", row.Query)
		}
	case "f":
		row, ok := m.selectedRow()
		if ok {
			m.expandedJobID = row.JobID
			m.detailMode = "profile"
			return m, copyTopTextCmd("profile command", profileCommandForQuery(row.Query))
		}
	case "/":
		m.filtering = true
		m.filterInput = m.filter
	case "x":
		row, ok := m.selectedRow()
		if ok && row.Status == "running" {
			m.confirmCancel = true
		}
	}

	return m, nil
}

func (m *topModel) toggleExpanded(jobID, mode string) {
	if m.expandedJobID == jobID && m.detailMode == mode {
		m.expandedJobID = ""
		m.detailMode = ""
		return
	}
	m.expandedJobID = jobID
	m.detailMode = mode
}

func (m topModel) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	for _, action := range []string{"copy", "cancel", "profile", "detail"} {
		if z := zone.Get("top-action-" + action); z != nil && z.InBounds(msg) {
			row, ok := m.selectedRow()
			if !ok {
				return m, nil
			}
			switch action {
			case "copy":
				return m, copyTopTextCmd("query", row.Query)
			case "cancel":
				if row.Status == "running" {
					m.confirmCancel = true
				}
			case "profile":
				m.expandedJobID = row.JobID
				m.detailMode = "profile"
				return m, copyTopTextCmd("profile command", profileCommandForQuery(row.Query))
			case "detail":
				m.toggleExpanded(row.JobID, "detail")
			}

			return m, nil
		}
	}

	for i := 0; i < 8; i++ {
		if z := zone.Get(fmt.Sprintf("top-panel-%d", i)); z != nil && z.InBounds(msg) {
			m.focus = i
			return m, nil
		}
	}
	rows := m.sortedFilteredRows()
	for i, row := range rows {
		if z := zone.Get("top-query-" + row.JobID); z != nil && z.InBounds(msg) {
			m.focus = 7
			m.selected = i
			m.expandedJobID = row.JobID
			m.detailMode = "detail"
			m.clampSelection()
			return m, nil
		}
	}

	return m, nil
}

func (m topModel) nextTick() tea.Cmd {
	return tea.Tick(m.interval, func(time.Time) tea.Msg { return topTickMsg{} })
}

func (m topModel) View() tea.View {
	out := m.renderView()
	v := tea.NewView(zone.Scan(out))
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion

	return v
}

func (m topModel) renderView() string {
	if !m.loaded {
		status := m.theme.Dim.Render("Connecting to " + m.server + "...")
		if m.err != nil {
			status = m.theme.Error.Render(m.err.Error())
		}
		return "\n  " + m.spinner.View() + " " + status + "\n"
	}

	var b strings.Builder
	b.WriteString(m.renderHeader())
	b.WriteByte('\n')

	panels := [][]string{
		m.renderOverviewPanel(0),
		m.renderIngestPanel(1),
		m.renderQueriesPanel(2),
		m.renderMemoryPanel(3),
		m.renderStoragePanel(4),
		m.renderClusterPanel(5),
		m.renderTopIndexesPanel(6),
	}
	b.WriteString(m.renderPanelGrid(panels))
	b.WriteByte('\n')
	b.WriteString(m.renderActiveQueriesPanel(7))

	if m.help {
		b.WriteByte('\n')
		b.WriteString(m.renderHelp())
	}
	if m.confirmCancel {
		b.WriteByte('\n')
		b.WriteString(m.theme.Warning.Render("Confirm cancel selected query? y/n"))
	}
	if m.cancelStatus != "" {
		b.WriteByte('\n')
		b.WriteString(m.theme.Dim.Render(m.cancelStatus))
	}
	if m.actionStatus != "" {
		b.WriteByte('\n')
		b.WriteString(m.theme.Dim.Render(m.actionStatus))
	}

	return b.String()
}

func (m topModel) renderHeader() string {
	s := m.snapshot
	t := m.theme
	state := "live"
	if m.paused {
		state = "paused"
	}
	if m.err != nil {
		if m.paused {
			state = "paused/error"
		} else {
			state = "error"
		}
	}
	last := "never"
	if !m.lastLoad.IsZero() {
		last = m.lastLoad.Format("15:04:05")
	}
	left := fmt.Sprintf(" LynxDB %s  %s  %s", valueOr(s.Server.Version, "dev"), m.server, s.Server.Health)
	right := fmt.Sprintf("%s  refresh %s  updated %s", state, m.interval, last)
	if m.err != nil {
		right = right + "  " + clip(m.err.Error(), 42)
	}
	if maxRight := max(12, m.width/2); lipgloss.Width(right) > maxRight {
		right = clip(right, maxRight)
	}
	if maxLeft := m.width - lipgloss.Width(right) - 1; maxLeft > 0 && lipgloss.Width(left) > maxLeft {
		left = clip(left, maxLeft)
	}
	pad := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if pad < 1 {
		pad = 1
	}

	return t.Bold.Render(left) + strings.Repeat(" ", pad) + t.Dim.Render(right)
}

func (m topModel) renderOverviewPanel(id int) []string {
	s := m.snapshot
	memUsed, memLimit := memoryUsedLimit(s)
	body := []string{
		kv("health", s.Server.Health),
		kv("ingest", fmt.Sprintf("%s eps", formatCount(int64(math.Round(s.Events.IngestRateEPS))))),
		kv("queries", formatCount(int64(s.Queries.Active))),
		kv("storage", formatBytes(s.Storage.UsedBytes)),
		kv("memory", memorySummary(memUsed, memLimit)),
		kv("cache", pct(s.Queries.CacheHitRate)),
	}
	return m.panel(id, "Overview", body)
}

func (m topModel) renderIngestPanel(id int) []string {
	s := m.snapshot
	body := []string{
		kv("rate", fmt.Sprintf("%s eps %s", formatCount(int64(math.Round(s.Events.IngestRateEPS))), sparkline(m.histories["ingest"], 20))),
		kv("today", fmt.Sprintf("%s events", formatCountHuman(s.Events.Today))),
		kv("total", fmt.Sprintf("%s events", formatCountHuman(s.Events.Total))),
		kv("buffered", fmt.Sprintf("%s events", formatCount(s.Events.Buffered))),
	}
	return m.panel(id, "Ingest", body)
}

func (m topModel) renderQueriesPanel(id int) []string {
	s := m.snapshot
	spillRows := 0
	for _, r := range s.Queries.Rows {
		if r.SpillBytes > 0 || r.SpillFiles > 0 {
			spillRows++
		}
	}
	body := []string{
		kv("active", fmt.Sprintf("%d %s", s.Queries.Active, sparkline(m.histories["queries"], 18))),
		kv("recent", formatCount(int64(s.Queries.Recent))),
		kv("cache hit", pct(s.Queries.CacheHitRate)+" "+sparkline(m.histories["cache"], 14)),
		kv("spilling", formatCount(int64(spillRows))),
		kv("sort", topSortLabels[m.sortMode]),
	}
	if m.filter != "" || m.filtering {
		body = append(body, kv("filter", clip(m.activeFilterText(), 24)))
	}
	return m.panel(id, "Queries", body)
}

func (m topModel) renderMemoryPanel(id int) []string {
	s := m.snapshot
	used, limit := memoryUsedLimit(s)
	body := []string{
		kv("governor", memorySummary(used, limit)+" "+sparkline(m.histories["memory"], 14)),
		kv("spill", fmt.Sprintf("%s / %d files", formatBytes(s.Memory.SpillBytes), s.Memory.SpillFiles)),
	}
	body = append(body, governorMemoryClassRows(s.Memory.Governor)...)
	if bm := s.Memory.BufferManager; bm != nil {
		usedFrames := bm.TotalFrames - bm.FreeFrames
		body = append(body,
			kv("frames", fmt.Sprintf("%d/%d", usedFrames, bm.TotalFrames)),
			kv("dirty", formatCount(int64(bm.DirtyFrames))),
			kv("hits", formatCount(bm.HitCount)),
		)
	} else {
		body = append(body, kv("frames", "n/a"))
	}
	return m.panel(id, "Memory", body)
}

func governorMemoryClassRows(gov *client.TopGovernorStats) []string {
	if gov == nil {
		return nil
	}
	labels := []string{"non-revocable", "revocable", "spillable", "page-cache", "metadata", "temp-io"}
	rows := make([]string, 0, len(gov.ByClass))
	for i, class := range gov.ByClass {
		if class.Allocated == 0 {
			continue
		}
		label := fmt.Sprintf("class-%d", i)
		if i < len(labels) {
			label = labels[i]
		}
		rows = append(rows, kv(label, formatBytes(class.Allocated)))
	}

	return rows
}

func (m topModel) renderStoragePanel(id int) []string {
	s := m.snapshot
	levels := []string{"L0", "L1", "L2", "L3"}
	levelParts := make([]string, 0, len(levels))
	for _, level := range levels {
		levelParts = append(levelParts, fmt.Sprintf("%s:%d", level, s.Storage.SegmentsByLevel[level]))
	}
	body := []string{
		kv("used", formatBytes(s.Storage.UsedBytes)),
		kv("segments", formatCount(int64(s.Storage.SegmentCount))),
		kv("bytes", formatBytes(s.Storage.SegmentBytes)),
		kv("levels", strings.Join(levelParts, " ")),
		kv("oldest", formatRelativeOrNA(s.Storage.OldestEvent)),
	}
	return m.panel(id, "Storage", body)
}

func (m topModel) renderClusterPanel(id int) []string {
	c := m.snapshot.Cluster
	nodeSummary := fmt.Sprintf("%d node", c.NodeCount)
	if c.NodeCount != 1 {
		nodeSummary += "s"
	}
	body := []string{
		kv("status", c.Status),
		kv("topology", nodeSummary),
		kv("roles", roleSummary(c)),
		kv("data dir", clip(c.DataDir, 28)),
		kv("buffer", fmt.Sprintf("%s events", formatCount(c.BufferedEvents))),
	}
	return m.panel(id, "Cluster", body)
}

func (m topModel) renderTopIndexesPanel(id int) []string {
	indexes := append([]client.TopIndexSnapshot(nil), m.snapshot.Indexes...)
	sort.Slice(indexes, func(i, j int) bool {
		if indexes[i].ActiveQueries != indexes[j].ActiveQueries {
			return indexes[i].ActiveQueries > indexes[j].ActiveQueries
		}
		if indexes[i].EventCount != indexes[j].EventCount {
			return indexes[i].EventCount > indexes[j].EventCount
		}
		if indexes[i].SizeBytes != indexes[j].SizeBytes {
			return indexes[i].SizeBytes > indexes[j].SizeBytes
		}
		return indexes[i].Name < indexes[j].Name
	})

	body := []string{}
	if len(indexes) == 0 {
		body = append(body, m.theme.Dim.Render("no indexes yet"))
	} else {
		maxEvents := int64(1)
		for _, idx := range indexes {
			if idx.EventCount > maxEvents {
				maxEvents = idx.EventCount
			}
		}
		limit := min(5, len(indexes))
		for i := 0; i < limit; i++ {
			idx := indexes[i]
			body = append(body, fmt.Sprintf("%-12s %7s %4d seg %2d q %s",
				clip(idx.Name, 12),
				formatCountHuman(idx.EventCount),
				idx.SegmentCount,
				idx.ActiveQueries,
				loadBar(idx.EventCount, maxEvents, 8)))
		}
	}
	return m.panel(id, "Top Indexes", body)
}

func (m topModel) renderActiveQueriesPanel(id int) string {
	width := max(48, m.width-4)
	rows := m.sortedFilteredRows()
	visible := m.visibleQueryRows()
	if visible < 1 {
		visible = 1
	}
	if m.scroll > max(0, len(rows)-visible) {
		m.scroll = max(0, len(rows)-visible)
	}
	end := min(len(rows), m.scroll+visible)

	title := "Active Queries"
	if m.filter != "" {
		title += " / " + m.filter
	}
	titleText := title
	if m.focus == id {
		titleText = "* " + title
	}
	lines := []string{zone.Mark(fmt.Sprintf("top-panel-%d", id), topPanelHeader(m.theme, titleText, width))}
	contentW := max(0, width-4)
	layout := topQueryLayoutForWidth(contentW)
	lines = append(lines, topPanelLine(m.theme, m.renderQueryActions(contentW), width))
	lines = append(lines, topPanelLine(m.theme, m.theme.Label.Render(layout.header()), width))

	if len(rows) == 0 {
		lines = append(lines, topPanelLine(m.theme, m.theme.Dim.Render("no active or recent queries"), width))
	} else {
		for i := m.scroll; i < end; i++ {
			row := rows[i]
			line := m.queryRowLine(row, i == m.selected, layout)
			line = zone.Mark("top-query-"+row.JobID, line)
			lines = append(lines, topPanelLine(m.theme, line, width))
			if row.JobID == m.expandedJobID {
				for _, detail := range m.renderQueryDetail(row, contentW) {
					lines = append(lines, topPanelLine(m.theme, detail, width))
				}
			}
		}
	}
	lines = append(lines, topPanelFooter(m.theme, width))

	return strings.Join(lines, "\n") + "\n"
}

func (m topModel) renderQueryActions(width int) string {
	selected := "none"
	if row, ok := m.selectedRow(); ok {
		selected = row.JobID
	}
	actions := []string{
		zone.Mark("top-action-detail", m.theme.Info.Render("[detail]")),
		zone.Mark("top-action-copy", m.theme.Info.Render("[copy]")),
		zone.Mark("top-action-profile", m.theme.Info.Render("[profile]")),
		zone.Mark("top-action-cancel", m.theme.Warning.Render("[cancel]")),
	}
	line := "selected " + clip(selected, 18) + "  " + strings.Join(actions, " ")
	return fitContent(line, width)
}

type topQueryTableLayout struct {
	id     int
	status int
	age    int
	phase  int
	prog   int
	rows   int
	seg    int
	mem    int
	spill  int
	query  int
}

func topQueryLayoutForWidth(width int) topQueryTableLayout {
	l := topQueryTableLayout{
		id:     13,
		status: 9,
		age:    7,
		phase:  18,
		prog:   7,
		rows:   10,
		seg:    11,
		mem:    9,
		spill:  10,
	}
	fixed := l.id + l.status + l.age + l.phase + l.prog + l.rows + l.seg + l.mem + l.spill + 9
	l.query = width - fixed
	if l.query < 12 {
		shortfall := 12 - l.query
		for shortfall > 0 && l.phase > 10 {
			l.phase--
			shortfall--
		}
		for shortfall > 0 && l.rows > 7 {
			l.rows--
			shortfall--
		}
		for shortfall > 0 && l.seg > 7 {
			l.seg--
			shortfall--
		}
		for shortfall > 0 && l.mem > 7 {
			l.mem--
			shortfall--
		}
		l.query = 12
	}

	return l
}

func (l topQueryTableLayout) header() string {
	return strings.Join([]string{
		topCell("ID", l.id, false),
		topCell("STATUS", l.status, false),
		topCell("AGE", l.age, false),
		topCell("PHASE", l.phase, false),
		topCell("PROG", l.prog, true),
		topCell("ROWS", l.rows, true),
		topCell("SEGMENTS", l.seg, true),
		topCell("MEM", l.mem, true),
		topCell("SPILL", l.spill, true),
		topCell("QUERY", l.query, false),
	}, " ")
}

func (m topModel) queryRowLine(row client.TopQueryRow, selected bool, layout topQueryTableLayout) string {
	marker := " "
	if selected {
		marker = ">"
	}
	id := marker + " " + row.JobID
	phase := valueOr(row.Phase, row.Status)
	segments := fmt.Sprintf("%d/%d", row.SegmentsScanned+row.SegmentsSkipped, row.SegmentsTotal)
	if row.SegmentsTotal == 0 {
		segments = "-"
	}
	mem := formatBytes(max(row.CurrentMemoryBytes, row.PeakMemoryBytes))
	spill := formatBytes(row.SpillBytes)
	idx := strings.Join(row.Indexes, ",")
	if idx != "" {
		phase = phase + "@" + idx
	}

	status := m.statusText(row.Status)
	cells := []string{
		topCell(id, layout.id, false),
		topCell(status, layout.status, false),
		topCell(formatElapsed(time.Duration(row.ElapsedMS)*time.Millisecond), layout.age, false),
		topCell(phase, layout.phase, false),
		topCell(fmt.Sprintf("%.1f%%", row.Percent), layout.prog, true),
		topCell(formatCountHuman(row.RowsReadSoFar), layout.rows, true),
		topCell(segments, layout.seg, true),
		topCell(mem, layout.mem, true),
		topCell(spill, layout.spill, true),
		topCell(row.Query, layout.query, false),
	}
	if selected {
		return m.theme.Accent.Render(strings.Join(cells, " "))
	}
	switch strings.ToLower(row.Status) {
	case "done", "complete", "completed":
		for i := range cells {
			cells[i] = m.theme.Dim.Render(cells[i])
		}
		cells[1] = m.theme.Success.Render(topCell(status, layout.status, false))
	case "error", "failed":
		for i := range cells {
			cells[i] = m.theme.Error.Render(cells[i])
		}
	case "canceled", "cancelled":
		for i := range cells {
			cells[i] = m.theme.Warning.Render(cells[i])
		}
	default:
		cells[1] = m.statusStyle(row.Status).Render(topCell(status, layout.status, false))
	}

	return strings.Join(cells, " ")
}

func (m topModel) renderQueryDetail(row client.TopQueryRow, width int) []string {
	mode := "detail"
	if m.detailMode != "" {
		mode = m.detailMode
	}
	prefix := m.theme.Dim.Render("  ")
	queryPrefix := "query: "
	progress := fmt.Sprintf("progress: %.1f%%  phase: %s  rows: %s  segments: %d/%d dispatched:%d skipped:%d",
		row.Percent,
		valueOr(row.Phase, row.Status),
		formatCountHuman(row.RowsReadSoFar),
		row.SegmentsScanned,
		row.SegmentsTotal,
		row.SegmentsDispatched,
		row.SegmentsSkipped)
	resources := fmt.Sprintf("resources: memory current %s peak %s  spill %s/%d files  processed %s",
		formatBytes(row.CurrentMemoryBytes),
		formatBytes(row.PeakMemoryBytes),
		formatBytes(row.SpillBytes),
		row.SpillFiles,
		formatBytes(row.ProcessedBytes))
	indexes := "indexes: " + valueOr(strings.Join(row.Indexes, ", "), "n/a")
	if mode == "profile" {
		resources += "  profile: run copied command to collect full query profile"
	}
	lines := make([]string, 0, 8)
	for _, qline := range wrapQueryDetail(queryPrefix, row.Query, width-2) {
		lines = append(lines, prefix+qline)
	}
	lines = append(lines,
		prefix+fitContent(progress, width-2),
		prefix+fitContent(resources, width-2),
		prefix+fitContent(indexes, width-2),
	)
	return lines
}

func (m topModel) statusText(status string) string {
	if strings.TrimSpace(status) == "" {
		return "unknown"
	}
	return status
}

func (m topModel) statusStyle(status string) lipgloss.Style {
	switch strings.ToLower(status) {
	case "running":
		return m.theme.Accent
	case "done", "complete", "completed":
		return m.theme.Success
	case "error", "failed":
		return m.theme.Error
	case "canceled", "cancelled":
		return m.theme.Warning
	default:
		return m.theme.Dim
	}
}

func topCell(value string, width int, right bool) string {
	value = clip(value, width)
	pad := width - lipgloss.Width(value)
	if pad < 0 {
		pad = 0
	}
	if right {
		return strings.Repeat(" ", pad) + value
	}
	return value + strings.Repeat(" ", pad)
}

func (m topModel) renderHelp() string {
	lines := []string{
		"q quit  p pause  r refresh  tab focus  up/down select  enter/d detail  c copy  f profile  s sort  / filter  x cancel  ? help",
	}
	return m.theme.Dim.Render(strings.Join(lines, "\n"))
}

func (m topModel) renderPanelGrid(panels [][]string) string {
	cols := 1
	if m.width >= 128 {
		cols = 3
	} else if m.width >= 86 {
		cols = 2
	}
	gap := 2
	totalW := max(48, m.width-4)
	panelW := (totalW - gap*(cols-1)) / cols
	if panelW < 36 {
		panelW = totalW
		cols = 1
	}

	rendered := make([][]string, len(panels))
	for i, p := range panels {
		rendered[i] = resizePanel(p, panelW)
	}

	var b strings.Builder
	for i := 0; i < len(rendered); i += cols {
		row := rendered[i:min(i+cols, len(rendered))]
		maxH := 0
		for _, p := range row {
			maxH = max(maxH, len(p))
		}
		for line := 0; line < maxH; line++ {
			b.WriteString("  ")
			for col, p := range row {
				if line < len(p) {
					b.WriteString(padRight(p[line], panelW))
				} else {
					b.WriteString(strings.Repeat(" ", panelW))
				}
				if col+1 < len(row) {
					b.WriteString(strings.Repeat(" ", gap))
				}
			}
			b.WriteByte('\n')
		}
	}

	return b.String()
}

func (m topModel) panel(id int, title string, body []string) []string {
	width := max(48, m.width-4)
	if m.width >= 128 {
		width = (m.width - 8) / 3
	} else if m.width >= 86 {
		width = (m.width - 6) / 2
	}
	titleText := title
	if m.focus == id {
		titleText = "* " + title
	}
	lines := []string{zone.Mark(fmt.Sprintf("top-panel-%d", id), topPanelHeader(m.theme, titleText, width))}
	for _, item := range body {
		lines = append(lines, topPanelLine(m.theme, item, width))
	}
	lines = append(lines, topPanelFooter(m.theme, width))

	return lines
}

func (m topModel) sortedFilteredRows() []client.TopQueryRow {
	if m.snapshot == nil {
		return nil
	}
	filter := strings.ToLower(strings.TrimSpace(m.filter))
	rows := make([]client.TopQueryRow, 0, len(m.snapshot.Queries.Rows))
	for _, row := range m.snapshot.Queries.Rows {
		if filter == "" ||
			strings.Contains(strings.ToLower(row.JobID), filter) ||
			strings.Contains(strings.ToLower(row.Query), filter) ||
			strings.Contains(strings.ToLower(row.Status), filter) ||
			strings.Contains(strings.ToLower(row.Phase), filter) {
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch m.sortMode {
		case topSortProgress:
			if a.Percent != b.Percent {
				return a.Percent < b.Percent
			}
		case topSortMemory:
			am := max(a.CurrentMemoryBytes, a.PeakMemoryBytes)
			bm := max(b.CurrentMemoryBytes, b.PeakMemoryBytes)
			if am != bm {
				return am > bm
			}
		case topSortSpill:
			if a.SpillBytes != b.SpillBytes {
				return a.SpillBytes > b.SpillBytes
			}
		default:
			if a.ElapsedMS != b.ElapsedMS {
				return a.ElapsedMS > b.ElapsedMS
			}
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})

	return rows
}

func (m topModel) selectedRow() (client.TopQueryRow, bool) {
	rows := m.sortedFilteredRows()
	if m.selected < 0 || m.selected >= len(rows) {
		return client.TopQueryRow{}, false
	}

	return rows[m.selected], true
}

func (m *topModel) clampSelection() {
	rows := m.sortedFilteredRows()
	if len(rows) == 0 {
		m.selected = 0
		m.scroll = 0
		return
	}
	if m.selected >= len(rows) {
		m.selected = len(rows) - 1
	}
	if m.selected < 0 {
		m.selected = 0
	}
	if m.scroll > m.selected {
		m.scroll = m.selected
	}
	visible := m.visibleQueryRows()
	if visible > 0 && m.selected >= m.scroll+visible {
		m.scroll = m.selected - visible + 1
	}
}

func (m topModel) visibleQueryRows() int {
	used := 9
	if m.width >= 128 {
		used = 16
	} else if m.width >= 86 {
		used = 22
	}
	return max(3, m.height-used)
}

func (m topModel) activeFilterText() string {
	if m.filtering {
		return "/" + m.filterInput
	}
	return m.filter
}

func (m *topModel) updateHistories() {
	if m.snapshot == nil {
		return
	}
	used, limit := memoryUsedLimit(m.snapshot)
	memRatio := 0.0
	if limit > 0 {
		memRatio = float64(used) / float64(limit) * 100
	} else {
		memRatio = float64(used)
	}
	m.pushHistory("ingest", m.snapshot.Events.IngestRateEPS)
	m.pushHistory("queries", float64(m.snapshot.Queries.Active))
	m.pushHistory("cache", m.snapshot.Queries.CacheHitRate*100)
	m.pushHistory("memory", memRatio)
	m.pushHistory("spill", float64(m.snapshot.Memory.SpillBytes))
}

func (m *topModel) pushHistory(name string, v float64) {
	h := append(m.histories[name], v)
	if len(h) > 80 {
		h = h[len(h)-80:]
	}
	m.histories[name] = h
}

func fetchTopSnapshotCmd(c *client.Client) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		snap, err := c.TopSnapshot(ctx)
		return topFetchedMsg{snapshot: snap, err: err}
	}
}

func cancelTopJobCmd(c *client.Client, jobID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		err := c.CancelJob(ctx, jobID)
		return topCancelMsg{jobID: jobID, err: err}
	}
}

func copyTopTextCmd(action, text string) tea.Cmd {
	return func() tea.Msg {
		err := writeToClipboard(text)
		return topClipboardMsg{action: action, err: err}
	}
}

func runTop(interval time.Duration) error {
	api := apiClient()
	if err := shell.RunServerPreflight(api, globalServer, "LynxDB top"); err != nil {
		if shell.IsPreflightQuit(err) {
			return nil
		}

		return err
	}

	zone.NewGlobal()
	defer zone.Close()

	m := newTopModel(globalServer, interval)
	m.client = api
	p := tea.NewProgram(m)
	_, err := p.Run()

	return err
}

func topPanelHeader(t *ui.Theme, title string, width int) string {
	prefix := "┌─ "
	suffix := " "
	lineLen := width - lipgloss.Width(prefix) - lipgloss.Width(title) - lipgloss.Width(suffix) - 1
	if lineLen < 0 {
		lineLen = 0
	}
	return t.Rule.Render(prefix) + t.Bold.Render(title) + t.Rule.Render(suffix+strings.Repeat("─", lineLen)+"┐")
}

func topPanelFooter(t *ui.Theme, width int) string {
	return t.Rule.Render("└" + strings.Repeat("─", max(0, width-2)) + "┘")
}

func topPanelLine(t *ui.Theme, content string, width int) string {
	content = fitContent(content, max(0, width-4))
	pad := width - lipgloss.Width(content) - 4
	if pad < 0 {
		pad = 0
	}
	return t.Rule.Render("│") + " " + content + strings.Repeat(" ", pad) + " " + t.Rule.Render("│")
}

func resizePanel(lines []string, width int) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = fitContent(line, width)
	}
	return out
}

func fitContent(s string, width int) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	return clip(s, width)
}

func padRight(s string, width int) string {
	pad := width - lipgloss.Width(s)
	if pad < 0 {
		return fitContent(s, width)
	}
	return s + strings.Repeat(" ", pad)
}

func kv(key, value string) string {
	return fmt.Sprintf("%-10s %s", key+":", value)
}

func profileCommandForQuery(query string) string {
	return "lynxdb query --analyze full " + shellQuote(query)
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func wrapQueryDetail(prefix, query string, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	firstWidth := width - lipgloss.Width(prefix)
	if firstWidth < 8 {
		firstWidth = width
		prefix = ""
	}
	chunks := wrapPlain(query, firstWidth)
	if len(chunks) == 0 {
		return []string{prefix}
	}
	lines := []string{prefix + chunks[0]}
	indent := strings.Repeat(" ", lipgloss.Width(prefix))
	for _, chunk := range wrapPlain(strings.Join(chunks[1:], " "), width-lipgloss.Width(indent)) {
		lines = append(lines, indent+chunk)
	}
	return lines
}

func wrapPlain(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	current := words[0]
	for _, word := range words[1:] {
		if lipgloss.Width(current)+1+lipgloss.Width(word) <= width {
			current += " " + word
			continue
		}
		lines = append(lines, current)
		current = word
	}
	lines = append(lines, current)
	return lines
}

func sparkline(values []float64, width int) string {
	if width <= 0 {
		return ""
	}
	if len(values) == 0 {
		return strings.Repeat("·", width)
	}
	if len(values) > width {
		values = values[len(values)-width:]
	}
	minV, maxV := values[0], values[0]
	for _, v := range values {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	levels := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for i := 0; i < width-len(values); i++ {
		b.WriteRune('·')
	}
	for _, v := range values {
		idx := 0
		if maxV > minV {
			idx = int(math.Round((v - minV) / (maxV - minV) * float64(len(levels)-1)))
		}
		if idx < 0 {
			idx = 0
		}
		if idx >= len(levels) {
			idx = len(levels) - 1
		}
		b.WriteRune(levels[idx])
	}

	return b.String()
}

func loadBar(value, maxValue int64, width int) string {
	if width <= 0 {
		return ""
	}
	filled := 0
	if maxValue > 0 {
		filled = int(math.Round(float64(value) / float64(maxValue) * float64(width)))
	}
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func pct(v float64) string {
	if v < 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", v*100)
}

func memoryUsedLimit(s *client.TopSnapshot) (int64, int64) {
	if s == nil || s.Memory.Governor == nil {
		return 0, 0
	}
	return s.Memory.Governor.Allocated, s.Memory.Governor.Limit
}

func memorySummary(used, limit int64) string {
	if used == 0 && limit == 0 {
		return "n/a"
	}
	if limit <= 0 {
		return formatBytes(used)
	}
	return fmt.Sprintf("%s/%s", formatBytes(used), formatBytes(limit))
}

func roleSummary(c client.TopClusterSnapshot) string {
	if c.MetaNodes == 0 && c.IngestNodes == 0 && c.QueryNodes == 0 {
		return "single-node"
	}
	return fmt.Sprintf("meta:%d ingest:%d query:%d", c.MetaNodes, c.IngestNodes, c.QueryNodes)
}

func formatRelativeOrNA(v string) string {
	if v == "" {
		return "n/a"
	}
	return formatRelativeTime(v)
}

func valueOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func clip(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return string(runes[:maxLen])
	}
	return string(runes[:maxLen-1]) + "…"
}
