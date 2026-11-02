// Package tui provides a read-only IRC-style terminal interface for the Teams archive.
package tui

import (
	"context"
	"crypto/md5" //nolint:gosec
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/jovandeginste/teams-tui/pkg/archive"
)

const (
	sidebarWidth        = 30
	defaultMessageLimit = 200
	refreshEvery        = time.Second
	wheelScrollStep     = 3

	modeNone      = ""
	modeChannel   = "channel"
	modeInChannel = "messages"
	modeGlobal    = "global"
)

type tickMsg time.Time

type Model struct {
	store         *archive.Store
	channels      []archive.Channel
	messages      []archive.Message
	selected      int
	channelID     string
	focusMessages bool
	showHelp      bool
	scrollOffset  int // lines from bottom (0 = follow latest)
	messageLimit  int
	searchMode    string
	channelQuery  string
	messageQuery  string
	globalQuery   string
	width         int
	height        int
	err           error
}

func New(store *archive.Store, channelFilter ...string) *Model {
	query := ""
	if len(channelFilter) > 0 {
		query = channelFilter[0]
	}

	m := &Model{ //nolint:exhaustruct_v5
		store:        store,
		channelQuery: query,
		messageLimit: defaultMessageLimit,
		width:        100,
		height:       30,
	}
	m.refresh()

	return m
}

func Run(store *archive.Store, channelFilter ...string) error {
	p := tea.NewProgram(New(store, channelFilter...), tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()

	return err
}

func (m *Model) Init() tea.Cmd {
	return tickCmd()
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshEvery, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		return m, nil
	case tickMsg:
		m.refresh()

		return m, tickCmd()
	case tea.MouseMsg:
		m.handleMouse(msg)

		return m, nil
	case tea.KeyMsg:
		if m.showHelp {
			if msg.String() == "ctrl+c" {
				return m, tea.Quit
			}

			m.showHelp = false

			return m, nil
		}

		if m.searchMode != modeNone {
			m.handleSearchInput(msg)

			return m, nil
		}

		return m, m.handleKey(msg.String())
	}

	return m, nil
}

func (m *Model) handleSearchInput(msg tea.KeyMsg) {
	target := m.activeQueryPtr()
	if target == nil {
		m.searchMode = modeNone
		return
	}

	switch msg.Type { //nolint:exhaustive
	case tea.KeyEnter:
		m.searchMode = modeNone
	case tea.KeyEsc:
		*target = ""
		m.searchMode = modeNone
		m.scrollOffset = 0
		m.refresh()
	case tea.KeyBackspace, tea.KeyDelete:
		r := []rune(*target)
		if len(r) > 0 {
			*target = string(r[:len(r)-1])
			m.scrollOffset = 0
			m.refresh()
		}
	case tea.KeyCtrlU:
		*target = ""
		m.scrollOffset = 0
		m.refresh()
	case tea.KeyRunes, tea.KeySpace:
		*target += string(msg.Runes)
		m.scrollOffset = 0
		m.refresh()
	}
}

func (m *Model) activeQueryPtr() *string {
	switch m.searchMode {
	case modeChannel:
		return &m.channelQuery
	case modeInChannel:
		return &m.messageQuery
	case modeGlobal:
		return &m.globalQuery
	default:
		return nil
	}
}

func (m *Model) activeMessageFilter() string {
	if m.messageQuery != "" {
		return m.messageQuery
	}

	return m.globalQuery
}

func (m *Model) handleMouse(msg tea.MouseMsg) {
	inSidebar := msg.X < m.leftPaneWidth()

	switch msg.Button { //nolint:exhaustive
	case tea.MouseButtonWheelUp:
		if inSidebar {
			m.handleChannelKey("up")
		} else {
			m.scrollBy(wheelScrollStep)
		}
	case tea.MouseButtonWheelDown:
		if inSidebar {
			m.handleChannelKey("down")
		} else {
			m.scrollBy(-wheelScrollStep)
		}
	case tea.MouseButtonLeft:
		if msg.Action != tea.MouseActionPress {
			return
		}

		if inSidebar {
			if msg.Y == m.paneHeight()-1 {
				m.showHelp = !m.showHelp
				return
			}

			m.focusMessages = false
			if idx := m.channelStartIndex() + msg.Y - 1; msg.Y >= 1 && idx >= 0 && idx < len(m.channels) {
				m.selectChannel(idx)
			}
		} else {
			m.focusMessages = true
		}
	}
}

