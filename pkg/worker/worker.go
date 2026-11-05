// Package worker persists Teams messages, channels, authors, and attachments into the SQLite archive.
package worker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const (
	maxScannerTokenSize = 64 * 1024 * 1024
	attachmentFileMode  = 0o400
	archiveDirMode      = 0o755
)

const schemaSQL = `
PRAGMA busy_timeout = 5000;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
CREATE TABLE IF NOT EXISTS authors (
	id TEXT PRIMARY KEY,
	name TEXT
);
CREATE TABLE IF NOT EXISTS channels (
	id TEXT PRIMARY KEY,
	name TEXT,
	team_name TEXT,
	type TEXT,
	last_sync TIMESTAMP
);
CREATE TABLE IF NOT EXISTS message_reactions (
	message_id INTEGER,
	key TEXT,
	emoji TEXT,
	count INTEGER,
	PRIMARY KEY (message_id, key)
);
CREATE TABLE IF NOT EXISTS message_mentions (
	message_id INTEGER,
	author_id TEXT,
	PRIMARY KEY (message_id, author_id),
	FOREIGN KEY (message_id) REFERENCES messages(id),
	FOREIGN KEY (author_id) REFERENCES authors(id)
);
CREATE TABLE IF NOT EXISTS message_attachments (
	message_id INTEGER,
	filename TEXT,
	checksum TEXT,
	extension TEXT,
	PRIMARY KEY (message_id, checksum),
	FOREIGN KEY (message_id) REFERENCES messages(id)
);
CREATE TABLE IF NOT EXISTS messages (
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
CREATE INDEX IF NOT EXISTS idx_messages_channel_timestamp ON messages(channel_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_thread_id ON messages(thread_id);
CREATE INDEX IF NOT EXISTS idx_messages_author_id ON messages(author_id);
CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages(timestamp);
CREATE INDEX IF NOT EXISTS idx_message_mentions_author_id ON message_mentions(author_id);
CREATE INDEX IF NOT EXISTS idx_message_attachments_checksum ON message_attachments(checksum);
CREATE INDEX IF NOT EXISTS idx_authors_name ON authors(name);
CREATE INDEX IF NOT EXISTS idx_channels_name ON channels(name);
CREATE INDEX IF NOT EXISTS idx_messages_channel_id ON messages(channel_id, id DESC);
`

type Worker struct {
	db  *gorm.DB
	dir string
	out io.Writer
}

type AuthorPayload struct {
	ID   string  `json:"id"`
	Name *string `json:"name"`
}

type ChannelPayload struct {
	ID       string  `json:"id"`
	Name     *string `json:"name"`
	TeamName *string `json:"teamName"`
	Type     *string `json:"type"`
}

type ReactionPayload struct {
	Key   string  `json:"key"`
	Emoji *string `json:"emoji"`
	Count int64   `json:"count"`
}

type MentionPayload struct {
	ID   string  `json:"id"`
	Name *string `json:"name"`
}

type AttachmentPayload struct {
	MessageID  int64  `json:"messageId"`
	Filename   string `json:"filename"`
	Checksum   string `json:"checksum"`
	Extension  string `json:"extension"`
	DataBase64 string `json:"dataBase64"`
}

type RecordPayload struct {
	ID               int64               `json:"id"`
	ThreadID         *int64              `json:"threadId"`
	ChannelID        *string             `json:"channelId"`
	ChannelName      *string             `json:"channelName"`
	TeamName         *string             `json:"teamName"`
	ChannelType      *string             `json:"channelType"`
	Direction        string              `json:"direction"`
	AuthorID         *string             `json:"authorId"`
	AuthorName       *string             `json:"authorName"`
	Content          string              `json:"content"`
	ReactionsSummary *string             `json:"reactionsSummary"`
	Reactions        []ReactionPayload   `json:"reactions"`
	Mentions         []MentionPayload    `json:"mentions"`
	Attachments      []AttachmentPayload `json:"attachments"`
}

type LastSyncPayload struct {
	ChannelID string `json:"channelId"`
	SyncTS    int64  `json:"syncTs"`
}

type UserInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type GroupNameRequest struct {
	ChannelID string    `json:"channelId"`
	Me        *UserInfo `json:"me"`
}

type RequestMessage struct {
	Type              string              `json:"type"`
	Authors           []AuthorPayload     `json:"authors"`
	Channels          []ChannelPayload    `json:"channels"`
	Records           []RecordPayload     `json:"records"`
	Attachments       []AttachmentPayload `json:"attachments"`
	LastSyncs         []LastSyncPayload   `json:"lastSyncs"`
	GroupNameRequests []GroupNameRequest  `json:"groupNameRequests"`
}

type InitAuthor struct {
	ID   string  `json:"id" gorm:"column:id"`
	Name *string `json:"name" gorm:"column:name"`
}

