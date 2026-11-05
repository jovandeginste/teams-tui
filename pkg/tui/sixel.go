package tui

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"  // register GIF decoder
	_ "image/jpeg" // register JPEG decoder
	_ "image/png"  // register PNG decoder
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
	"github.com/jovandeginste/teams-tui/pkg/archive"
	"golang.org/x/sys/unix"
)

const (
	sixelMaxCols     = 44
	sixelMaxRows     = 10
	defaultCellW     = 8
	defaultCellH     = 16
	sixelBandHeight  = 6
	rgbCubeSteps     = 6
	rgbCubeMaxVal    = 5
	colorScaleMaxPct = 100
)

var (
	archiveDirMu      sync.RWMutex
	activeArchiveDir  string
	sixelRowCache     sync.Map
	cellPixelsOnce    sync.Once
	cachedCellWidthPx = defaultCellW
	cachedCellHeight  = defaultCellH
)

type sixelCacheKey struct {
	checksum string
	maxCols  int
	cellW    int
	cellH    int
}

type sixelGrid struct {
	width   int
	height  int
	indices []uint8
}

func setArchiveDir(dir string) {
	if dir == "" {
		return
	}

	archiveDirMu.Lock()

	activeArchiveDir = dir

	archiveDirMu.Unlock()
}

func currentArchiveDir() string {
	archiveDirMu.RLock()

	dir := activeArchiveDir

	archiveDirMu.RUnlock()

	if dir != "" {
		return dir
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".local", "share", "teams-archive")
}

func detectCellPixels() (int, int) {
	cellPixelsOnce.Do(func() {
		for _, fd := range []uintptr{os.Stdout.Fd(), os.Stdin.Fd(), os.Stderr.Fd()} {
			ws, err := unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ)
			if err == nil && ws.Col > 0 && ws.Row > 0 && ws.Xpixel > 0 && ws.Ypixel > 0 {
				cachedCellWidthPx = max(int(ws.Xpixel)/int(ws.Col), 4)
				cachedCellHeight = max(int(ws.Ypixel)/int(ws.Row), 8)

				return
			}
		}
	})

	return cachedCellWidthPx, cachedCellHeight
}

func isImageAttachment(att archive.Attachment) bool {
	ext := strings.ToLower(strings.TrimPrefix(att.Extension, "."))
	if ext == "" {
		ext = strings.ToLower(strings.TrimPrefix(filepath.Ext(att.Filename), "."))
	}

	switch ext {
	case "png", "jpg", "jpeg", "gif":
		return true
	default:
		return false
	}
}

func renderAttachmentSixelLines(att archive.Attachment, indent string, maxWidth int) []string {
	if !isImageAttachment(att) || len(att.Checksum) < 6 {
		return nil
	}

	dir := currentArchiveDir()
	if dir == "" {
		return nil
	}

	path := att.Path(dir)
	cellW, cellH := detectCellPixels()
	maxCols := min(max(maxWidth, 8), sixelMaxCols)

	key := sixelCacheKey{
		checksum: att.Checksum,
		maxCols:  maxCols,
		cellW:    cellW,
		cellH:    cellH,
	}

	if cached, ok := sixelRowCache.Load(key); ok {
		if rows, ok := cached.([]string); ok {
			return prefixSixelRows(rows, indent)
		}
	}

	grid, cols, rows, err := loadScaledGrid(path, maxCols, sixelMaxRows, cellW, cellH)
	if err != nil || grid == nil {
		return nil
	}

	rawRows := make([]string, 0, rows)
	spaces := strings.Repeat(" ", cols)

	for r := range rows {
		yStart := r * cellH
		yEnd := min(yStart+cellH, grid.height)

		if yStart >= yEnd {
			break
		}

		seq := encodeSixelStrip(grid, yStart, yEnd)
		line := fmt.Sprintf("%s\x1b[%dD\x1b7%s\x1b8\x1b[%dC", spaces, cols, seq, cols)
		rawRows = append(rawRows, line)
	}

	sixelRowCache.Store(key, rawRows)

	return prefixSixelRows(rawRows, indent)
}

func prefixSixelRows(rows []string, indent string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = indent + r
	}

	return out
}

func loadScaledGrid(path string, maxCols, maxRows, cellW, cellH int) (*sixelGrid, int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()

	src, _, err := image.Decode(f)
	if err != nil {
		return nil, 0, 0, err
	}

	b := src.Bounds()
	srcW := b.Dx()
	srcH := b.Dy()

	if srcW <= 0 || srcH <= 0 {
		return nil, 0, 0, nil
	}

	maxW := maxCols * cellW
	maxH := maxRows * cellH
	dstW := srcW
	dstH := srcH

	if dstW > maxW {
		dstH = max((dstH*maxW)/dstW, 1)
		dstW = maxW
	}

	if dstH > maxH {
		dstW = max((dstW*maxH)/dstH, 1)
		dstH = maxH
	}

	cols := max((dstW+cellW-1)/cellW, 1)
	rows := max((dstH+cellH-1)/cellH, 1)

	grid := &sixelGrid{
		width:   dstW,
		height:  dstH,
		indices: make([]uint8, dstW*dstH),
	}

	for y := range dstH {
		sy := b.Min.Y + (y*srcH)/dstH
		rowOff := y * dstW

		for x := range dstW {
			sx := b.Min.X + (x*srcW)/dstW
			grid.indices[rowOff+x] = quantizeColor(src, sx, sy)
		}
	}

	return grid, cols, rows, nil
}