func (m *Model) handleKey(key string) tea.Cmd {
	switch key {
	case "q", "ctrl+c":
		return tea.Quit
	case "esc":
		if m.clearFilters() {
			return nil
		}

		if m.focusMessages {
			m.focusMessages = false
			return nil
		}

		return tea.Quit
	case "/":
		if m.focusMessages {
			m.searchMode = modeInChannel
		} else {
			m.searchMode = modeChannel
		}
	case "c":
		m.searchMode = modeChannel
	case "f":
		m.searchMode = modeInChannel
	case "s":
		m.searchMode = modeGlobal
	case "?":
		m.showHelp = true
	case "tab":
		m.focusMessages = !m.focusMessages
	case "right", "l", "enter":
		m.focusMessages = true
	case "left", "h":
		m.focusMessages = false
	case "pgup", "ctrl+u", "ctrl+b":
		m.scrollBy(m.pageStep())
	case "pgdown", "ctrl+d", "ctrl+f":
		m.scrollBy(-m.pageStep())
	case "K", "shift+up", "ctrl+up":
		m.scrollBy(1)
	case "J", "shift+down", "ctrl+down":
		m.scrollBy(-1)
	default:
		if m.focusMessages {
			m.handleMessageKey(key)
		} else {
			m.handleChannelKey(key)
		}
	}

	return nil
}

func (m *Model) clearFilters() bool {
	if m.messageQuery != "" {
		m.messageQuery = ""
		m.scrollOffset = 0
		m.loadMessages()

		return true
	}

	if m.channelQuery != "" || m.globalQuery != "" {
		m.channelQuery = ""
		m.globalQuery = ""
		m.scrollOffset = 0
		m.refresh()

		return true
	}

	return false
}

func (m *Model) handleChannelKey(key string) {
	switch key {
	case "up", "k":
		if m.selected > 0 {
			m.selectChannel(m.selected - 1)
		}
	case "down", "j":
		if m.selected < len(m.channels)-1 {
			m.selectChannel(m.selected + 1)
		}
	case "home", "g":
		if len(m.channels) > 0 {
			m.selectChannel(0)
		}
	case "end", "G":
		if len(m.channels) > 0 {
			m.selectChannel(len(m.channels) - 1)
		}
	}
}

func (m *Model) handleMessageKey(key string) {
	switch key {
	case "up", "k":
		m.scrollBy(1)
	case "down", "j":
		m.scrollBy(-1)
	case "home", "g":
		m.scrollOffset = max(len(m.renderedLines())-m.messageBodyHeight(), 0)
	case "end", "G":
		m.scrollOffset = 0
		m.messageLimit = defaultMessageLimit
	}
}

func (m *Model) selectChannel(index int) {
	m.selected = index
	m.channelID = m.channels[index].ID
	m.scrollOffset = 0
	m.messageLimit = defaultMessageLimit
	m.loadMessages()
}

func (m *Model) scrollBy(delta int) {
	if delta > 0 && len(m.messages) >= m.messageLimit {
		lines := m.renderedLines()
		if m.scrollOffset+delta >= max(len(lines)-m.messageBodyHeight(), 0) {
			m.messageLimit += defaultMessageLimit
			m.loadMessages()
		}
	}

	maxOffset := max(len(m.renderedLines())-m.messageBodyHeight(), 0)
	m.scrollOffset = min(max(m.scrollOffset+delta, 0), maxOffset)

	if m.scrollOffset == 0 {
		m.messageLimit = defaultMessageLimit
	}
}

func (m *Model) pageStep() int {
	return max(m.messageBodyHeight()/2, 1)
}

func (m *Model) paneHeight() int {
	if m.searchMode != modeNone {
		return max(m.height-2, 4)
	}

	return max(m.height-1, 4)
}

func (m *Model) leftPaneWidth() int {
	return min(sidebarWidth, max(m.width/4, 18))
}

func (m *Model) messageBodyHeight() int {
	return max(m.paneHeight()-2, 1)
}

