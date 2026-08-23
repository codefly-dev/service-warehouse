// Package config resolves the server configuration from environment variables.
// The default backend is DuckDB: local and test contexts always run against an
// embedded DuckDB, and only a deployed environment names a cloud warehouse —
// so you develop against DuckDB and ship on BigQuery, and the gap is absorbed
// once, here, not in every app.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/codefly-dev/service-warehouse/internal/backend"
)

// Config is the fully resolved server configuration.
type Config struct {
	ListenAddr string
	Backend    backend.Config
}

// FromEnv resolves configuration from SWH_* environment variables.
func FromEnv() (Config, error) {
	cfg := Config{
		ListenAddr: env("SWH_LISTEN", ":9465"),
		Backend: backend.Config{
			Kind:            env("SWH_BACKEND", "duckdb"),
			Database:        os.Getenv("SWH_DATABASE"),
			DefaultDataset:  os.Getenv("SWH_DATASET"),
			Location:        os.Getenv("SWH_LOCATION"),
			DSN:             os.Getenv("SWH_DSN"),
			Host:            os.Getenv("SWH_HOST"),
			Port:            int(envInt("SWH_PORT", 0)),
			User:            os.Getenv("SWH_USER"),
			Password:        os.Getenv("SWH_PASSWORD"),
			Account:         os.Getenv("SWH_ACCOUNT"),
			CredentialsFile: os.Getenv("SWH_CREDENTIALS_FILE"),
			MaxQueryBytes:   envInt("SWH_MAX_QUERY_BYTES", 0),
			QueryTimeout:    envDuration("SWH_QUERY_TIMEOUT", 0),
		},
	}
	// DuckDB is self-contained; every cloud backend is bound to a database.
	if cfg.Backend.Kind != "duckdb" && cfg.Backend.Database == "" && cfg.Backend.DSN == "" {
		return Config{}, fmt.Errorf("SWH_DATABASE (or SWH_DSN) is required for backend %q", cfg.Backend.Kind)
	}
	return cfg, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
