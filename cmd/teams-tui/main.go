package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jovandeginste/teams-tui/pkg/archive"
	"github.com/jovandeginste/teams-tui/pkg/tui"
	"github.com/spf13/cobra"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}

	dbPath := filepath.Join(home, ".local", "share", "teams-archive", "teams.db")
	channel := ""

	rootCmd := &cobra.Command{ //nolint:exhaustruct_v5
		Use:          "teams-tui",
		Short:        "Read-only IRC-style TUI for the teams-for-linux SQLite archive",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			store, err := archive.Open(dbPath)
			if err != nil {
				return err
			}

			return tui.Run(store, channel)
		},
	}

	rootCmd.PersistentFlags().StringVarP(&dbPath, "db", "d", dbPath, "path to teams.db")
	rootCmd.PersistentFlags().StringVarP(&channel, "channel", "c", "", "filter by channel name")
	rootCmd.AddCommand(newMessagesCmd(&dbPath, &channel), newStatsCmd(&dbPath, &channel))
	rootCmd.InitDefaultCompletionCmd()

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newStatsCmd(dbPath, channel *string) *cobra.Command {
	var (
		filter archive.StatsFilter
		format string
	)

	cmd := &cobra.Command{ //nolint:exhaustruct_v5
		Use:   "stats",
		Short: "Show top channels by total messages or messages sent by you",
		RunE: func(cmd *cobra.Command, _ []string) error {
			filter.Channel = *channel

			store, err := archive.Open(*dbPath)
			if err != nil {
				return err
			}

			stats, err := store.Stats(cmd.Context(), filter)
			if err != nil {
				return err
			}

			return archive.WriteStats(os.Stdout, stats, format)
		},
	}

	cmd.Flags().IntVarP(&filter.Limit, "limit", "n", 20, "number of top channels to show")
	cmd.Flags().StringSliceVarP(&filter.Types, "type", "t", nil, "filter by channel type (channel, group, meeting, direct; repeatable)")
	cmd.Flags().BoolVarP(&filter.BySent, "sent", "s", false, "rank channels by messages sent by you instead of total messages")
	cmd.Flags().StringVarP(&format, "format", "o", "table", "output format: table or json")

	return cmd
}

func newMessagesCmd(dbPath, channel *string) *cobra.Command {
	var (
		from   string
		to     string
		format string
		filter archive.Filter
	)

	today := time.Now().Format("2006-01-02")

	cmd := &cobra.Command{ //nolint:exhaustruct_v5
		Use:   "messages",
		Short: "Show archived messages for a date range (defaults to today)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error

			filter.Channel = *channel

			if filter.From, err = archive.ParseDate(from, false); err != nil {
				return err
			}

			if filter.To, err = archive.ParseDate(to, true); err != nil {
				return err
			}

			store, err := archive.Open(*dbPath)
			if err != nil {
				return err
			}

			msgs, err := store.Messages(cmd.Context(), filter)
			if err != nil {
				return err
			}

			return archive.WriteMessages(os.Stdout, msgs, format)
		},
	}

	cmd.Flags().StringVar(&from, "from", today, "start date (YYYY-MM-DD or RFC3339)")
	cmd.Flags().StringVar(&to, "to", today, "end date (YYYY-MM-DD or RFC3339)")
	cmd.Flags().StringVarP(&filter.Author, "author", "a", "", "filter by author name or ID")
	cmd.Flags().StringVarP(&filter.Query, "search", "q", "", "filter by message content")
	cmd.Flags().StringVar(&filter.Direction, "direction", "", "filter by direction (sent, received, event)")
	cmd.Flags().StringVarP(&format, "format", "o", "table", "output format: table or json")

	return cmd
}