func (m *Model) messageBodyWidth() int {
	rightWidth := max(m.width-m.leftPaneWidth()-3, 20)

	return max(rightWidth-3, 15)
}

func (m *Model) channelStartIndex() int {
	listHeight := max(m.paneHeight()-2, 1)
	if m.selected >= listHeight {
		return m.selected - listHeight + 1
	}

	return 0
}

func (m *Model) renderedLines() []string {
	width := m.messageBodyWidth()
	lines := make([]string, 0, len(m.messages))

	for _, msg := range m.messages {
		lines = append(lines, formatIRCMessage(msg, width)...)
	}

	return lines
}

func (m *Model) refresh() {
	ctx := context.Background()

	channels, err := m.store.ChannelsFiltered(ctx, m.channelQuery, m.globalQuery)
	if err != nil {
		m.err = err
		return
	}

	m.err = nil
	m.channels = channels

	if len(channels) == 0 {
		m.selected = 0
		m.channelID = ""
		m.messages = nil

		return
	}

	found := false

	if m.channelID != "" {
		for i, ch := range channels {
			if ch.ID == m.channelID {
				m.selected = i
				found = true

				break
			}
		}
	}

	if !found {
		m.selected = 0
	} else if m.selected >= len(channels) {
		m.selected = len(channels) - 1
	}

	m.channelID = channels[m.selected].ID
	m.loadMessages()
}

func (m *Model) loadMessages() {
	if m.channelID == "" {
		m.messages = nil
		return
	}

	msgs, err := m.store.Messages(context.Background(), archive.Filter{ //nolint:exhaustruct_v5
		ChannelID: m.channelID,
		Query:     m.activeMessageFilter(),
		Limit:     max(m.messageLimit, defaultMessageLimit),
	})
	if err != nil {
		m.err = err
		return
	}

	m.messages = msgs
}

func (m *Model) View() string {
	paneHeight := m.paneHeight()
	leftWidth := m.leftPaneWidth()
	rightWidth := max(m.width-leftWidth-3, 20)

	borderColor := "#44475a"
	if m.focusMessages {
		borderColor = "#bd93f9"
	}

	leftPane := lipgloss.NewStyle().
		Width(leftWidth).
		Height(paneHeight).
		MaxHeight(paneHeight).
		BorderStyle(lipgloss.NormalBorder()).
		BorderRight(true).
		BorderForeground(lipgloss.Color(borderColor)).
		Render(m.renderChannels(leftWidth-1, paneHeight))

	rightPane := lipgloss.NewStyle().
		Width(rightWidth).
		Height(paneHeight).
		MaxHeight(paneHeight).
		PaddingLeft(1).
		Render(m.renderMessages(rightWidth-1, paneHeight))

	mainView := lipgloss.JoinHorizontal(lipgloss.Top, leftPane, rightPane)
	if q := m.activeQueryPtr(); q != nil {
		prompt := lipgloss.NewStyle().
			Foreground(lipgloss.Color("#50fa7b")).
			Render(fmt.Sprintf("[%s search] %s█  (enter: done, esc: cancel)", m.searchMode, *q))

		mainView = lipgloss.JoinVertical(lipgloss.Left, mainView, prompt)
	}

	if m.showHelp {
		return overlayCenter(mainView, m.renderHelpBox(), m.width, m.height)
	}

	return mainView
}

func (m *Model) renderHelpBox() string {
	helpText := strings.Join([]string{
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffb86c")).Render("Keyboard Shortcuts"),
		"",
		"  ↑/↓, j/k        Navigate channels / scroll messages",
		"  tab, h/l        Switch focus between panes",
		"  J/K, pgup/pgdn  Scroll messages from either pane",
		"  home/end, g/G   Jump to top / bottom",
		"  c (or /)        Filter channels by name",
		"  f (or /)        Search messages in current channel",
		"  s               Search messages across all channels",
		"  esc             Clear active filter / unfocus messages",
		"  ?               Toggle this help popup",
		"  q, ctrl+c       Quit",
		"",
		lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Italic(true).Render("Press any key to close"),
	}, "\n")

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#bd93f9")).
		Padding(1, 3).
		Render(helpText)
}

