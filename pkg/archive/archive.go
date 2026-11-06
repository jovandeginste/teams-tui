// Package archive reads the SQLite archive created by the teams-for-linux message-logger script.
package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aquasecurity/table"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const (
	formatJSON  = "json"
	formatTable = "table"

	channelNameExpr = "CASE WHEN COALESCE(c.team_name, '') != '' THEN c.team_name || ' › ' || COALESCE(c.name, c.id) ELSE COALESCE(c.name, c.id) END"
)

type Store struct {
	db  *gorm.DB
	dir string
}

type Channel struct {
	ID       string `json:"id" gorm:"column:id"`
	Name     string `json:"name" gorm:"column:name"`
	Type     string `json:"type" gorm:"column:type"`
	Messages int64  `json:"messages" gorm:"column:messages"`
	Sent     int64  `json:"sent,omitempty" gorm:"column:sent"`
}

type Stats struct {
	TotalMessages int64     `json:"total_messages"`
	TotalSent     int64     `json:"total_sent"`
	Channels      []Channel `json:"channels"`
}

type StatsFilter struct {
	Channel string
	Types   []string
	BySent  bool
	Limit   int
}

type Author struct {
	ID       string `json:"id" gorm:"column:id"`
	Name     string `json:"name" gorm:"column:name"`
	Messages int64  `json:"messages" gorm:"column:messages"`
}

type Mention struct {
	ID   string `json:"id" gorm:"column:author_id"`
	Name string `json:"name" gorm:"column:author_name"`
}

type Attachment struct {
	Filename  string `json:"filename" gorm:"column:filename"`
	Checksum  string `json:"checksum" gorm:"column:checksum"`
	Extension string `json:"extension" gorm:"column:extension"`
	Size      int64  `json:"size,omitempty" gorm:"column:size"`
}

func (a Attachment) RelativePath() string {
	if len(a.Checksum) < 6 {
		return ""
	}

	name := a.Checksum
	if ext := strings.TrimPrefix(a.Extension, "."); ext != "" {
		name += "." + ext
	}

	return filepath.Join("attachments", a.Checksum[0:2], a.Checksum[2:4], a.Checksum[4:6], name)
}

func (a Attachment) Path(archiveDir string) string {
	rel := a.RelativePath()
	if rel == "" {
		return ""
	}

	return filepath.Join(archiveDir, rel)
}

type Message struct {
	ID          int64        `json:"id" gorm:"column:id"`
	ThreadID    *int64       `json:"thread_id,omitempty" gorm:"column:thread_id"`
	ChannelID   string       `json:"channel_id,omitempty" gorm:"column:channel_id"`
	ChannelName string       `json:"channel,omitempty" gorm:"column:channel_name"`
	ChannelType string       `json:"channel_type,omitempty" gorm:"column:channel_type"`
	Direction   string       `json:"direction,omitempty" gorm:"column:direction"`
	AuthorID    string       `json:"author_id,omitempty" gorm:"column:author_id"`
	AuthorName  string       `json:"author,omitempty" gorm:"column:author_name"`
	Content     string       `json:"content" gorm:"column:content"`
	Reactions   string       `json:"reactions,omitempty" gorm:"column:reactions"`
	Mentions    []Mention    `json:"mentions,omitempty" gorm:"-"`
	Attachments []Attachment `json:"attachments,omitempty" gorm:"-"`
	Timestamp   time.Time    `json:"timestamp" gorm:"-"`
}

type Filter struct {
	ChannelID string
	Channel   string
	Author    string
	Query     string
	Direction string
	From      time.Time
	To        time.Time
	Limit     int
}

func Open(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("stat archive database: %w", err)
	}

	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}).String() //nolint:exhaustruct_v5

	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{ //nolint:exhaustruct_v5
		Logger: gormlogger.Discard,
	})
	if err != nil {
		return nil, fmt.Errorf("open archive database: %w", err)
	}

	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxIdleConns(0)
	}

	return &Store{db: db, dir: filepath.Dir(path)}, nil
}

func (s *Store) Dir() string {
	if s == nil {
		return ""
	}

	return s.dir
}

func (s *Store) AttachmentPath(a Attachment) string {
	if s == nil || s.dir == "" {
		return a.RelativePath()
	}

	return a.Path(s.dir)
}

