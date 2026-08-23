# service-warehouse

A **server exposing a generic data-warehouse API** over a uniform gRPC contract,
backed by **BigQuery / Snowflake / Redshift / ClickHouse**, with **DuckDB** as
the embedded local/test engine.

Clients speak only the gRPC API and **never link a warehouse SDK**. All
provider-specificity lives in this one server — written once, all backends
compiled in and selected by config.

This is the warehouse analog of
[`service-object-storage`](https://github.com/codefly-dev/service-object-storage):
that gateway abstracts S3 / GCS / Azure / MinIO behind one object API; this one
abstracts the SQL warehouses behind one query + catalog API.

## Why

Every app that talks to a warehouse otherwise links a heavy vendor SDK, holds
vendor credentials, and hard-codes one vendor's dialect and result format. This
replaces that: the app talks to one gateway, and the backend is a per-environment
choice — **local/test runs DuckDB**, deployed names the real warehouse. Because
the app only ever speaks the uniform API, you **develop on DuckDB and ship on
BigQuery**; the gap is absorbed and tested once, here, not in every app.

## The API (`codefly/warehouse/v0`)

The honest intersection across the SQL warehouses (SQL in, Arrow out — the model
BigQuery Storage / Snowflake / ADBC converged on). Anything backend-specific
(time-travel, clustering ops, warehouse resize, …) is reached only through
`Native`.

| RPC | Purpose |
|-----|---------|
| `Query` | submit SQL (+ bind params, dry-run, byte cap); stream **Arrow** result batches |
| `GetJob` / `CancelJob` | inspect / cancel an async job |
| `ListDatasets` / `ListTables` / `GetTable` | catalog introspection |
| `CreateDataset` / `DropDataset` | namespace DDL |
| `CreateTable` / `DropTable` | table DDL (incl. CTAS) |
| `Load` | bulk-load object-storage files (CSV/JSON/Parquet/Avro/ORC) into a table |
| `Unload` | export a table or query to object storage |
| `InsertRows` | streaming Arrow row appends |
| `Capabilities` | machine-readable feature set — introspect before calling |
| `Native` | escape hatch for backend-specific verbs |

Errors are normalized to gRPC status codes (`NotFound`, `AlreadyExists`,
`FailedPrecondition`, `Unimplemented`, `ResourceExhausted`, …) so clients never
parse a vendor SQLSTATE or reason string.

## Results are Arrow

`Query` streams a header (portable result schema + the equivalent Arrow-IPC
schema) followed by Arrow-IPC record batches. This is the encoding the industry
converged on for warehouse egress, so a client feeds batches straight into any
Arrow reader — no per-vendor row shape.

## Datasets vs. tables

The server is bound to one **database** (a BigQuery project, a Snowflake /
Redshift database, a ClickHouse server, or a DuckDB file). Within it, a
**dataset** is the namespace — a BigQuery dataset, a Snowflake / Redshift
schema, or a ClickHouse database — and a **table** is the leaf.

## Backends

| Kind | Status | Notes |
|------|--------|-------|
| `mem` | **working** | in-memory catalog + DDL, no query engine — the zero-dep default for tests |
| `duckdb` | skeleton | intended embedded local engine (the "MinIO of warehouses") |
| `bigquery` | planned | |
| `snowflake` | planned | |
| `redshift` | planned | |
| `clickhouse` | planned | |

Today `mem` is the only fully-wired backend: it serves the whole catalog/DDL
plane and returns `Unimplemented` for query and data-movement, so a client
introspects `Capabilities` and never discovers the gap by a wrong answer. The
next milestone is `duckdb` (SQL execution + Arrow encoding), which makes the
"develop on DuckDB, ship on BigQuery" story real.

## Configuration (env)

| Var | Default | Notes |
|-----|---------|-------|
| `SWH_LISTEN` | `:9465` | gRPC listen address |
| `SWH_BACKEND` | `duckdb` | `mem` \| `duckdb` \| `bigquery` \| `snowflake` \| `redshift` \| `clickhouse` |
| `SWH_DATABASE` | — | required for cloud backends (BQ project / DB name); DuckDB path |
| `SWH_DATASET` | — | default namespace for unqualified names |
| `SWH_LOCATION` | — | region for datasets/jobs (BigQuery) |
| `SWH_DSN` | — | backend-native connection string (overrides discrete fields) |
| `SWH_HOST` / `SWH_PORT` | — | SQL backend host/port |
| `SWH_USER` / `SWH_PASSWORD` | — | credentials |
| `SWH_ACCOUNT` | — | Snowflake account identifier |
| `SWH_CREDENTIALS_FILE` | — | BigQuery service-account JSON (else ADC) |
| `SWH_MAX_QUERY_BYTES` | `0` | per-query scan cap (0 = backend default) |
| `SWH_QUERY_TIMEOUT` | `0` | per-query timeout (Go duration; 0 = backend default) |

## Develop

```bash
# regenerate stubs from proto (requires buf + protoc-gen-go/-grpc)
buf generate

go build ./...
go vet ./...
go test ./...            # unit tests (mem backend + bufconn server)
```

## Layout

```
proto/codefly/warehouse/v0/   the uniform API
gen/                          generated gRPC stubs
internal/backend/             Backend interface + mem, duckdb (+ cloud backends)
internal/server/              gRPC Warehouse implementation + proto↔backend mapping
internal/config/              env configuration
internal/serr/                normalized error model
cmd/service-warehouse/        the server binary
```
