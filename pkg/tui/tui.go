// Package tui provides a read-only IRC-style terminal interface for the Teams archive.
package tui

import (
	"cmp"
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
	errorDisplayTTL     = 2 * time.Second
	wheelScrollStep     = 3

	modeNone      = ""
	modeChannel   = "channel"
	modeInChannel = "messages"
	modeGlobal    = "global"
)

type threadRole int

const (
	threadNone threadRole = iota
	threadSingle
	threadStart
	threadMiddle
	threadEnd
)

type (
	tickMsg     time.Time
	clearErrMsg struct{}
	refreshMsg  struct {
		seq       uint64
		channels  []archive.Channel
		messages  []archive.Message
		selected  int
		channelID string
		err       error
	}
)

type Model struct {
	store         *archive.Store
	channels      []archive.Channel
	messages      []archive.Message
	selected      int
	channelID     string
	refreshSeq    uint64
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
		return m, tea.Batch(m.refreshCmd(), tickCmd())
	case refreshMsg:
		return m, m.applyRefresh(msg)
	case clearErrMsg:
		m.err = nil

		return m, nil
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
	roles := computeThreadRoles(m.messages)

	for i, msg := range m.messages {
		lines = append(lines, formatIRCMessage(msg, width, roles[i])...)
	}

	return lines
}

func computeThreadRoles(messages []archive.Message) []threadRole {
	roles := make([]threadRole, len(messages))
	firstIdx := make(map[int64]int)
	lastIdx := make(map[int64]int)
	counts := make(map[int64]int)

	for i, msg := range messages {
		if msg.ThreadID == nil || *msg.ThreadID <= 0 {
			continue
		}

		tid := *msg.ThreadID
		if counts[tid] == 0 {
			firstIdx[tid] = i
		}

		lastIdx[tid] = i
		counts[tid]++
	}

	for i, msg := range messages {
		if msg.ThreadID == nil || *msg.ThreadID <= 0 {
			continue
		}

		tid := *msg.ThreadID
		switch {
		case counts[tid] == 1:
			roles[i] = threadSingle
		case i == firstIdx[tid]:
			roles[i] = threadStart
		case i == lastIdx[tid]:
			roles[i] = threadEnd
		default:
			roles[i] = threadMiddle
		}
	}

	return roles
}

func (m *Model) refreshCmd() tea.Cmd {
	m.refreshSeq++
	seq := m.refreshSeq
	store := m.store
	channelQuery := m.channelQuery
	globalQuery := m.globalQuery
	msgFilter := m.activeMessageFilter()
	limit := max(m.messageLimit, defaultMessageLimit)
	curChannelID := m.channelID
	curSelected := m.selected

	return func() tea.Msg {
		ctx := context.Background()

		channels, err := store.ChannelsFiltered(ctx, channelQuery, globalQuery)
		if err != nil {
			return refreshMsg{seq: seq, err: err} //nolint:exhaustruct_v5
		}

		if len(channels) == 0 {
			return refreshMsg{seq: seq, channels: channels} //nolint:exhaustruct_v5
		}

		selected, channelID := resolveSelection(channels, curChannelID, curSelected)

		msgs, err := store.Messages(ctx, archive.Filter{ //nolint:exhaustruct_v5
			ChannelID: channelID,
			Query:     msgFilter,
			Limit:     limit,
		})
		if err != nil {
			return refreshMsg{seq: seq, err: err} //nolint:exhaustruct_v5
		}

		return refreshMsg{
			seq:       seq,
			channels:  channels,
			messages:  msgs,
			selected:  selected,
			channelID: channelID,
			err:       nil,
		}
	}
}

func (m *Model) applyRefresh(msg refreshMsg) tea.Cmd {
	if msg.seq != m.refreshSeq {
		return nil
	}

	if msg.err != nil {
		m.err = msg.err

		return tea.Tick(errorDisplayTTL, func(time.Time) tea.Msg {
			return clearErrMsg{}
		})
	}

	m.err = nil
	m.channels = msg.channels
	m.messages = msg.messages
	m.selected = msg.selected
	m.channelID = msg.channelID

	return nil
}

func resolveSelection(channels []archive.Channel, channelID string, selected int) (int, string) {
	if channelID != "" {
		for i, ch := range channels {
			if ch.ID == channelID {
				return i, ch.ID
			}
		}

		return 0, channels[0].ID
	}

	if selected < 0 || selected >= len(channels) {
		selected = 0
	}

	return selected, channels[selected].ID
}

func (m *Model) refresh() {
	if msg, ok := m.refreshCmd()().(refreshMsg); ok {
		_ = m.applyRefresh(msg)
	}
}

