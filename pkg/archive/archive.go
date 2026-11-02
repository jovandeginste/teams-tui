// Package archive reads the SQLite archive created by the teams-for-linux message-logger script.
package archive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
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
)

type Store struct {
	db *gorm.DB
}

type Channel struct {
	ID       string `json:"id" gorm:"column:id"`
	Name     string `json:"name" gorm:"column:name"`
	Type     string `json:"type" gorm:"column:type"`
	Messages int64  `json:"messages" gorm:"column:messages"`
}

type Author struct {
	ID       string `json:"id" gorm:"column:id"`
	Name     string `json:"name" gorm:"column:name"`
	Messages int64  `json:"messages" gorm:"column:messages"`
}

type Message struct {
	ID          int64     `json:"id" gorm:"column:id"`
	ThreadID    *int64    `json:"thread_id,omitempty" gorm:"column:thread_id"`
	ChannelID   string    `json:"channel_id,omitempty" gorm:"column:channel_id"`
	ChannelName string    `json:"channel,omitempty" gorm:"column:channel_name"`
	ChannelType string    `json:"channel_type,omitempty" gorm:"column:channel_type"`
	Direction   string    `json:"direction,omitempty" gorm:"column:direction"`
	AuthorID    string    `json:"author_id,omitempty" gorm:"column:author_id"`
	AuthorName  string    `json:"author,omitempty" gorm:"column:author_name"`
	Content     string    `json:"content" gorm:"column:content"`
	Reactions   string    `json:"reactions,omitempty" gorm:"column:reactions"`
	Timestamp   time.Time `json:"timestamp" gorm:"-"`
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
		Logger: gormlogger.New(log.New(os.Stderr, "", log.LstdFlags), gormlogger.Config{LogLevel: gormlogger.Error}), //nolint:exhaustruct_v5
	})
	if err != nil {
		return nil, fmt.Errorf("open archive database: %w", err)
	}

	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxIdleConns(0)
	}

	return &Store{db: db}, nil
}

func (s *Store) Channels(ctx context.Context) ([]Channel, error) {
	return s.ChannelsFiltered(ctx, "", "")
}

func (s *Store) ChannelsFiltered(ctx context.Context, nameQuery, messageQuery string) ([]Channel, error) {
	var out []Channel

	q := s.db.WithContext(ctx).
		Table("channels c").
		Select("c.id, COALESCE(c.name, c.id) AS name, COALESCE(c.type, '') AS type, COUNT(m.id) AS messages")

	if messageQuery != "" {
		q = q.Joins("JOIN messages m ON m.channel_id = c.id AND m.content LIKE ?", "%"+messageQuery+"%")
	} else {
		q = q.Joins("LEFT JOIN messages m ON m.channel_id = c.id")
	}

	if nameQuery != "" {
		like := "%" + nameQuery + "%"
		q = q.Where("c.name LIKE ? OR c.id LIKE ?", like, like)
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

func (s *Store) Messages(ctx context.Context, f Filter) ([]Message, error) {
	q := f.apply(s.db.WithContext(ctx).
		Table("messages m").
		Select(`
			m.id, m.thread_id,
			COALESCE(m.channel_id, '') AS channel_id,
			COALESCE(c.name, m.channel_id, '') AS channel_name,
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

	return rows, nil
}

func (f Filter) apply(q *gorm.DB) *gorm.DB {
	if f.ChannelID != "" {
		q = q.Where("m.channel_id = ?", f.ChannelID)
	}

	if f.Channel != "" {
		like := "%" + f.Channel + "%"
		q = q.Where("c.name LIKE ? OR m.channel_id LIKE ?", like, like)
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