func (s *Store) LastChange(ctx context.Context) (string, error) {
	var token string

	err := s.db.WithContext(ctx).Raw(`
		SELECT COALESCE(MAX(last_sync), '') || ':' || COUNT(*) || ':' || COALESCE(SUM(unixepoch(last_sync)), 0) || ':' || COALESCE((SELECT MAX(id) || ':' || COALESCE(MAX(recorded_at), '') FROM messages), '0:')
		FROM channels
	`).Scan(&token).Error

	return token, err
}

func (s *Store) Channels(ctx context.Context) ([]Channel, error) {
	return s.ChannelsFiltered(ctx, "", "")
}

func (s *Store) ChannelsFiltered(ctx context.Context, nameQuery, messageQuery string) ([]Channel, error) {
	var out []Channel

	q := s.db.WithContext(ctx).
		Table("channels c").
		Select("c.id, " + channelNameExpr + " AS name, COALESCE(c.type, '') AS type, COUNT(m.id) AS messages")

	if messageQuery != "" {
		q = q.Joins("JOIN messages m ON m.channel_id = c.id AND m.content LIKE ?", "%"+messageQuery+"%")
	} else {
		q = q.Joins("LEFT JOIN messages m ON m.channel_id = c.id")
	}

	if nameQuery != "" {
		like := "%" + nameQuery + "%"
		q = q.Where(channelNameExpr+" LIKE ? OR c.id LIKE ?", like, like)
	}

	err := q.Group("c.id").Order("MAX(m.id) DESC, name ASC").Scan(&out).Error

	return out, err
}

func (s *Store) Authors(ctx context.Context) ([]Author, error) {
	var out []Author

	err := s.db.WithContext(ctx).Raw(`
		SELECT a.id, COALESCE(a.name, a.id) AS name, COUNT(m.id) AS messages
		FROM authors a
		LEFT JOIN messages m ON m.author_id = a.id
		GROUP BY a.id
		ORDER BY messages DESC, name ASC
	`).Scan(&out).Error

	return out, err
}

func (s *Store) Stats(ctx context.Context, f StatsFilter) (Stats, error) {
	var totals struct {
		Messages int64 `gorm:"column:messages"`
		Sent     int64 `gorm:"column:sent"`
	}

	tq := f.applyChannelFilters(s.db.WithContext(ctx).
		Table("channels c").
		Select(`
			COUNT(m.id) AS messages,
			COALESCE(SUM(CASE WHEN m.direction = 'sent' THEN 1 ELSE 0 END), 0) AS sent
		`).
		Joins("JOIN messages m ON m.channel_id = c.id"))

	if err := tq.Scan(&totals).Error; err != nil {
		return Stats{}, err
	}

	channels, err := s.topChannels(ctx, f)
	if err != nil {
		return Stats{}, err
	}

	return Stats{
		TotalMessages: totals.Messages,
		TotalSent:     totals.Sent,
		Channels:      channels,
	}, nil
}

func (s *Store) topChannels(ctx context.Context, f StatsFilter) ([]Channel, error) {
	var out []Channel

	q := f.applyChannelFilters(s.db.WithContext(ctx).
		Table("channels c").
		Select(`
			c.id,
			` + channelNameExpr + ` AS name,
			COALESCE(c.type, '') AS type,
			COUNT(m.id) AS messages,
			COALESCE(SUM(CASE WHEN m.direction = 'sent' THEN 1 ELSE 0 END), 0) AS sent
		`).
		Joins("JOIN messages m ON m.channel_id = c.id")).
		Group("c.id")

	if f.BySent {
		q = q.Having("sent > 0").Order("sent DESC, messages DESC, name ASC")
	} else {
		q = q.Order("messages DESC, sent DESC, name ASC")
	}

	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}

	err := q.Scan(&out).Error

	return out, err
}

func (f StatsFilter) applyChannelFilters(q *gorm.DB) *gorm.DB {
	if f.Channel != "" {
		like := "%" + f.Channel + "%"
		q = q.Where(channelNameExpr+" LIKE ? OR c.id LIKE ?", like, like)
	}

	if len(f.Types) > 0 {
		normalized := make([]string, 0, len(f.Types)*2)
		for _, t := range f.Types {
			v := strings.ToLower(strings.TrimSpace(t))
			normalized = append(normalized, v, v+" chat")
		}

		q = q.Where("LOWER(c.type) IN ?", normalized)
	}

	return q
}