func quantizeColor(img image.Image, x, y int) uint8 {
	r, g, b, a := img.At(x, y).RGBA()
	if a < 0x2000 {
		return 0
	}

	r6 := (r * rgbCubeMaxVal) / 0xffff
	g6 := (g * rgbCubeMaxVal) / 0xffff
	b6 := (b * rgbCubeMaxVal) / 0xffff

	return uint8(1 + r6*rgbCubeSteps*rgbCubeSteps + g6*rgbCubeSteps + b6)
}

func encodeSixelStrip(grid *sixelGrid, yStart, yEnd int) string {
	var buf bytes.Buffer

	stripH := yEnd - yStart

	fmt.Fprintf(&buf, "\"1;1;%d;%d", grid.width, stripH)
	writeUsedPalette(&buf, grid, yStart, yEnd)

	for bandY := yStart; bandY < yEnd; bandY += sixelBandHeight {
		if bandY > yStart {
			buf.WriteByte('-')
		}

		bandEnd := min(bandY+sixelBandHeight, yEnd)
		writeSixelBand(&buf, grid, bandY, bandEnd)
	}

	return ansi.SixelGraphics(0, 1, 0, buf.Bytes())
}

func writeUsedPalette(buf *bytes.Buffer, grid *sixelGrid, yStart, yEnd int) {
	var used [256]bool

	for y := yStart; y < yEnd; y++ {
		row := grid.indices[y*grid.width : (y+1)*grid.width]
		for _, c := range row {
			if c > 0 {
				used[c] = true
			}
		}
	}

	for c := 1; c <= rgbCubeSteps*rgbCubeSteps*rgbCubeSteps; c++ {
		if !used[c] {
			continue
		}

		idx := c - 1
		rPct := ((idx / (rgbCubeSteps * rgbCubeSteps)) * colorScaleMaxPct) / rgbCubeMaxVal
		gPct := (((idx / rgbCubeSteps) % rgbCubeSteps) * colorScaleMaxPct) / rgbCubeMaxVal
		bPct := ((idx % rgbCubeSteps) * colorScaleMaxPct) / rgbCubeMaxVal
		fmt.Fprintf(buf, "#%d;2;%d;%d;%d", c, rPct, gPct, bPct)
	}
}

func writeSixelBand(buf *bytes.Buffer, grid *sixelGrid, bandY, bandEnd int) {
	var colorsInBand [256]bool

	for y := bandY; y < bandEnd; y++ {
		row := grid.indices[y*grid.width : (y+1)*grid.width]
		for _, c := range row {
			if c > 0 {
				colorsInBand[c] = true
			}
		}
	}

	firstColor := true
	for c := 1; c <= rgbCubeSteps*rgbCubeSteps*rgbCubeSteps; c++ {
		if !colorsInBand[c] {
			continue
		}

		if !firstColor {
			buf.WriteByte('$')
		}

		firstColor = false

		buf.WriteByte('#')
		buf.WriteString(strconv.Itoa(c))
		writeColorBandRun(buf, grid, bandY, bandEnd, uint8(c))
	}
}

func writeColorBandRun(buf *bytes.Buffer, grid *sixelGrid, bandY, bandEnd int, colorIdx uint8) {
	runChar := byte(0)
	runLen := 0

	for x := range grid.width {
		mask := byte(0)

		for dy := range bandEnd - bandY {
			if grid.indices[(bandY+dy)*grid.width+x] == colorIdx {
				mask |= 1 << dy
			}
		}

		ch := '?' + mask
		if ch == runChar {
			runLen++
			continue
		}

		flushSixelRun(buf, runChar, runLen)

		runChar = ch
		runLen = 1
	}

	flushSixelRun(buf, runChar, runLen)
}

func flushSixelRun(buf *bytes.Buffer, ch byte, count int) {
	if count <= 0 {
		return
	}

	if count <= 3 {
		for range count {
			buf.WriteByte(ch)
		}

		return
	}

	buf.WriteByte('!')
	buf.WriteString(strconv.Itoa(count))
	buf.WriteByte(ch)
}

func stripSixel(s string) string {
	for {
		start := strings.Index(s, "\x1bP")
		if start < 0 {
			return s
		}

		endRel := strings.Index(s[start:], "\x1b\\")
		if endRel < 0 {
			return s[:start]
		}

		s = s[:start] + s[start+endRel+2:]
	}
}
