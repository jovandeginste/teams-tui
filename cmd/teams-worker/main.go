package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jovandeginste/teams-tui/pkg/worker"
	"github.com/spf13/cobra"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}

	dbPath := filepath.Join(home, ".local", "share", "teams-archive", "teams.db")

	rootCmd := &cobra.Command{ //nolint:exhaustruct_v5
		Use:          "teams-worker",
		Short:        "SQLite archive worker daemon for teams-for-linux",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return worker.Run(cmd.Context(), dbPath, os.Stdin, os.Stdout)
		},
	}

	rootCmd.Flags().StringVarP(&dbPath, "db", "d", dbPath, "path to teams.db")
	rootCmd.InitDefaultCompletionCmd()

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