func (m *Model) loadMessages() {
	m.refreshSeq++

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
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#ffb86c")).Render("Channel Symbols"),
		"  # channel   & group chat   * meeting chat   @ direct chat",
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
			ch := m.channels[i]
			sym := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ChannelColor(ch.ID))).Render(channelSymbol(ch.Type))
			name := truncate(strings.ReplaceAll(ch.Name, "\n", " "), max(width-4, 4))

			if i == m.selected {
				lines = append(lines, activeStyle.Render(">")+" "+sym+" "+activeStyle.Render(name))
			} else {
				lines = append(lines, "  "+sym+" "+normalStyle.Render(name))
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
	title := "Messages"
	if m.selected >= 0 && m.selected < len(m.channels) {
		ch := m.channels[m.selected]
		title = fmt.Sprintf("%s %s (%s)", channelSymbol(ch.Type), ch.Name, ch.Type)
	}

	if m.scrollOffset > 0 {
		title = fmt.Sprintf("%s [+%d]", title, m.scrollOffset)
	}

	if q := m.activeMessageFilter(); q != "" {
		title = fmt.Sprintf("%s [search: %q]", title, q)
	}

	if m.err != nil {
		title = fmt.Sprintf("%s [sync err: %v]", title, m.err)
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

func channelSymbol(channelType string) string {
	switch channelType {
	case "channel":
		return "#"
	case "group chat":
		return "&"
	case "meeting chat":
		return "*"
	default:
		return "@"
	}
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

func formatIRCMessage(msg archive.Message, width int, roles ...threadRole) []string {
	const indent = "    "

	role := defaultThreadRole(msg)
	if len(roles) > 0 {
		role = roles[0]
	}

	ts, prefixWidth, contWidth := renderPrefix(msg, width, role)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4")).Italic(true)

	content := sanitizeText(msg.Content)
	if msg.Reactions != "" {
		content += " [" + sanitizeText(msg.Reactions) + "]"
	}

	if msg.Direction == "event" || (msg.AuthorID == "" && msg.AuthorName == "") {
		lines := formatEventLines(ts, indent, max(width-prefixWidth, 10), contWidth, content, dimStyle)

		return applyThreadGutter(lines, msg, role)
	}

	nick := sanitizeText(msg.AuthorName)
	if nick == "" {
		nick = sanitizeText(msg.AuthorID)
	}

	color := AuthorColor(msg.AuthorID)
	nickPlain := "<" + nick + ">"
	nickRendered := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true).Render(nickPlain)

	if !strings.Contains(content, "\n") {
		content = normalizeMentions(content, msg.Mentions)
		firstWidth := max(width-(prefixWidth+cellWidth(nickPlain)+1), 10)
		chunks := wrapFirstThenRest(content, firstWidth, contWidth)
		out := make([]string, 0, len(chunks))

		for i, chunk := range chunks {
			styled := styleInlineText(chunk, msg.Mentions)
			if i == 0 {
				out = append(out, fmt.Sprintf("%s%s %s", ts, nickRendered, styled))
			} else {
				out = append(out, indent+styled)
			}
		}

		return applyThreadGutter(out, msg, role)
	}

	rawLines := strings.Split(content, "\n")
	out := make([]string, 0, len(rawLines)+1)
	out = append(out, fmt.Sprintf("%s%s", ts, nickRendered))

	for _, line := range formatMultilineBody(rawLines, contWidth, msg.Mentions) {
		out = append(out, indent+line)
	}

	return applyThreadGutter(out, msg, role)
}

func defaultThreadRole(msg archive.Message) threadRole {
	if msg.ThreadID == nil || *msg.ThreadID <= 0 {
		return threadNone
	}

	if *msg.ThreadID == msg.ID {
		return threadStart
	}

	return threadMiddle
}

func renderPrefix(msg archive.Message, width int, role threadRole) (string, int, int) {
	const indent = "    "

	tsPlain := msg.Timestamp.Format("2006-01-02 15:04")
	tsStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#6272a4"))
	cueWidth := 0

	if role != threadNone && msg.ThreadID != nil && *msg.ThreadID > 0 {
		tsStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(ThreadColor(*msg.ThreadID)))
		cueWidth = 2

		if role == threadSingle || role == threadStart {
			tsStyle = tsStyle.Bold(true).Underline(true)
		}
	}

	prefixWidth := cueWidth + len(tsPlain) + 1
	contWidth := max(width-len(indent)-cueWidth, 10)

	return tsStyle.Render(tsPlain) + " ", prefixWidth, contWidth
}

func applyThreadGutter(lines []string, msg archive.Message, role threadRole) []string {
	if role == threadNone || msg.ThreadID == nil || *msg.ThreadID <= 0 {
		return lines
	}

	cueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ThreadColor(*msg.ThreadID))).Bold(true)
	pipe := cueStyle.Render("│ ")
	cornerTop := cueStyle.Render("┌ ")
	cornerBottom := cueStyle.Render("└ ")
	teeRight := cueStyle.Render("├ ")
	bullet := cueStyle.Render("• ")

	isFirstMsg := role == threadSingle || role == threadStart
	isLastMsg := role == threadSingle || role == threadEnd

	for i := range lines {
		cue := pipe

		switch {
		case isFirstMsg && isLastMsg && len(lines) == 1:
			cue = bullet
		case isFirstMsg && i == 0:
			cue = cornerTop
		case isLastMsg && i == len(lines)-1:
			cue = cornerBottom
		case i == 0:
			cue = teeRight
		}

		lines[i] = cue + lines[i]
	}

	return lines
}