func overlayCenter(bg, fg string, width, height int) string {
	bgLines := strings.Split(bg, "\n")
	fgLines := strings.Split(fg, "\n")

	fgWidth := lipgloss.Width(fg)
	startY := max((min(height, len(bgLines))-len(fgLines))/2, 0)
	startX := max((width-fgWidth)/2, 0)

	for i, fgLine := range fgLines {
		row := startY + i
		if row < 0 || row >= len(bgLines) {
			continue
		}

		bgLine := bgLines[row]
		w := lipgloss.Width(fgLine)
		left := ansi.Truncate(bgLine, startX, "")

		if pad := startX - lipgloss.Width(left); pad > 0 {
			left += strings.Repeat(" ", pad)
		}

		right := ansi.TruncateLeft(bgLine, startX+w, "")
		bgLines[row] = left + "\x1b[0m" + fgLine + "\x1b[0m" + right
	}

	return strings.Join(bgLines, "\n")
}

func (m *Model) renderChannels(width, height int) string {
	headerColor := "#8be9fd"
	if !m.focusMessages {
		headerColor = "#50fa7b"
	}

	title := "Channels"
	if m.channelQuery != "" {
		title += fmt.Sprintf(" [/%s]", m.channelQuery)
	}

	if m.globalQuery != "" {
		title += fmt.Sprintf(" [?%s]", m.globalQuery)
	}

	header := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(headerColor)).Render(truncate(title, width))
	hint := lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Render("? for help")
	listHeight := max(height-2, 1)

	lines := make([]string, 0, height)
	lines = append(lines, header)

	if len(m.channels) == 0 {
		lines = append(lines, "(none)")
	} else {
		start := m.channelStartIndex()
		end := min(start+listHeight, len(m.channels))

		activeStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#282a36")).Background(lipgloss.Color("#bd93f9"))
		normalStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#f8f8f2"))

		for i := start; i < end; i++ {
			name := truncate(strings.ReplaceAll(m.channels[i].Name, "\n", " "), max(width-2, 4))
			if i == m.selected {
				lines = append(lines, activeStyle.Render("> "+name))
			} else {
				lines = append(lines, normalStyle.Render("  "+name))
			}
		}
	}

	for len(lines) < height-1 {
		lines = append(lines, "")
	}

	lines = append(lines, hint)

	return strings.Join(lines, "\n")
}

func (m *Model) renderMessages(width, height int) string {
	if m.err != nil {
		return fmt.Sprintf("Error: %v", m.err)
	}

	title := "Messages"
	if m.selected >= 0 && m.selected < len(m.channels) {
		ch := m.channels[m.selected]
		title = fmt.Sprintf("# %s (%s)", ch.Name, ch.Type)
	}

	if m.scrollOffset > 0 {
		title = fmt.Sprintf("%s [+%d]", title, m.scrollOffset)
	}

	if q := m.activeMessageFilter(); q != "" {
		title = fmt.Sprintf("%s [search: %q]", title, q)
	}

	headerColor := "#ffb86c"
	if m.focusMessages {
		headerColor = "#50fa7b"
	}

	header := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(headerColor)).Render(truncate(title, width))
	bodyHeight := max(height-2, 1)
	allLines := m.renderedLines()
	total := len(allLines)

	maxOffset := max(total-bodyHeight, 0)
	offset := min(max(m.scrollOffset, 0), maxOffset)
	end := total - offset
	start := max(end-bodyHeight, 0)
	visible := allLines[start:end]

	withBar := attachScrollbar(visible, total, start, bodyHeight, max(width-2, 10))
	sep := lipgloss.NewStyle().Foreground(lipgloss.Color("#44475a")).Render(strings.Repeat("─", max(width, 1)))

	return header + "\n" + sep + "\n" + strings.Join(withBar, "\n")
}

