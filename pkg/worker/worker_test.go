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

func TestWorkerCachedAttachmentFlushDoesNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "teams.db")

	pngData := base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x01})
	chanID := "19:group1@thread.v2"
	rec := RecordPayload{ //nolint:exhaustruct_v5
		ID:        1700000001000,
		ChannelID: &chanID,
		Direction: "received",
		Content:   "first with inline image",
		AttachmentTasks: []AttachmentDescriptor{{ //nolint:exhaustruct_v5
			Key:        "ams:shared-obj",
			Filename:   "shared.png",
			Extension:  "png",
			DataBase64: pngData,
		}},
	}
	msg1 := RequestMessage{Type: "flush", Records: []RecordPayload{rec}} //nolint:exhaustruct_v5
	rec.ID = 1700000002000
	msg2 := RequestMessage{Type: msg1.Type, Records: []RecordPayload{rec}} //nolint:exhaustruct_v5

	w, err := New(context.Background(), dbPath, nil)
	require.NoError(t, err)
	require.NoError(t, w.HandleFlush(context.Background(), msg1))
	w.waitForAttachmentJobs()
	require.NoError(t, w.HandleFlush(context.Background(), msg2))
	w.waitForAttachmentJobs()

	store, err := archive.Open(dbPath)
	require.NoError(t, err)

	msgs, err := store.Messages(context.Background(), archive.Filter{Limit: 10}) //nolint:exhaustruct_v5
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Len(t, msgs[0].Attachments, 1)
	require.Len(t, msgs[1].Attachments, 1)
}

func TestWorkerFormatAndAttachmentExtraction(t *testing.T) {
	html := `<p>Hello <span itemtype="http://schema.skype.com/Mention">Alice</span></p>` +
		`<pre class="language-go">fmt.Println(&quot;hi&quot;)</pre>` +
		`<img itemtype="http://schema.skype.com/AMSImage" id="0-weu-d1-abc123" src="https://eu-api.asm.skype.com/v1/objects/0-weu-d1-abc123/views/imgo" alt="image">`

	text := HTMLToText(html)
	require.Contains(t, text, "Hello Alice")
	require.Contains(t, text, "```go\nfmt.Println(\"hi\")\n```")
	require.NotContains(t, text, "0-weu-d1-abc123")

	filesJSON := `[{"fileName":"report.pdf","fileType":"pdf","objectUrl":"https://tenant.sharepoint.com/sites/Team/Shared%20Documents/report.pdf"}]`
	atts := ExtractChatSvcAttachments(html, filesJSON)
	require.Len(t, atts, 2)
	require.Equal(t, "report.pdf", atts[0].Filename)
	require.Equal(t, "pdf", atts[0].Extension)
	require.Equal(t, "ams:0-weu-d1-abc123", atts[1].Key)
	require.True(t, atts[1].IsInlineImage)

	require.Equal(t, "png", DetectBufferExtension([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}))
	require.Equal(t, "pdf", DetectBufferExtension([]byte("%PDF-1.7")))
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