func formatEventLines(ts, contPrefix string, firstWidth, contWidth int, content string, dimStyle lipgloss.Style) []string {
	flat := "* " + strings.Join(strings.Fields(content), " ")
	wrapped := wrapFirstThenRest(flat, firstWidth, contWidth)
	out := make([]string, 0, len(wrapped))

	for i, line := range wrapped {
		if i == 0 {
			out = append(out, fmt.Sprintf("%s%s", ts, dimStyle.Render(line)))
		} else {
			out = append(out, contPrefix+dimStyle.Render(line))
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

func formatMultilineBody(rawLines []string, width int, mentions []archive.Mention) []string {
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

		out = append(out, formatBodyLine(line, width, mentions)...)
	}

	return out
}

func formatBodyLine(line string, width int, mentions []archive.Mention) []string {
	trimmed := strings.TrimLeft(line, " ")
	if !strings.HasPrefix(trimmed, ">") {
		parts := wrapLine(normalizeMentions(line, mentions), width)
		for i, part := range parts {
			parts[i] = styleInlineText(part, mentions)
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

func normalizeMentions(s string, mentions []archive.Mention) string {
	for _, m := range mentions {
		words := strings.Fields(sanitizeText(m.Name))
		for i := range len(words) - 1 {
			s = strings.ReplaceAll(s, words[i]+"  "+words[i+1], words[i]+" "+words[i+1])
		}

		for _, cand := range mentionCandidates(m) {
			if idx := findMentionIndex(s, cand); idx >= 0 {
				if idx == 0 || s[idx-1] != '@' {
					s = s[:idx] + "@" + s[idx:]
				}

				break
			}
		}
	}

	return s
}

func styleInlineText(s string, mentions []archive.Mention) string {
	if !strings.Contains(s, "`") {
		return styleMentions(s, mentions)
	}

	codeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#f1fa8c"))
	parts := strings.Split(s, "`")

	if len(parts) < 3 {
		return styleMentions(s, mentions)
	}

	var b strings.Builder

	for i, part := range parts {
		if i%2 == 1 {
			b.WriteString(codeStyle.Render("`" + part + "`"))
		} else {
			b.WriteString(styleMentions(part, mentions))
		}
	}

	return b.String()
}

func styleMentions(s string, mentions []archive.Mention) string {
	for _, m := range mentions {
		color := AuthorColor(cmp.Or(m.ID, m.Name))
		for _, cand := range mentionCandidates(m) {
			if updated, ok := replaceMentionToken(s, cand, color); ok {
				s = updated
				break
			}
		}
	}

	return s
}

func mentionCandidates(m archive.Mention) []string {
	name := sanitizeText(m.Name)
	if name == "" {
		return nil
	}

	out := []string{name}
	if strings.EqualFold(name, "Iedereen") {
		out = append(out, "Everyone")
	}

	if words := strings.Fields(name); len(words) > 1 && len(words[0]) >= 2 {
		out = append(out, words[0])
	}

	return out
}

func replaceMentionToken(s, token, color string) (string, bool) {
	idx := findMentionIndex(s, token)
	if idx < 0 {
		return s, false
	}

	start := idx
	if start > 0 && s[start-1] == '@' {
		start--
	}

	end := idx + len(token)
	style := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true).Underline(true)

	return s[:start] + style.Render(s[start:end]) + s[end:], true
}

func findMentionIndex(s, token string) int {
	offset := 0

	for offset < len(s) {
		rel := strings.Index(s[offset:], token)
		if rel < 0 {
			return -1
		}

		idx := offset + rel
		if hasWordBoundary(s, idx, idx+len(token)) {
			return idx
		}

		offset = idx + len(token)
	}

	return -1
}

func hasWordBoundary(s string, start, end int) bool {
	if start > 0 && isWordByte(s[start-1]) {
		return false
	}

	if end < len(s) && isWordByte(s[end]) {
		return false
	}

	return true
}

func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
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

// ChannelColor derives a hex color from the first 6 characters of the MD5 sum of the channel ID.
func ChannelColor(channelID string) string {
	return hashColor(strings.TrimSpace(channelID))
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
