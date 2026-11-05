package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jovandeginste/teams-tui/pkg/archive"
	"github.com/stretchr/testify/require"
)

func TestWorkerRunAndFlush(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "teams.db")

	rawFlush, err := json.Marshal(sampleFlushPayload())
	require.NoError(t, err)

	var out bytes.Buffer

	require.NoError(t, Run(context.Background(), dbPath, bytes.NewBuffer(append(rawFlush, '\n')), &out))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Len(t, lines, 2)
	require.Contains(t, lines[0], `"type":"init"`)
	require.Contains(t, lines[1], `"type":"group-names"`)
	require.Contains(t, lines[1], `"name":"Alice Smith"`)

	store, err := archive.Open(dbPath)
	require.NoError(t, err)

	msgs, err := store.Messages(context.Background(), archive.Filter{Limit: 10}) //nolint:exhaustruct_v5
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.Equal(t, "Alice Smith", msgs[0].ChannelName)
	require.Len(t, msgs[0].Attachments, 1)
	require.Equal(t, "notes.txt", msgs[0].Attachments[0].Filename)
	require.Equal(t, "txt", msgs[0].Attachments[0].Extension)

	attPath := store.AttachmentPath(msgs[0].Attachments[0])
	info, err := os.Stat(attPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o400), info.Mode().Perm())
}

func sampleFlushPayload() RequestMessage {
	chanID := "19:group1@thread.v2"
	chanType := "group chat"
	author1 := "8:orgid:00000000-0000-0000-0000-000000000001"
	author1Name := "Alice Smith"
	author2 := "8:orgid:00000000-0000-0000-0000-000000000002"
	author2Name := "Bob Jones"

	return RequestMessage{ //nolint:exhaustruct_v5
		Type: "flush",
		Authors: []AuthorPayload{
			{ID: author1, Name: &author1Name},
			{ID: author2, Name: &author2Name},
		},
		Records: []RecordPayload{{ //nolint:exhaustruct_v5
			ID:          1700000000000,
			ChannelID:   &chanID,
			ChannelType: &chanType,
			Direction:   "received",
			AuthorID:    &author1,
			AuthorName:  &author1Name,
			Content:     "Hi @Bob Jones",
			Mentions:    []MentionPayload{{ID: author2, Name: &author2Name}},
			Attachments: []AttachmentPayload{{ //nolint:exhaustruct_v5
				MessageID:  1700000000000,
				Filename:   "notes.txt",
				Extension:  "txt",
				DataBase64: base64.StdEncoding.EncodeToString([]byte("hello attachment content")),
			}},
		}},
		LastSyncs: []LastSyncPayload{{ChannelID: chanID, SyncTS: 1700000005000}},
		GroupNameRequests: []GroupNameRequest{{
			ChannelID: chanID,
			Me:        &UserInfo{ID: author2, Name: author2Name},
		}},
	}
}
