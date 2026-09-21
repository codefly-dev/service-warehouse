package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// clearEnv blanks every SWH_* variable so a test describes the whole
// environment rather than inheriting the developer's shell.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"SWH_LISTEN", "SWH_BACKEND", "SWH_DATABASE", "SWH_DATASET", "SWH_LOCATION",
		"SWH_DSN", "SWH_HOST", "SWH_PORT", "SWH_USER", "SWH_PASSWORD", "SWH_ACCOUNT",
		"SWH_CREDENTIALS_FILE", "SWH_MAX_QUERY_BYTES", "SWH_QUERY_TIMEOUT",
	} {
		t.Setenv(k, "")
	}
}

func TestLocalBackendsNeedNoDatabase(t *testing.T) {
	for _, kind := range []string{"mem", "duckdb"} {
		t.Run(kind, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("SWH_BACKEND", kind)

			cfg, err := FromEnv()
			require.NoError(t, err)
			require.Equal(t, kind, cfg.Backend.Kind)
			require.Empty(t, cfg.Backend.Database)
		})
	}
}

func TestCloudBackendRequiresDatabaseOrDSN(t *testing.T) {
	clearEnv(t)
	t.Setenv("SWH_BACKEND", "bigquery")

	_, err := FromEnv()
	require.ErrorContains(t, err, "SWH_DATABASE")
	require.ErrorContains(t, err, "bigquery")
}

func TestCloudBackendAcceptsDSNAlone(t *testing.T) {
	clearEnv(t)
	t.Setenv("SWH_BACKEND", "snowflake")
	t.Setenv("SWH_DSN", "user:pass@account/db")

	cfg, err := FromEnv()
	require.NoError(t, err)
	require.Equal(t, "user:pass@account/db", cfg.Backend.DSN)
}

func TestDefaultsAndParsing(t *testing.T) {
	clearEnv(t)
	t.Setenv("SWH_MAX_QUERY_BYTES", "4096")
	t.Setenv("SWH_QUERY_TIMEOUT", "90s")
	t.Setenv("SWH_PORT", "5439")

	cfg, err := FromEnv()
	require.NoError(t, err)
	require.Equal(t, ":9465", cfg.ListenAddr)
	require.Equal(t, "duckdb", cfg.Backend.Kind)
	require.Equal(t, int64(4096), cfg.Backend.MaxQueryBytes)
	require.Equal(t, 90*time.Second, cfg.Backend.QueryTimeout)
	require.Equal(t, 5439, cfg.Backend.Port)
}