type InitChannel struct {
	ID         string  `json:"id" gorm:"column:id"`
	Name       *string `json:"name" gorm:"column:name"`
	TeamName   *string `json:"team_name" gorm:"column:team_name"`
	Type       *string `json:"type" gorm:"column:type"`
	LastSyncMS *int64  `json:"last_sync_ms" gorm:"column:last_sync_ms"`
	MaxID      *int64  `json:"max_id" gorm:"column:max_id"`
}

type InitAttachment struct {
	MessageID int64  `json:"message_id" gorm:"column:message_id"`
	Filename  string `json:"filename" gorm:"column:filename"`
	Checksum  string `json:"checksum" gorm:"column:checksum"`
	Extension string `json:"extension" gorm:"column:extension"`
}

type InitResponse struct {
	Type        string           `json:"type"`
	Authors     []InitAuthor     `json:"authors"`
	Channels    []InitChannel    `json:"channels"`
	Attachments []InitAttachment `json:"attachments"`
}

type GroupNameUpdate struct {
	ChannelID string `json:"channelId"`
	Name      string `json:"name"`
}

type GroupNamesResponse struct {
	Type    string            `json:"type"`
	Updates []GroupNameUpdate `json:"updates"`
}

func New(dbPath string, out io.Writer) (*Worker, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, archiveDirMode); err != nil {
		return nil, fmt.Errorf("create archive dir: %w", err)
	}

	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{ //nolint:exhaustruct_v5
		Logger: gormlogger.Discard,
	})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.Exec(schemaSQL).Error; err != nil {
		return nil, fmt.Errorf("initialize schema: %w", err)
	}

	_ = db.Exec("ALTER TABLE channels ADD COLUMN last_sync TIMESTAMP").Error
	_ = db.Exec("ALTER TABLE channels ADD COLUMN team_name TEXT").Error

	return &Worker{
		db:  db,
		dir: dir,
		out: out,
	}, nil
}

func Run(ctx context.Context, dbPath string, in io.Reader, out io.Writer) error {
	w, err := New(dbPath, out)
	if err != nil {
		return err
	}

	if err := w.SendInitSnapshot(ctx); err != nil {
		return err
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), maxScannerTokenSize)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var msg RequestMessage

		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "request-init":
			if err := w.SendInitSnapshot(ctx); err != nil {
				return err
			}
		case "flush":
			if err := w.HandleFlush(ctx, msg); err != nil {
				continue
			}
		}
	}

	return scanner.Err()
}

func (w *Worker) SendInitSnapshot(ctx context.Context) error {
	if w.out == nil {
		return nil
	}

	authors := make([]InitAuthor, 0)
	if err := w.db.WithContext(ctx).Raw("SELECT id, name FROM authors").Scan(&authors).Error; err != nil {
		return err
	}

	channels := make([]InitChannel, 0)

	err := w.db.WithContext(ctx).Raw(`
		SELECT
			c.id,
			c.name,
			c.team_name,
			c.type,
			unixepoch(c.last_sync) * 1000 AS last_sync_ms,
			(SELECT MAX(m.id) FROM messages m WHERE m.channel_id = c.id) AS max_id
		FROM channels c
	`).Scan(&channels).Error
	if err != nil {
		return err
	}

	attachments := make([]InitAttachment, 0)

	err = w.db.WithContext(ctx).Raw(`
		SELECT message_id, COALESCE(filename, '') AS filename, COALESCE(checksum, '') AS checksum, COALESCE(extension, '') AS extension
		FROM message_attachments
	`).Scan(&attachments).Error
	if err != nil {
		return err
	}

	return w.writeJSON(InitResponse{
		Type:        "init",
		Authors:     authors,
		Channels:    channels,
		Attachments: attachments,
	})
}

func (w *Worker) HandleFlush(ctx context.Context, msg RequestMessage) error {
	var updates []GroupNameUpdate

	err := w.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var txErr error

		updates, txErr = w.applyFlushTx(tx, msg)

		return txErr
	})
	if err != nil {
		return err
	}

	_ = w.db.WithContext(ctx).Exec("PRAGMA wal_checkpoint(PASSIVE)").Error

	if len(updates) > 0 && w.out != nil {
		return w.writeJSON(GroupNamesResponse{
			Type:    "group-names",
			Updates: updates,
		})
	}

	return nil
}