func attachScrollbar(lines []string, total, start, height, contentWidth int) []string {
	if total <= height || height <= 0 {
		return lines
	}

	thumbSize := max((height*height)/total, 1)
	maxStart := max(total-height, 1)
	thumbTop := (start * (height - thumbSize)) / maxStart

	track := lipgloss.NewStyle().Foreground(lipgloss.Color("#44475a")).Render("│")
	thumb := lipgloss.NewStyle().Foreground(lipgloss.Color("#bd93f9")).Bold(true).Render("┃")

	out := make([]string, len(lines))

	for i, line := range lines {
		pad := max(contentWidth-cellWidth(line), 0)
		bar := track

		if i >= thumbTop && i < thumbTop+thumbSize {
			bar = thumb
		}

		out[i] = line + strings.Repeat(" ", pad) + " " + bar
	}

	return out
}

func sanitizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\t", "  ")
	// Strip U+FE0F (emoji variation selector-16): rivo/uniseg counts 1-cell BMP
	// symbols like ❤ (U+2764) and ☝ (U+261D) + U+FE0F as 2 cells while terminal
	// wcwidth advances 1 cell, shifting the right-aligned scrollbar.
	s = strings.ReplaceAll(s, "\ufe0f", "")

	return strings.TrimSpace(s)
}

func formatIRCMessage(msg archive.Message, width int) []string {
	ts, prefixWidth := renderTimestamp(msg)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Italic(true)

	content := sanitizeText(msg.Content)
	if msg.Reactions != "" {
		content += " [" + sanitizeText(msg.Reactions) + "]"
	}

	const indent = "    "

	contWidth := max(width-len(indent), 10)

	if msg.Direction == "event" || (msg.AuthorID == "" && msg.AuthorName == "") {
		return formatEventLines(ts, prefixWidth, content, dimStyle, width, indent)
	}

	nick := sanitizeText(msg.AuthorName)
	if nick == "" {
		nick = sanitizeText(msg.AuthorID)
	}

	color := AuthorColor(msg.AuthorID)
	nickPlain := "<" + nick + ">"
	nickRendered := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true).Render(nickPlain)

	if !strings.Contains(content, "\n") {
		firstWidth := max(width-(prefixWidth+cellWidth(nickPlain)+1), 10)
		chunks := wrapFirstThenRest(content, firstWidth, contWidth)
		out := make([]string, 0, len(chunks))

		for i, chunk := range chunks {
			styled := styleInlineCode(chunk)
			if i == 0 {
				out = append(out, fmt.Sprintf("%s%s %s", ts, nickRendered, styled))
			} else {
				out = append(out, indent+styled)
			}
		}

		return out
	}

	rawLines := strings.Split(content, "\n")
	out := make([]string, 0, len(rawLines)+1)
	out = append(out, fmt.Sprintf("%s%s", ts, nickRendered))

	for _, line := range formatMultilineBody(rawLines, contWidth) {
		out = append(out, indent+line)
	}

	return out
}

func renderTimestamp(msg archive.Message) (string, int) {
	tsPlain := msg.Timestamp.Format("2006-01-02 15:04")
	tsStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4"))
	threadCue := ""
	cueWidth := 0

	if msg.ThreadID != nil && *msg.ThreadID > 0 {
		tColor := lipgloss.Color(ThreadColor(*msg.ThreadID))
		tsStyle = lipgloss.NewStyle().Foreground(tColor)
		cueStyle := lipgloss.NewStyle().Foreground(tColor).Bold(true)
		cueWidth = 2

		if *msg.ThreadID == msg.ID {
			tsStyle = tsStyle.Bold(true).Underline(true)
			threadCue = cueStyle.Render("┬ ")
		} else {
			threadCue = cueStyle.Render("│ ")
		}
	}

	return tsStyle.Render(tsPlain) + " " + threadCue, len(tsPlain) + 1 + cueWidth
}

func formatEventLines(ts string, prefixWidth int, content string, dimStyle lipgloss.Style, width int, indent string) []string {
	flat := "* " + strings.Join(strings.Fields(content), " ")
	wrapped := wrapFirstThenRest(flat, max(width-prefixWidth, 10), max(width-len(indent), 10))
	out := make([]string, 0, len(wrapped))

	for i, line := range wrapped {
		if i == 0 {
			out = append(out, fmt.Sprintf("%s%s", ts, dimStyle.Render(line)))
		} else {
			out = append(out, indent+dimStyle.Render(line))
		}
	}

	return out
}

