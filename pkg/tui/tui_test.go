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
}

func TestThreadAndMentionFormatting(t *testing.T) {
	tid := int64(1700000000000)
	rootLines := formatIRCMessage(archive.Message{ //nolint:exhaustruct_v5
		ID: tid, ThreadID: &tid, Direction: testDirection, AuthorID: testAuthorID, AuthorName: testAuthorName,
		Content:  "thread root line 1\nthread root line 2 with Bob  Smith",
		Mentions: []archive.Mention{{ID: "8:orgid:2", Name: "Bob Smith"}},
	}, 60)
	require.Equal(t, []string{"┌ ", "│ ", "│ "}, []string{rootLines[0][:4], rootLines[1][:4], rootLines[2][:4]})
	require.Contains(t, rootLines[2], "@Bob Smith")

	replyLines := formatIRCMessage(archive.Message{ //nolint:exhaustruct_v5
		ID: tid + 1000, ThreadID: &tid, Direction: testDirection, AuthorID: testAuthorID, AuthorName: testAuthorName,
		Content: "thread reply line 1\nthread reply line 2",
	}, 60)
	require.Equal(t, []string{"├ ", "│ ", "│ "}, []string{replyLines[0][:4], replyLines[1][:4], replyLines[2][:4]})

	endLines := formatIRCMessage(archive.Message{ //nolint:exhaustruct_v5
		ID: tid + 2000, ThreadID: &tid, Direction: testDirection, AuthorID: testAuthorID, AuthorName: testAuthorName,
		Content: "last thread reply line 1\nlast thread reply line 2",
	}, 60, threadEnd)
	require.Equal(t, []string{"├ ", "│ ", "└ "}, []string{endLines[0][:4], endLines[1][:4], endLines[2][:4]})

	endSingleLine := formatIRCMessage(archive.Message{ //nolint:exhaustruct_v5
		ID: tid + 2500, ThreadID: &tid, Direction: testDirection, AuthorID: testAuthorID, AuthorName: testAuthorName,
		Content: "short last reply",
	}, 60, threadEnd)
	require.Equal(t, "└ ", endSingleLine[0][:4])

	singleLines := formatIRCMessage(archive.Message{ //nolint:exhaustruct_v5
		ID: tid + 3000, ThreadID: &tid, Direction: testDirection, AuthorID: testAuthorID, AuthorName: testAuthorName,
		Content: "standalone thread message",
	}, 60, threadSingle)
	require.Equal(t, "• ", singleLines[0][:len("• ")])

	singleMultiLines := formatIRCMessage(archive.Message{ //nolint:exhaustruct_v5
		ID: tid + 4000, ThreadID: &tid, Direction: testDirection, AuthorID: testAuthorID, AuthorName: testAuthorName,
		Content: "standalone line 1\nstandalone line 2",
	}, 60, threadSingle)
	require.Equal(t, []string{"┌ ", "│ ", "└ "}, []string{singleMultiLines[0][:4], singleMultiLines[1][:4], singleMultiLines[2][:4]})

	tid2 := tid + 5000
	roles := computeThreadRoles([]archive.Message{
		{ID: tid, ThreadID: &tid},     //nolint:exhaustruct_v5
		{ID: tid + 1, ThreadID: &tid}, //nolint:exhaustruct_v5
		{ID: tid + 2, ThreadID: &tid}, //nolint:exhaustruct_v5
		{ID: tid2, ThreadID: &tid2},   //nolint:exhaustruct_v5
		{ID: tid2 + 1, ThreadID: nil}, //nolint:exhaustruct_v5
	})
	require.Equal(t, []threadRole{threadStart, threadMiddle, threadEnd, threadSingle, threadNone}, roles)
}

func TestSearchModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "teams.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{}) //nolint:exhaustruct_v5
	require.NoError(t, err)

	require.NoError(t, db.Exec(`
		CREATE TABLE authors (id TEXT PRIMARY KEY, name TEXT);
		CREATE TABLE channels (id TEXT PRIMARY KEY, name TEXT, team_name TEXT, type TEXT, last_sync TIMESTAMP);
		CREATE TABLE messages (id INTEGER PRIMARY KEY, thread_id INTEGER, channel_id TEXT, direction TEXT, author_id TEXT, content TEXT, reactions TEXT, timestamp TIMESTAMP, recorded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO authors VALUES ('8:orgid:00000000-0000-0000-0000-000000000001', 'Alice');
		INSERT INTO channels (id, name, team_name, type) VALUES ('c1', 'General', 'ICTS', 'channel'), ('c2', 'Operations', '', 'channel');
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
	require.Equal(t, "ICTS › General", m.channels[0].Name)
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
		CREATE TABLE channels (id TEXT PRIMARY KEY, name TEXT, team_name TEXT, type TEXT, last_sync TIMESTAMP);
		CREATE TABLE messages (id INTEGER PRIMARY KEY, thread_id INTEGER, channel_id TEXT, direction TEXT, author_id TEXT, content TEXT, reactions TEXT, timestamp TIMESTAMP, recorded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
		INSERT INTO authors VALUES ('8:orgid:00000000-0000-0000-0000-000000000001', 'Alice');
		INSERT INTO channels (id, name, team_name, type) VALUES ('c1', 'General', 'ICTS', 'channel'), ('c2', 'Random', NULL, 'group chat');
		INSERT INTO messages (id, channel_id, direction, author_id, content, reactions) VALUES
			(1700000000000, 'c1', 'received', '8:orgid:00000000-0000-0000-0000-000000000001', 'hello irc', '👍 1');
	`).Error)

	for i := range 20 {
		require.NoError(t, db.Exec(`INSERT INTO messages (id, channel_id, direction, author_id, content) VALUES (?, 'c2', 'sent', '8:orgid:00000000-0000-0000-0000-000000000001', ?)`, 1700000060000+int64(i)*1000, fmt.Sprintf("msg-%02d", i)).Error)
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

	// When nothing changed in the database, refreshCmd marks the message as unchanged.
	unchangedMsg, ok := m.refreshCmd()().(refreshMsg)
	require.True(t, ok && unchangedMsg.unchanged)

	_, _ = m.Update(unchangedMsg)

	// Start a background refresh after a DB change while on c1 (index 1); a tick while it is
	// in flight must not start a second refresh, and switching to c2 must not be reverted.
	require.NoError(t, db.Exec(`UPDATE channels SET last_sync = CURRENT_TIMESTAMP WHERE id = 'c1'`).Error)

	staleCmd := m.refreshCmd()
	seqBeforeTick := m.refreshSeq
	_, _ = m.Update(tickMsg{})
	require.Equal(t, seqBeforeTick, m.refreshSeq)
	m.handleKey("up")
	require.Equal(t, 0, m.selected)
	require.Equal(t, "c2", m.channelID)

	_, _ = m.Update(staleCmd())
	require.Zero(t, m.refreshInFlight)
	require.Equal(t, 0, m.selected)
	require.Equal(t, "c2", m.channelID)
	require.Contains(t, m.View(), "msg-19")
}

func TestOpenAttachments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "teams.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{}) //nolint:exhaustruct_v5
	require.NoError(t, err)

	sum1 := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	sum2 := "1234560123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	require.NoError(t, db.Exec(`
		CREATE TABLE authors (id TEXT PRIMARY KEY, name TEXT);
		CREATE TABLE channels (id TEXT PRIMARY KEY, name TEXT, team_name TEXT, type TEXT, last_sync TIMESTAMP);
		CREATE TABLE messages (id INTEGER PRIMARY KEY, thread_id INTEGER, channel_id TEXT, direction TEXT, author_id TEXT, content TEXT, reactions TEXT, timestamp TIMESTAMP, recorded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP);
		CREATE TABLE message_attachments (message_id INTEGER, filename TEXT, checksum TEXT, extension TEXT, PRIMARY KEY (message_id, checksum));
		INSERT INTO authors VALUES ('8:orgid:00000000-0000-0000-0000-000000000001', 'Alice');
		INSERT INTO channels (id, name, team_name, type) VALUES ('c1', 'General', 'ICTS', 'channel');
		INSERT INTO messages (id, channel_id, direction, author_id, content) VALUES
			(1700000000000, 'c1', 'received', '8:orgid:00000000-0000-0000-0000-000000000001', 'here is file 1'),
			(1700000010000, 'c1', 'received', '8:orgid:00000000-0000-0000-0000-000000000001', 'here is file 2');
		INSERT INTO message_attachments (message_id, filename, checksum, extension) VALUES
			(1700000000000, 'first.png', ?, 'png'),
			(1700000010000, 'second.pdf', ?, 'pdf');
	`, sum1, sum2).Error)

	var opened []string

	origOpen := openAttachmentFunc
	openAttachmentFunc = func(p string) error {
		opened = append(opened, p)
		return nil
	}

	t.Cleanup(func() {
		openAttachmentFunc = origOpen
	})

	store, err := archive.Open(path)
	require.NoError(t, err)

	m := New(store)
	require.Contains(t, m.View(), "[attachment: first.png]")
	require.Contains(t, m.View(), "[attachment: second.pdf]")

	// Pressing 'o' with multiple attachments opens the selector popup pre-selected on the latest attachment.
	m.handleKey("o")
	require.True(t, m.showAttachments)
	require.Contains(t, m.View(), "Open Attachment (xdg-open)")

	// Select first attachment ('k') and press 'enter'
	m.handleAttachmentKey("k")
	m.handleAttachmentKey("enter")
	require.False(t, m.showAttachments)
	require.Equal(t, []string{filepath.Join(dir, "attachments", "ab", "cd", "ef", sum1+".png")}, opened)
}

func TestWrappedHyperlinkPreservesFullURL(t *testing.T) {
	fullURL := "https://example.com/very/long/path/that/wraps/across/multiple/lines/in/the/terminal"
	lines := formatIRCMessage(archive.Message{ //nolint:exhaustruct_v5
		ID:         1700000000000,
		Direction:  testDirection,
		AuthorID:   testAuthorID,
		AuthorName: testAuthorName,
		Content:    "See " + fullURL + " for details",
	}, 45)

	require.GreaterOrEqual(t, len(lines), 3)

	matchedChunks := 0
	for _, line := range lines {
		if uri := extractHyperlinkURI(line); uri != "" {
			require.Equal(t, fullURL, uri)

			matchedChunks++
		}
	}

	require.GreaterOrEqual(t, matchedChunks, 2)
}
