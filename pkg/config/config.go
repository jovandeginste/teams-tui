// Package config loads configuration for teams-tui and teams-worker using Cobra and Viper.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	DefaultMaxAttachmentSize = 20 * 1024 * 1024
	binaryUnit               = 1024

	KeyDB                = "db"
	KeyChannel           = "channel"
	KeyMaxAttachmentSize = "max_attachment_size"
)

type Config struct {
	DBPath            string
	Channel           string
	MaxAttachmentSize int64
}

func DefaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}

	return filepath.Join(home, ".local", "share", "teams-archive", "teams.db")
}

func BindCommonFlags(cmd *cobra.Command, cfgFile *string) {
	pflags := cmd.PersistentFlags()
	pflags.StringVarP(cfgFile, "config", "C", "", "path to config file (default: ~/.config/teams-tui/config.yaml)")
	pflags.StringP("db", "d", DefaultDBPath(), "path to teams.db")
	pflags.StringP("channel", "c", "", "filter by channel name")

	if err := viper.BindPFlags(pflags); err != nil {
		panic(err)
	}
}

func BindWorkerFlags(cmd *cobra.Command, cfgFile *string) {
	flags := cmd.Flags()
	flags.StringVarP(cfgFile, "config", "C", "", "path to config file (default: ~/.config/teams-tui/config.yaml)")
	flags.StringP("db", "d", DefaultDBPath(), "path to teams.db")
	flags.String("max-attachment-size", "20MB", "maximum attachment download size (e.g. 20MB)")

	if err := viper.BindPFlag(KeyDB, flags.Lookup("db")); err != nil {
		panic(err)
	}

	if err := viper.BindPFlag(KeyMaxAttachmentSize, flags.Lookup("max-attachment-size")); err != nil {
		panic(err)
	}
}

func Load(explicitPath string) (Config, error) {
	viper.SetDefault(KeyDB, DefaultDBPath())
	viper.SetDefault(KeyChannel, "")
	viper.SetDefault(KeyMaxAttachmentSize, "20MB")

	viper.SetEnvPrefix("TEAMS_TUI")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	if err := readConfigFile(explicitPath); err != nil {
		return Config{}, err
	}

	maxSize, err := ParseSize(viper.GetString(KeyMaxAttachmentSize))
	if err != nil {
		return Config{}, err
	}

	return Config{
		DBPath:            expandHome(viper.GetString(KeyDB)),
		Channel:           viper.GetString(KeyChannel),
		MaxAttachmentSize: maxSize,
	}, nil
}

func readConfigFile(explicitPath string) error {
	if explicitPath == "" {
		explicitPath = strings.TrimSpace(os.Getenv("TEAMS_TUI_CONFIG"))
	}

	if explicitPath != "" {
		viper.SetConfigFile(expandHome(explicitPath))

		if err := viper.ReadInConfig(); err != nil {
			return fmt.Errorf("read config %s: %w", explicitPath, err)
		}

		return nil
	}

	viper.SetConfigName("config")

	if cfgDir, err := os.UserConfigDir(); err == nil {
		viper.AddConfigPath(filepath.Join(cfgDir, "teams-tui"))
	}

	if home, err := os.UserHomeDir(); err == nil {
		viper.AddConfigPath(filepath.Join(home, ".config", "teams-tui"))
	}

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := errors.AsType[viper.ConfigFileNotFoundError](err); !ok {
			return fmt.Errorf("read config: %w", err)
		}
	}

	return nil
}

func ParseSize(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return DefaultMaxAttachmentSize, nil
	}

	upper := strings.ToUpper(s)
	mult := int64(1)

	for _, unit := range []struct {
		suffix string
		factor int64
	}{
		{"GIB", binaryUnit * binaryUnit * binaryUnit},
		{"GB", binaryUnit * binaryUnit * binaryUnit},
		{"G", binaryUnit * binaryUnit * binaryUnit},
		{"MIB", binaryUnit * binaryUnit},
		{"MB", binaryUnit * binaryUnit},
		{"M", binaryUnit * binaryUnit},
		{"KIB", binaryUnit},
		{"KB", binaryUnit},
		{"K", binaryUnit},
		{"B", 1},
	} {
		if strings.HasSuffix(upper, unit.suffix) {
			mult = unit.factor
			s = strings.TrimSpace(s[:len(s)-len(unit.suffix)])

			break
		}
	}

	val, err := strconv.ParseFloat(s, 64)
	if err != nil || val <= 0 {
		return 0, fmt.Errorf("invalid size %q", raw)
	}

	return int64(val * float64(mult)), nil
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}

	return path
}
