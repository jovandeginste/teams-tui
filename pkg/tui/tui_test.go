package tui

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/jovandeginste/teams-tui/pkg/archive"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const (
	testAuthorID   = "8:orgid:00000000-0000-0000-0000-000000000001"
	testAuthorName = "Alice"
	testDirection  = "received"
)

func TestAuthorColor(t *testing.T) {
	otherID := "8:orgid:00000000-0000-0000-0000-000000000002"
	require.Len(t, AuthorColor(otherID), 7)
	require.NotEqual(t, AuthorColor(otherID), AuthorColor(testAuthorID))
	require.NotEqual(t, ThreadColor(1700000000001), ThreadColor(1700000000002))
}

func TestCellWidthVariationSelectors(t *testing.T) {
	require.Equal(t, 1, cellWidth(sanitizeText("❤️")))
	require.Equal(t, 1, cellWidth(sanitizeText("☝️")))
	require.Equal(t, 3, cellWidth(sanitizeText("❤️ 1")))
	require.Equal(t, 3, cellWidth(sanitizeText("☝️ 1")))
}

func TestQuotedReplyFormatting(t *testing.T) {
	lines := formatIRCMessage(archive.Message{ //nolint:exhaustruct_v5
		ID:         1700000000000,
		Direction:  testDirection,
		AuthorID:   testAuthorID,
		AuthorName: testAuthorName,
		Content:    "> Bob (2-11-2026 14:28):\n> Quoted preview line that wraps cleanly.\nActual reply line",
	}, 60)

	require.GreaterOrEqual(t, len(lines), 4)
	require.Contains(t, lines[0], "<Alice>")
	require.True(t, len(lines[1]) >= 4 && lines[1][:4] == "    ")
	require.Contains(t, lines[1], "│ ")
	require.Contains(t, lines[1], "Bob")
	require.Equal(t, "    Actual reply line", lines[len(lines)-1])
}

func TestCodeBlockFormatting(t *testing.T) {
	lines := formatIRCMessage(archive.Message{ //nolint:exhaustruct_v5
		ID:         1700000000000,
		Direction:  testDirection,
		AuthorID:   testAuthorID,
		AuthorName: testAuthorName,
		Content:    "Run `go test`:\n```bash\necho hello\n```",
	}, 60)

	require.Len(t, lines, 4)
	require.Contains(t, lines[1], "`go test`")
	require.Contains(t, lines[2], "[bash]")
	require.Contains(t, lines[3], "▎ ")
	require.Contains(t, lines[3], "echo hello")

	tid := int64(1700000000000)
	rootLines := formatIRCMessage(archive.Message{ //nolint:exhaustruct_v5
		ID:         tid,
		ThreadID:   &tid,
		Direction:  testDirection,
		AuthorID:   testAuthorID,
		AuthorName: testAuthorName,
		Content:    "thread root",
	}, 60)
	require.Contains(t, rootLines[0], "┬ ")

	replyLines := formatIRCMessage(archive.Message{ //nolint:exhaustruct_v5
		ID:         tid + 1000,
		ThreadID:   &tid,
		Direction:  testDirection,
		AuthorID:   testAuthorID,
		AuthorName: testAuthorName,
		Content:    "thread reply",
	}, 60)
	require.NotContains(t, replyLines[0], "┬ ")
	require.Contains(t, replyLines[0], "│ ")
}

func TestSearchModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "teams.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{}) //nolint:exhaustruct_v5
	require.NoError(t, err)

	require.NoError(t, db.Exec(`
		CREATE TABLE authors (id TEXT PRIMARY KEY, name TEXT);
		CREATE TABLE channels (id TEXT PRIMARY KEY, name TEXT, type TEXT);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			thread_id INTEGER,
			channel_id TEXT,
			direction TEXT,
			author_id TEXT,
			content TEXT,
			reactions TEXT,
			timestamp TIMESTAMP
		);
		INSERT INTO authors VALUES ('8:orgid:00000000-0000-0000-0000-000000000001', 'Alice');
		INSERT INTO channels VALUES ('c1', 'General', 'channel'), ('c2', 'Operations', 'channel');
		INSERT INTO messages (id, channel_id, direction, author_id, content) VALUES
			(1700000000000, 'c1', 'received', '8:orgid:00000000-0000-0000-0000-000000000001', 'first message'),
			(1700000010000, 'c1', 'received', '8:orgid:00000000-0000-0000-0000-000000000001', 'needle in general'),
			(1700000020000, 'c2', 'received', '8:orgid:00000000-0000-0000-0000-000000000001', 'incident update');
	`).Error)

	store, err := archive.Open(path)
	require.NoError(t, err)

	m := New(store)
	require.Len(t, m.channels, 2)

	// 1. Channel filter ('c')
	m.channelQuery = "Oper"
	m.refresh()
	require.Len(t, m.channels, 1)
	require.Equal(t, "Operations", m.channels[0].Name)
	m.handleKey("esc")
	require.Len(t, m.channels, 2)

	// 2. Global message search ('s') filters both channels and messages
	m.globalQuery = "needle"
	m.refresh()
	require.Len(t, m.channels, 1)
	require.Equal(t, "General", m.channels[0].Name)
	require.Len(t, m.messages, 1)
	require.Equal(t, "needle in general", m.messages[0].Content)
	m.handleKey("esc")
	require.Len(t, m.channels, 2)

	// 3. In-channel message search ('f')
	m.selectChannel(1) // General
	require.Len(t, m.messages, 2)

	m.messageQuery = "first"
	m.loadMessages()
	require.Len(t, m.messages, 1)
	require.Equal(t, "first message", m.messages[0].Content)

	// 4. Help popup ('?') overlaid over current content
	require.Contains(t, m.View(), "? for help")
	m.handleKey("?")
	require.True(t, m.showHelp)

	viewWithHelp := m.View()
	require.Contains(t, viewWithHelp, "Keyboard Shortcuts")
	require.Contains(t, viewWithHelp, "Channels")
}

func TestModelNavigationAndScroll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "teams.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{}) //nolint:exhaustruct_v5
	require.NoError(t, err)

	require.NoError(t, db.Exec(`
		CREATE TABLE authors (id TEXT PRIMARY KEY, name TEXT);
		CREATE TABLE channels (id TEXT PRIMARY KEY, name TEXT, type TEXT);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			thread_id INTEGER,
			channel_id TEXT,
			direction TEXT,
			author_id TEXT,
			content TEXT,
			reactions TEXT,
			timestamp TIMESTAMP
		);
		INSERT INTO authors VALUES ('8:orgid:00000000-0000-0000-0000-000000000001', 'Alice');
		INSERT INTO channels VALUES ('c1', 'General', 'channel'), ('c2', 'Random', 'group chat');
		INSERT INTO messages (id, channel_id, direction, author_id, content, reactions) VALUES
			(1700000000000, 'c1', 'received', '8:orgid:00000000-0000-0000-0000-000000000001', 'hello irc', '👍 1');
	`).Error)

	for i := range 20 {
		require.NoError(t, db.Exec(
			`INSERT INTO messages (id, channel_id, direction, author_id, content) VALUES (?, 'c2', 'sent', '8:orgid:00000000-0000-0000-0000-000000000001', ?)`,
			1700000060000+int64(i)*1000,
			fmt.Sprintf("msg-%02d", i),
		).Error)
	}

	store, err := archive.Open(path)
	require.NoError(t, err)

	m := New(store)
	m.height = 10
	require.Len(t, m.channels, 2)
	require.Contains(t, m.View(), "msg-19")
	require.NotContains(t, m.View(), "msg-00")

	// Focus messages and scroll to top
	m.handleKey("tab")
	require.True(t, m.focusMessages)
	m.handleKey("g")
	require.Greater(t, m.scrollOffset, 0)
	require.Contains(t, m.View(), "msg-00")

	// Switch back to channels and select next channel (resets scrollOffset)
	m.handleKey("h")
	require.False(t, m.focusMessages)
	m.handleKey("down")
	require.Equal(t, 0, m.scrollOffset)
	require.Contains(t, m.View(), "hello irc")
}