func wrapFirstThenRest(s string, firstWidth, restWidth int) []string {
	if s == "" || cellWidth(s) <= firstWidth {
		return []string{s}
	}

	var first strings.Builder

	restIdx := len(s)

	for i, r := range s {
		if r != '\ufe0f' && first.Len() > 0 && cellWidth(first.String()+string(r)) > firstWidth {
			restIdx = i
			break
		}

		first.WriteRune(r)
	}

	out := []string{first.String()}
	if restIdx < len(s) {
		out = append(out, wrapLine(s[restIdx:], restWidth)...)
	}

	return out
}

func formatMultilineBody(rawLines []string, width int) []string {
	var (
		out         []string
		inCodeBlock bool
	)

	codeBar := lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Render("▎ ")
	codeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#f1fa8c"))
	codeHeaderStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Italic(true)

	for _, line := range rawLines {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(trimmed, "```"); ok {
			lang := strings.TrimSpace(after)
			if !inCodeBlock && lang != "" {
				out = append(out, codeBar+codeHeaderStyle.Render("["+lang+"]"))
			}

			inCodeBlock = !inCodeBlock

			continue
		}

		if inCodeBlock {
			for _, part := range wrapLine(line, max(width-2, 8)) {
				out = append(out, codeBar+codeStyle.Render(part))
			}

			continue
		}

		out = append(out, formatBodyLine(line, width)...)
	}

	return out
}

func formatBodyLine(line string, width int) []string {
	trimmed := strings.TrimLeft(line, " ")
	if !strings.HasPrefix(trimmed, ">") {
		parts := wrapLine(line, width)
		for i, part := range parts {
			parts[i] = styleInlineCode(part)
		}

		return parts
	}

	quoteBody := strings.TrimPrefix(strings.TrimPrefix(trimmed, ">"), " ")
	bar := lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd")).Render("│ ")
	quoteStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#94a3b8")).Italic(true)

	if strings.HasSuffix(quoteBody, "):") || strings.HasSuffix(quoteBody, ":") {
		quoteStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#8be9fd"))
	}

	parts := wrapLine(quoteBody, max(width-2, 8))
	out := make([]string, 0, len(parts))

	for _, part := range parts {
		out = append(out, bar+quoteStyle.Render(part))
	}

	return out
}

func styleInlineCode(s string) string {
	if !strings.Contains(s, "`") {
		return s
	}

	codeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#f1fa8c"))
	parts := strings.Split(s, "`")

	if len(parts) < 3 {
		return s
	}

	var b strings.Builder

	for i, part := range parts {
		if i%2 == 1 {
			b.WriteString(codeStyle.Render("`" + part + "`"))
		} else {
			b.WriteString(part)
		}
	}

	return b.String()
}

func hashColor(value string) string {
	sum := md5.Sum([]byte(value)) //nolint:gosec

	return fmt.Sprintf("#%x", sum[:3])
}

// ThreadColor derives a hex color from the first 6 characters of the MD5 sum of the thread ID.
func ThreadColor(threadID int64) string {
	return hashColor(strconv.FormatInt(threadID, 10))
}

// AuthorColor derives a hex color from the first 6 characters of the MD5 sum of the author ID.
func AuthorColor(authorID string) string {
	return hashColor(strings.TrimSpace(authorID))
}

func cellWidth(s string) int {
	return lipgloss.Width(s)
}

func truncate(s string, maxWidth int) string {
	s = sanitizeText(s)
	if cellWidth(s) <= maxWidth {
		return s
	}

	if maxWidth <= 1 {
		return "…"
	}

	var b strings.Builder

	for _, r := range s {
		candidate := b.String() + string(r)
		if cellWidth(candidate) > maxWidth-1 {
			break
		}

		b.WriteRune(r)
	}

	b.WriteRune('…')

	return b.String()
}

func wrapLine(s string, width int) []string {
	if s == "" || cellWidth(s) <= width {
		return []string{s}
	}

	var (
		out  []string
		line strings.Builder
	)

	for _, r := range s {
		if r != '\ufe0f' && line.Len() > 0 && cellWidth(line.String()+string(r)) > width {
			out = append(out, line.String())
			line.Reset()
		}

		line.WriteRune(r)
	}

	if line.Len() > 0 {
		out = append(out, line.String())
	}

	return out
}
