package tui

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strings"

	osc52 "github.com/aymanbagabas/go-osc52/v2"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type textPos struct {
	line int
	col  int
}

var copyClipboardFunc = copyToSystemClipboard

func normalizeRange(a, b textPos) (textPos, textPos) {
	if a.line > b.line || (a.line == b.line && a.col > b.col) {
		return b, a
	}

	return a, b
}

func (m *Model) clearSelection() {
	m.selecting = false
	m.hasSelection = false
}

func (m *Model) visibleLineBounds(total, bodyHeight int) (int, int) {
	maxOffset := max(total-bodyHeight, 0)
	offset := min(max(m.scrollOffset, 0), maxOffset)
	end := total - offset
	start := max(end-bodyHeight, 0)

	return start, end
}

func (m *Model) mouseTextPos(x, y int) textPos {
	allLines := m.renderedLines()
	start, end := m.visibleLineBounds(len(allLines), m.messageBodyHeight())
	relY := min(max(y-2, 0), max(end-start-1, 0))
	col := max(x-(m.leftPaneWidth()+1), 0)

	return textPos{
		line: start + relY,
		col:  col,
	}
}

func (m *Model) applySelectionHighlight(visible []string, startLine int) []string {
	if !m.hasSelection {
		return visible
	}

	startPos, endPos := normalizeRange(m.selStart, m.selEnd)
	if startPos == endPos {
		return visible
	}

	out := make([]string, len(visible))
	selStyle := lipgloss.NewStyle().Background(lipgloss.Color("#44475a")).Foreground(lipgloss.Color("#f8f8f2"))

	for i, line := range visible {
		lineIdx := startLine + i
		if lineIdx < startPos.line || lineIdx > endPos.line || strings.Contains(line, "\x1bP") {
			out[i] = line
			continue
		}

		width := cellWidth(line)
		cStart := 0
		cEnd := width

		if lineIdx == startPos.line {
			cStart = min(max(startPos.col, 0), width)
		}

		if lineIdx == endPos.line {
			cEnd = min(max(endPos.col+1, 0), width)
		}

		if cStart >= cEnd {
			out[i] = line
			continue
		}

		left := ansi.Cut(line, 0, cStart)
		mid := ansi.Strip(ansi.Cut(line, cStart, cEnd))
		right := ansi.Cut(line, cEnd, width)
		out[i] = left + selStyle.Render(mid) + right
	}

	return out
}

func (m *Model) selectedText() string {
	if !m.hasSelection {
		return ""
	}

	startPos, endPos := normalizeRange(m.selStart, m.selEnd)
	if startPos == endPos {
		return ""
	}

	allLines := m.renderedLines()
	if len(allLines) == 0 {
		return ""
	}

	startLine := min(max(startPos.line, 0), len(allLines)-1)
	endLine := min(max(endPos.line, 0), len(allLines)-1)
	picked := make([]string, 0, endLine-startLine+1)

	for l := startLine; l <= endLine; l++ {
		if strings.Contains(allLines[l], "\x1bP") {
			continue
		}

		plain := ansi.Strip(allLines[l])
		width := cellWidth(plain)
		cStart := 0
		cEnd := width

		if l == startPos.line {
			cStart = min(max(startPos.col, 0), width)
		}

		if l == endPos.line {
			cEnd = min(max(endPos.col+1, 0), width)
		}

		if cStart < cEnd {
			picked = append(picked, strings.TrimRight(ansi.Cut(plain, cStart, cEnd), " "))
		}
	}

	return strings.Join(picked, "\n")
}

func copyToSystemClipboard(text string) {
	if text == "" {
		return
	}

	if _, err := io.WriteString(os.Stderr, osc52.New(text).String()); err != nil {
		return
	}

	if _, err := io.WriteString(os.Stderr, osc52.New(text).Primary().String()); err != nil {
		return
	}

	for _, args := range [][]string{
		{"wl-copy"},
		{"wl-copy", "--primary"},
		{"xclip", "-selection", "clipboard"},
		{"xclip", "-selection", "primary"},
	} {
		runClipboardCmd(args, text)
	}
}

func runClipboardCmd(args []string, text string) {
	path, err := exec.LookPath(args[0])
	if err != nil {
		return
	}

	cmd := exec.CommandContext(context.Background(), path, args[1:]...)
	cmd.Stdin = strings.NewReader(text)

	if err := cmd.Start(); err != nil {
		return
	}

	go func() {
		if err := cmd.Wait(); err != nil {
			return
		}
	}()
}
