// Package duckdb is the intended local warehouse engine — the "MinIO of
// warehouses": an embedded, SQL-native, columnar engine you develop and test
// against before shipping on BigQuery / Snowflake / Redshift / ClickHouse.
//
// It is a skeleton. Wiring it means adding a DuckDB driver (cgo:
// github.com/marcboeker/go-duckdb, or the pure-Go replicator when it matures),
// executing SQL, and encoding result rows as Arrow-IPC batches through
// backend.BatchReader. Until then the constructor fails loudly so a
// misconfigured SWH_BACKEND=duckdb never silently degrades — use "mem" for the
// catalog-only local backend.
package duckdb

import (
	"context"

	"github.com/codefly-dev/service-warehouse/internal/backend"
	"github.com/codefly-dev/service-warehouse/internal/serr"
)

func init() {
	backend.Register("duckdb", func(_ context.Context, _ backend.Config) (backend.Backend, error) {
		return nil, serr.New(serr.Unsupported, "duckdb.Open",
			"duckdb backend not yet implemented — use SWH_BACKEND=mem for the catalog-only local backend")
	})
}
