package archive

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func seedTestDB(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "teams.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{}) //nolint:exhaustruct_v5
	require.NoError(t, err)

	require.NoError(t, db.Exec(`
		CREATE TABLE authors (id TEXT PRIMARY KEY, name TEXT);
		CREATE TABLE channels (id TEXT PRIMARY KEY, name TEXT, team_name TEXT, type TEXT);
		CREATE TABLE message_mentions (
			message_id INTEGER,
			author_id TEXT,
			PRIMARY KEY (message_id, author_id)
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY,
			thread_id INTEGER,
			channel_id TEXT,
			direction TEXT,
			author_id TEXT,
			content TEXT,
			reactions TEXT,
			timestamp TIMESTAMP,
			recorded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO authors VALUES ('8:orgid:1', 'Alice'), ('8:orgid:2', 'Bob');
		INSERT INTO channels VALUES
			('19:chan1@thread.tacv2', 'General', 'ICTS', 'channel'),
			('19:chat1@thread.v2', 'Standup', '', 'meeting chat');
		INSERT INTO messages (id, channel_id, direction, author_id, content, reactions) VALUES
			(1700000000000, '19:chan1@thread.tacv2', 'received', '8:orgid:1', 'hello world Bob', '👍 1'),
			(1700000060000, '19:chan1@thread.tacv2', 'sent', '8:orgid:2', 'reply here', NULL);
		INSERT INTO message_mentions (message_id, author_id) VALUES (1700000000000, '8:orgid:2');
	`).Error)

	return path
}

func TestStoreQueries(t *testing.T) {
	store, err := Open(seedTestDB(t))
	require.NoError(t, err)

	ctx := context.Background()

	channels, err := store.Channels(ctx)
	require.NoError(t, err)
	require.Len(t, channels, 2)
	require.Equal(t, "ICTS › General", channels[0].Name)
	require.Equal(t, int64(2), channels[0].Messages)
	require.Equal(t, "Standup", channels[1].Name)

	authors, err := store.Authors(ctx)
	require.NoError(t, err)
	require.Len(t, authors, 2)

	msgs, err := store.Messages(ctx, Filter{Author: "Alice", Query: "hello", Limit: 10}) //nolint:exhaustruct_v5
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "hello world Bob", msgs[0].Content)
	require.Equal(t, "👍 1", msgs[0].Reactions)
	require.Equal(t, []Mention{{ID: "8:orgid:2", Name: "Bob"}}, msgs[0].Mentions)
	require.Equal(t, time.UnixMilli(1700000000000).Local(), msgs[0].Timestamp)

	var buf bytes.Buffer

	require.NoError(t, WriteMessages(&buf, msgs, "table"))
	require.Contains(t, buf.String(), "hello world")

	buf.Reset()
	require.NoError(t, WriteMessages(&buf, msgs, "json"))
	require.Contains(t, buf.String(), `"mentions":[{"id":"8:orgid:2","name":"Bob"}]`)

	stats, err := store.Stats(ctx, StatsFilter{Types: []string{"channel"}, BySent: true, Limit: 20}) //nolint:exhaustruct_v5
	require.NoError(t, err)
	require.Len(t, stats.Channels, 1)
	require.Equal(t, int64(2), stats.Channels[0].Messages)
	require.Equal(t, int64(1), stats.Channels[0].Sent)
}