func (w *Worker) applyFlushTx(tx *gorm.DB, msg RequestMessage) ([]GroupNameUpdate, error) {
	if err := w.flushAuthorsAndChannels(tx, msg.Authors, msg.Channels); err != nil {
		return nil, err
	}

	for _, rec := range msg.Records {
		if err := w.flushRecord(tx, rec); err != nil {
			return nil, err
		}
	}

	for _, att := range msg.Attachments {
		if err := w.flushAttachment(tx, att); err != nil {
			return nil, err
		}
	}

	for _, ls := range msg.LastSyncs {
		if ls.ChannelID != "" && ls.SyncTS > 0 {
			if err := upsertLastSync(tx, ls.ChannelID, ls.SyncTS); err != nil {
				return nil, err
			}
		}
	}

	return w.flushGroupNames(tx, msg.GroupNameRequests)
}

func (w *Worker) flushAuthorsAndChannels(tx *gorm.DB, authors []AuthorPayload, channels []ChannelPayload) error {
	for _, a := range authors {
		if a.ID == "" {
			continue
		}

		if err := upsertAuthor(tx, a.ID, a.Name); err != nil {
			return err
		}
	}

	for _, c := range channels {
		if c.ID == "" {
			continue
		}

		if _, err := upsertChannel(tx, c.ID, c.Name, c.Type, c.TeamName); err != nil {
			return err
		}
	}

	return nil
}

func (w *Worker) flushRecord(tx *gorm.DB, rec RecordPayload) error {
	if rec.ChannelID != nil && *rec.ChannelID != "" {
		if _, err := upsertChannel(tx, *rec.ChannelID, rec.ChannelName, rec.ChannelType, rec.TeamName); err != nil {
			return err
		}
	}

	if rec.AuthorID != nil && *rec.AuthorID != "" {
		if err := upsertAuthor(tx, *rec.AuthorID, rec.AuthorName); err != nil {
			return err
		}
	}

	if err := upsertMessage(tx, rec); err != nil {
		return err
	}

	if err := replaceReactions(tx, rec.ID, rec.Reactions); err != nil {
		return err
	}

	if err := replaceMentions(tx, rec.ID, rec.Mentions); err != nil {
		return err
	}

	for _, att := range rec.Attachments {
		att.MessageID = rec.ID
		if err := w.flushAttachment(tx, att); err != nil {
			return err
		}
	}

	return nil
}

func (w *Worker) flushAttachment(tx *gorm.DB, att AttachmentPayload) error {
	if att.MessageID == 0 {
		return nil
	}

	checksum := att.Checksum
	ext := cleanExt(att.Extension)

	if att.DataBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(att.DataBase64)
		if err == nil && len(raw) > 0 {
			if savedSum, savedExt, err := w.SaveAttachment(raw, ext); err == nil {
				checksum = savedSum
				ext = savedExt
			}
		}
	}

	if checksum == "" {
		return nil
	}

	if err := upsertAttachment(tx, att.MessageID, att.Filename, checksum, ext); err != nil {
		return err
	}

	return tx.Exec("UPDATE messages SET recorded_at = CURRENT_TIMESTAMP WHERE id = ?", att.MessageID).Error
}

func (w *Worker) flushGroupNames(tx *gorm.DB, reqs []GroupNameRequest) ([]GroupNameUpdate, error) {
	var updates []GroupNameUpdate

	for _, req := range reqs {
		computed := computeGroupChatNameFromDB(tx, req.ChannelID, req.Me)
		if computed == "" {
			continue
		}

		groupType := "group chat"

		finalName, err := upsertChannel(tx, req.ChannelID, &computed, &groupType, nil)
		if err != nil {
			return nil, err
		}

		if finalName != "" {
			updates = append(updates, GroupNameUpdate{
				ChannelID: req.ChannelID,
				Name:      finalName,
			})
		}
	}

	return updates, nil
}

func (w *Worker) SaveAttachment(buf []byte, rawExt string) (string, string, error) {
	sum := sha256.Sum256(buf)
	checksum := hex.EncodeToString(sum[:])
	ext := cleanExt(rawExt)

	name := checksum
	if ext != "" {
		name += "." + ext
	}

	destPath := filepath.Join(w.dir, "attachments", checksum[0:2], checksum[2:4], checksum[4:6], name)
	if _, err := os.Stat(destPath); err == nil {
		return checksum, ext, nil
	}

	if err := os.MkdirAll(filepath.Dir(destPath), archiveDirMode); err != nil {
		return "", "", err
	}

	tmpPath := fmt.Sprintf("%s.tmp-%d-%d", destPath, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmpPath, buf, attachmentFileMode); err != nil {
		return "", "", err
	}

	defer func() {
		_ = os.Remove(tmpPath)
	}()

	if err := os.Chmod(tmpPath, attachmentFileMode); err != nil {
		return "", "", err
	}

	if err := os.Link(tmpPath, destPath); err != nil && !os.IsExist(err) {
		return "", "", err
	}

	return checksum, ext, nil
}

func (w *Worker) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	data = append(data, '\n')
	_, err = w.out.Write(data)

	return err
}

