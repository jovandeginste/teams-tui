package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestParseSize(t *testing.T) {
	sz, err := ParseSize("20MB")
	require.NoError(t, err)
	require.Equal(t, int64(20*1024*1024), sz)

	sz, err = ParseSize("1.5 MiB")
	require.NoError(t, err)
	require.Equal(t, int64(1572864), sz)

	sz, err = ParseSize("4096")
	require.NoError(t, err)
	require.Equal(t, int64(4096), sz)

	_, err = ParseSize("-5MB")
	require.Error(t, err)
}

func TestLoadConfigAndCobraOverride(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	err := os.WriteFile(cfgPath, []byte("db: /tmp/custom.db\nchannel: General\nmax_attachment_size: 15MB\n"), 0o600)
	require.NoError(t, err)

	var flagCfg string

	cmd := &cobra.Command{Use: "teams-worker"} //nolint:exhaustruct_v5
	BindWorkerFlags(cmd, &flagCfg)

	cfg, err := Load(cfgPath)
	require.NoError(t, err)
	require.Equal(t, "/tmp/custom.db", cfg.DBPath)
	require.Equal(t, "General", cfg.Channel)
	require.Equal(t, int64(15*1024*1024), cfg.MaxAttachmentSize)

	require.NoError(t, cmd.Flags().Set("max-attachment-size", "30MB"))

	cfg, err = Load(cfgPath)
	require.NoError(t, err)
	require.Equal(t, int64(30*1024*1024), cfg.MaxAttachmentSize)
}
