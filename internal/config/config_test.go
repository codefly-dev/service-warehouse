package config

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/service-warehouse/internal/backend"

	// The kinds FromEnv resolves exist only once their packages are linked in,
	// exactly as in cmd/service-warehouse. Without these the tests would assert
	// against an empty registry.
	_ "github.com/codefly-dev/service-warehouse/internal/backend/duckdb"
	_ "github.com/codefly-dev/service-warehouse/internal/backend/mem"
)

// boundKind registers a backend that is bound to a database and can be given a
// DSN instead, standing in for the cloud backends that are not implemented.
// Registering a real kind's name would assert a support claim this repository
// does not make. noDSNKind is the same without a DSN, which is what a project-
// bound backend such as BigQuery looks like to the configuration.
const (
	boundKind = "test-bound"
	noDSNKind = "test-bound-no-dsn"
)

func init() {
	backend.Register(boundKind, backend.Registration{
		DSN:  true,
		Open: func(context.Context, backend.Config) (backend.Backend, error) { return nil, nil },
	})
	backend.Register(noDSNKind, backend.Registration{
		Open: func(context.Context, backend.Config) (backend.Backend, error) { return nil, nil },
	})
}

// clearEnv blanks every SWH_* variable so a test describes the whole
// environment rather than inheriting the developer's shell.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"SWH_LISTEN", "SWH_BACKEND", "SWH_DATABASE", "SWH_DATASET", "SWH_LOCATION",
		"SWH_DSN", "SWH_HOST", "SWH_PORT", "SWH_USER", "SWH_PASSWORD", "SWH_ACCOUNT",
		"SWH_CREDENTIALS_FILE", "SWH_MAX_QUERY_BYTES", "SWH_QUERY_TIMEOUT", "SWH_MAX_ARROW_MESSAGE_BYTES",
	} {
		t.Setenv(k, "")
	}
}

func TestSelfContainedBackendsNeedNoDatabase(t *testing.T) {
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

func TestBoundBackendRequiresDatabaseOrDSN(t *testing.T) {
	clearEnv(t)
	t.Setenv("SWH_BACKEND", boundKind)

	_, err := FromEnv()
	require.ErrorContains(t, err, "SWH_DATABASE")
	require.ErrorContains(t, err, boundKind)
}

// A backend that reads no DSN must not be told to supply one: that sends the
// operator to set a variable the backend then refuses.
func TestBackendWithoutDSNAsksForTheDatabaseOnly(t *testing.T) {
	clearEnv(t)
	t.Setenv("SWH_BACKEND", noDSNKind)

	_, err := FromEnv()
	require.ErrorContains(t, err, "SWH_DATABASE")
	require.NotContains(t, err.Error(), "SWH_DSN")

	t.Setenv("SWH_DSN", "user:pass@account/db")
	_, err = FromEnv()
	require.ErrorContains(t, err, "SWH_DATABASE", "a DSN does not stand in for the database of a backend that has none")
}

func TestBoundBackendAcceptsDSNAlone(t *testing.T) {
	clearEnv(t)
	t.Setenv("SWH_BACKEND", boundKind)
	t.Setenv("SWH_DSN", "user:pass@account/db")

	cfg, err := FromEnv()
	require.NoError(t, err)
	require.Equal(t, "user:pass@account/db", cfg.Backend.DSN)
}

// An unregistered kind is reported as unimplemented, not as a missing database:
// telling an operator to supply SWH_DATABASE sends them to provision one for a
// backend that does not exist.
func TestUnknownBackendIsRejectedBeforeItsDatabase(t *testing.T) {
	clearEnv(t)
	t.Setenv("SWH_BACKEND", "snowflake")

	_, err := FromEnv()
	require.ErrorContains(t, err, `unknown backend kind "snowflake"`)
	require.ErrorContains(t, err, "mem")
	require.NotContains(t, err.Error(), "SWH_DATABASE")
}

// A malformed limit must not resolve to the zero value: every backend spends 0
// as "no limit", so silently falling back would uncap a query the operator
// meant to cap.
func TestMalformedLimitsAreRejected(t *testing.T) {
	for _, tc := range []struct{ key, value, want string }{
		{"SWH_MAX_QUERY_BYTES", "1GB", "not an integer"},
		{"SWH_MAX_QUERY_BYTES", "1_000_000", "not an integer"},
		{"SWH_MAX_QUERY_BYTES", "1e9", "not an integer"},
		{"SWH_MAX_QUERY_BYTES", "-5", "must not be negative"},
		{"SWH_QUERY_TIMEOUT", "90", "not a duration"},
		{"SWH_QUERY_TIMEOUT", "5 minutes", "not a duration"},
		{"SWH_QUERY_TIMEOUT", "-30s", "must not be negative"},
		{"SWH_PORT", "abc", "not an integer"},
		{"SWH_MAX_ARROW_MESSAGE_BYTES", "64MiB", "not an integer"},
		{"SWH_MAX_ARROW_MESSAGE_BYTES", "-1", "must be positive"},
		// Zero is refused rather than read as "the default": a zero that reached
		// the Arrow reader would be no bound at all.
		{"SWH_MAX_ARROW_MESSAGE_BYTES", "0", "must be positive"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(tc.key, tc.value)

			_, err := FromEnv()
			require.Error(t, err)
			require.ErrorContains(t, err, tc.key)
			require.ErrorContains(t, err, tc.want)
		})
	}
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
	require.Equal(t, int64(64<<20), cfg.Backend.MaxArrowMessageBytes, "the Arrow message bound is on by default")
}

func TestArrowMessageBoundIsConfigurable(t *testing.T) {
	clearEnv(t)
	t.Setenv("SWH_MAX_ARROW_MESSAGE_BYTES", "1048576")

	cfg, err := FromEnv()
	require.NoError(t, err)
	require.Equal(t, int64(1<<20), cfg.Backend.MaxArrowMessageBytes)
}

// Unset limits keep meaning "use the backend default"; rejecting malformed
// values must not make an absent value an error.
func TestUnsetLimitsFallBackToDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := FromEnv()
	require.NoError(t, err)
	require.Zero(t, cfg.Backend.MaxQueryBytes)
	require.Zero(t, cfg.Backend.QueryTimeout)
	require.Zero(t, cfg.Backend.Port)
}
