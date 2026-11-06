package main

import (
	"fmt"
	"os"

	"github.com/jovandeginste/teams-tui/pkg/config"
	"github.com/jovandeginste/teams-tui/pkg/worker"
	"github.com/spf13/cobra"
)

func main() {
	var cfgFile string

	rootCmd := &cobra.Command{ //nolint:exhaustruct_v5
		Use:          "teams-worker",
		Short:        "SQLite archive worker daemon for teams-for-linux",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgFile)
			if err != nil {
				return err
			}

			return worker.Run(cmd.Context(), cfg.DBPath, os.Stdin, os.Stdout, cfg.MaxAttachmentSize)
		},
	}

	config.BindWorkerFlags(rootCmd, &cfgFile)
	rootCmd.InitDefaultCompletionCmd()

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