func (s *Store) Messages(ctx context.Context, f Filter) ([]Message, error) {
	q := f.apply(s.db.WithContext(ctx).
		Table("messages m").
		Select(`
			m.id, m.thread_id,
			COALESCE(m.channel_id, '') AS channel_id,
			CASE WHEN COALESCE(c.team_name, '') != '' THEN c.team_name || ' › ' || COALESCE(c.name, m.channel_id, '') ELSE COALESCE(c.name, m.channel_id, '') END AS channel_name,
			COALESCE(c.type, '') AS channel_type,
			COALESCE(m.direction, '') AS direction,
			COALESCE(m.author_id, '') AS author_id,
			COALESCE(a.name, m.author_id, '') AS author_name,
			COALESCE(m.content, '') AS content,
			COALESCE(m.reactions, '') AS reactions
		`).
		Joins("LEFT JOIN channels c ON c.id = m.channel_id").
		Joins("LEFT JOIN authors a ON a.id = m.author_id"))

	var rows []Message

	if err := q.Scan(&rows).Error; err != nil {
		return nil, err
	}

	slices.Reverse(rows)

	for i := range rows {
		rows[i].Timestamp = time.UnixMilli(rows[i].ID).Local()
	}

	s.populateMentions(ctx, rows)
	s.populateAttachments(ctx, rows)

	return rows, nil
}

func (s *Store) populateMentions(ctx context.Context, rows []Message) {
	if len(rows) == 0 {
		return
	}

	ids := make([]int64, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}

	var mentions []struct {
		MessageID int64  `gorm:"column:message_id"`
		ID        string `gorm:"column:author_id"`
		Name      string `gorm:"column:author_name"`
	}

	err := s.db.WithContext(ctx).
		Table("message_mentions mm").
		Select("mm.message_id, COALESCE(mm.author_id, '') AS author_id, COALESCE(a.name, c.name, mm.author_id, '') AS author_name").
		Joins("LEFT JOIN authors a ON a.id = mm.author_id").
		Joins("LEFT JOIN channels c ON c.id = mm.author_id").
		Where("mm.message_id IN ?", ids).
		Order("mm.rowid ASC").
		Scan(&mentions).Error
	if err != nil || len(mentions) == 0 {
		return
	}

	byMsg := make(map[int64][]Mention, len(mentions))
	for _, m := range mentions {
		byMsg[m.MessageID] = append(byMsg[m.MessageID], Mention{ID: m.ID, Name: m.Name})
	}

	for i := range rows {
		rows[i].Mentions = byMsg[rows[i].ID]
	}
}

func (s *Store) populateAttachments(ctx context.Context, rows []Message) {
	if len(rows) == 0 {
		return
	}

	ids := make([]int64, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}

	var attachments []struct {
		MessageID int64  `gorm:"column:message_id"`
		Filename  string `gorm:"column:filename"`
		Checksum  string `gorm:"column:checksum"`
		Extension string `gorm:"column:extension"`
		Size      int64  `gorm:"column:size"`
	}

	err := s.db.WithContext(ctx).
		Table("message_attachments ma").
		Select("ma.message_id, COALESCE(ma.filename, '') AS filename, COALESCE(ma.checksum, '') AS checksum, COALESCE(ma.extension, '') AS extension, COALESCE(ma.size, 0) AS size").
		Where("ma.message_id IN ?", ids).
		Order("ma.rowid ASC").
		Scan(&attachments).Error
	if err != nil {
		err = s.db.WithContext(ctx).
			Table("message_attachments ma").
			Select("ma.message_id, COALESCE(ma.filename, '') AS filename, COALESCE(ma.checksum, '') AS checksum, COALESCE(ma.extension, '') AS extension").
			Where("ma.message_id IN ?", ids).
			Order("ma.rowid ASC").
			Scan(&attachments).Error
	}

	if err != nil || len(attachments) == 0 {
		return
	}

	byMsg := make(map[int64][]Attachment, len(attachments))
	for _, a := range attachments {
		att := Attachment{
			Filename:  a.Filename,
			Checksum:  a.Checksum,
			Extension: a.Extension,
			Size:      a.Size,
		}
		if att.Size <= 0 && s.dir != "" {
			if info, statErr := os.Stat(att.Path(s.dir)); statErr == nil {
				att.Size = info.Size()
			}
		}

		byMsg[a.MessageID] = append(byMsg[a.MessageID], att)
	}

	for i := range rows {
		rows[i].Attachments = byMsg[rows[i].ID]
	}
}