func upsertAuthor(tx *gorm.DB, id string, name *string) error {
	return tx.Exec(`
		INSERT INTO authors (id, name) VALUES (?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = CASE
				WHEN excluded.name IS NULL OR excluded.name = '' OR excluded.name LIKE '8:%' THEN authors.name
				WHEN authors.name IS NULL OR authors.name = '' OR authors.name LIKE '8:%' THEN excluded.name
				WHEN authors.name LIKE '% %' AND excluded.name NOT LIKE '% %' THEN authors.name
				ELSE excluded.name
			END
	`, id, name).Error
}

func upsertMessage(tx *gorm.DB, rec RecordPayload) error {
	return tx.Exec(`
		INSERT INTO messages (
			id, thread_id, channel_id, direction, author_id, content, reactions, timestamp
		) VALUES (?, ?, ?, ?, ?, ?, ?, datetime(? / 1000, 'unixepoch'))
		ON CONFLICT(id) DO UPDATE SET
			thread_id = COALESCE(messages.thread_id, excluded.thread_id),
			channel_id = CASE
				WHEN excluded.channel_id LIKE '19:%' OR excluded.channel_id = '48:notes' THEN excluded.channel_id
				ELSE COALESCE(messages.channel_id, excluded.channel_id)
			END,
			direction = CASE
				WHEN excluded.direction IN ('sent', 'received') THEN excluded.direction
				ELSE COALESCE(messages.direction, excluded.direction)
			END,
			author_id = COALESCE(messages.author_id, excluded.author_id),
			content = CASE WHEN excluded.content != '' THEN excluded.content ELSE messages.content END,
			reactions = COALESCE(excluded.reactions, messages.reactions),
			recorded_at = CASE
				WHEN excluded.content != '' AND excluded.content IS NOT messages.content THEN CURRENT_TIMESTAMP
				ELSE messages.recorded_at
			END
	`, rec.ID, rec.ThreadID, rec.ChannelID, rec.Direction, rec.AuthorID, rec.Content, rec.ReactionsSummary, rec.ID).Error
}

func replaceReactions(tx *gorm.DB, messageID int64, reactions []ReactionPayload) error {
	if len(reactions) == 0 {
		return nil
	}

	if err := tx.Exec("DELETE FROM message_reactions WHERE message_id = ?", messageID).Error; err != nil {
		return err
	}

	for _, r := range reactions {
		err := tx.Exec(`
			INSERT INTO message_reactions (message_id, key, emoji, count)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(message_id, key) DO UPDATE SET
				emoji = COALESCE(excluded.emoji, message_reactions.emoji),
				count = excluded.count
		`, messageID, r.Key, r.Emoji, r.Count).Error
		if err != nil {
			return err
		}
	}

	return nil
}

func replaceMentions(tx *gorm.DB, messageID int64, mentions []MentionPayload) error {
	if err := tx.Exec("DELETE FROM message_mentions WHERE message_id = ?", messageID).Error; err != nil {
		return err
	}

	for _, m := range mentions {
		if m.ID == "" {
			continue
		}

		if err := upsertAuthor(tx, m.ID, m.Name); err != nil {
			return err
		}

		if err := tx.Exec("INSERT OR IGNORE INTO message_mentions (message_id, author_id) VALUES (?, ?)", messageID, m.ID).Error; err != nil {
			return err
		}
	}

	return nil
}

func upsertAttachment(tx *gorm.DB, messageID int64, filename, checksum, ext string) error {
	return tx.Exec(`
		INSERT INTO message_attachments (message_id, filename, checksum, extension)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(message_id, checksum) DO UPDATE SET
			filename = CASE
				WHEN excluded.filename IS NOT NULL AND excluded.filename != '' AND (message_attachments.filename IS NULL OR message_attachments.filename = '' OR message_attachments.filename LIKE 'image.%')
				THEN excluded.filename
				ELSE message_attachments.filename
			END,
			extension = COALESCE(NULLIF(excluded.extension, ''), message_attachments.extension)
	`, messageID, filename, checksum, ext).Error
}

func upsertLastSync(tx *gorm.DB, channelID string, syncTS int64) error {
	return tx.Exec(`
		UPDATE channels
		SET last_sync = datetime(? / 1000, 'unixepoch')
		WHERE id = ? AND (last_sync IS NULL OR unixepoch(last_sync) * 1000 < ?)
	`, syncTS, channelID, syncTS).Error
}

func cleanExt(raw string) string {
	trimmed := strings.ToLower(strings.TrimLeft(strings.TrimSpace(raw), "."))
	if trimmed == "" || len(trimmed) > 15 {
		return ""
	}

	for i := range len(trimmed) {
		b := trimmed[i]
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') {
			return ""
		}
	}

	return trimmed
}