func (f Filter) apply(q *gorm.DB) *gorm.DB {
	if f.ChannelID != "" {
		q = q.Where("m.channel_id = ?", f.ChannelID)
	}

	if f.Channel != "" {
		like := "%" + f.Channel + "%"
		q = q.Where(channelNameExpr+" LIKE ? OR m.channel_id LIKE ?", like, like)
	}

	if f.Author != "" {
		like := "%" + f.Author + "%"
		q = q.Where("a.name LIKE ? OR m.author_id LIKE ?", like, like)
	}

	if f.Query != "" {
		q = q.Where("m.content LIKE ?", "%"+f.Query+"%")
	}

	if f.Direction != "" {
		q = q.Where("m.direction = ?", f.Direction)
	}

	if !f.From.IsZero() {
		q = q.Where("m.id >= ?", f.From.UnixMilli())
	}

	if !f.To.IsZero() {
		q = q.Where("m.id <= ?", f.To.UnixMilli())
	}

	q = q.Order("m.id DESC")
	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}

	return q
}

func ParseDate(value string, endOfDay bool) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}

	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}

	t, err := time.ParseInLocation("2006-01-02", value, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q (use YYYY-MM-DD or RFC3339)", value)
	}

	if endOfDay {
		return t.Add(24*time.Hour - time.Millisecond), nil
	}

	return t, nil
}

func WriteMessages(w io.Writer, msgs []Message, format string) error {
	switch format {
	case formatJSON:
		return json.NewEncoder(w).Encode(msgs)
	case formatTable:
		t := table.New(w)
		t.SetHeaders("Time", "Channel", "Author", "Content", "Reactions")

		for _, m := range msgs {
			t.AddRow(
				m.Timestamp.Format("2006-01-02 15:04:05"),
				m.ChannelName,
				m.AuthorName,
				strings.TrimSpace(m.Content),
				m.Reactions,
			)
		}

		t.Render()

		return nil
	default:
		return fmt.Errorf("unsupported format %q (use table or json)", format)
	}
}

func WriteChannels(w io.Writer, channels []Channel, format string) error {
	switch format {
	case formatJSON:
		return json.NewEncoder(w).Encode(channels)
	case formatTable:
		t := table.New(w)
		t.SetHeaders("Name", "Type", "Messages", "ID")

		for _, c := range channels {
			t.AddRow(c.Name, c.Type, strconv.FormatInt(c.Messages, 10), c.ID)
		}

		t.Render()

		return nil
	default:
		return fmt.Errorf("unsupported format %q (use table or json)", format)
	}
}

func WriteAuthors(w io.Writer, authors []Author, format string) error {
	switch format {
	case formatJSON:
		return json.NewEncoder(w).Encode(authors)
	case formatTable:
		t := table.New(w)
		t.SetHeaders("Name", "Messages", "ID")

		for _, a := range authors {
			t.AddRow(a.Name, strconv.FormatInt(a.Messages, 10), a.ID)
		}

		t.Render()

		return nil
	default:
		return fmt.Errorf("unsupported format %q (use table or json)", format)
	}
}

func WriteStats(w io.Writer, stats Stats, format string) error {
	switch format {
	case formatJSON:
		return json.NewEncoder(w).Encode(stats)
	case formatTable:
		t := table.New(w)
		t.SetHeaders("Channel", "Type", "Messages", "From You")

		for _, c := range stats.Channels {
			t.AddRow(
				c.Name,
				c.Type,
				formatCountPct(c.Messages, stats.TotalMessages),
				formatCountPct(c.Sent, stats.TotalSent),
			)
		}

		t.Render()

		return nil
	default:
		return fmt.Errorf("unsupported format %q (use table or json)", format)
	}
}

func formatCountPct(value, total int64) string {
	if total <= 0 {
		return strconv.FormatInt(value, 10)
	}

	pct := (float64(value) * 100) / float64(total)

	return fmt.Sprintf("%d (%.1f%%)", value, pct)
}
