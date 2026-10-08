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
`FailedPrecondition`, `Unimplemented`, `ResourceExhausted`, `DeadlineExceeded`,
…) so clients never parse a vendor SQLSTATE or reason string. The text of a
failure is written by this server; the one piece of vendor text a client can
read is the diagnosis of its own invalid SQL (`InvalidArgument`).

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
| `bigquery` | **working** | catalog, DDL (incl. CTAS), queries with named parameters and a dry run, job get/cancel, streaming `InsertRows`; no `Load`, `Unload` or `Native` |
| `duckdb` | skeleton | intended embedded local engine (the "MinIO of warehouses") |
| `snowflake` | planned | |
| `redshift` | planned | |
| `clickhouse` | planned | |

What each wired backend serves, per RPC. Anything marked `Unimplemented` returns
that gRPC code and says so in `Capabilities`, so a client introspects first and
never discovers a gap by a wrong answer.

| RPC | `mem` | `bigquery` |
|-----|-------|------------|
| `Query` | `Unimplemented` | yes — named parameters, dry run, byte cap, timeout |
| `GetJob` / `CancelJob` | `NotFound` (it runs no jobs) | yes |
| `ListDatasets` / `ListTables` / `GetTable` | yes | yes |
| `CreateDataset` / `DropDataset` | yes | yes |
| `CreateTable` / `DropTable` | yes (CTAS: `Unimplemented`) | yes, including CTAS |
| `InsertRows` | `Unimplemented` | yes — streaming inserts, per-row refusals |
| `Load` / `Unload` | `Unimplemented` | `Unimplemented` |
| `Native` | `Unimplemented` | `Unimplemented` (no verbs) |

The next milestone is `duckdb` (SQL execution + Arrow encoding), which makes the
"develop on DuckDB, ship on BigQuery" story real; until then the zero-dependency
way to run the server is `SWH_BACKEND=mem`.

### The BigQuery backend

Credentials are Application Default Credentials, or the service-account key
named by `SWH_CREDENTIALS_FILE`; nothing else is read. The server is bound to
the project in `SWH_DATABASE`: jobs run and are billed in it, and `dataset` names
are datasets of it. What the credentials must be allowed to do follows the RPCs a
deployment uses: run query jobs and read data (`Query`, CTAS), read table
metadata (`GetTable`, `ListTables`, and `InsertRows`, which reads it to compare
the batch with the table), create and delete datasets and tables, and stream rows
into a table.

**Queries.** `sql` uses BigQuery's named parameters, `@name`; a parameter's `name`
is given without the sigil. `value` is text, checked here and bound by BigQuery
with the type you give, never spliced into the SQL: booleans as `true`/`false`,
integers and floats as decimal text, `NUMERIC` as a plain decimal (it becomes
`BIGNUMERIC` when only that holds it, and a value that would be rounded is
refused), bytes as standard base64, dates as `YYYY-MM-DD`, `TIME` and wall-clock
timestamps without a zone, instants as RFC 3339 with a zone, intervals as
`Y-M D H:M:S[.F]`, JSON and geography (WKT) as text. Array and struct parameters
are `Unimplemented`; sub-microsecond precision is refused, not truncated. The
query has finished by the time its header is sent (the schema is known only
then), so `CancelJob` cannot reach a running query: cancel the call, which also
cancels the job. The job `id` is opaque; hand it back to `GetJob`/`CancelJob`.
`max_bytes_billed` and `timeout_ms` are tightened by `SWH_MAX_QUERY_BYTES` and
`SWH_QUERY_TIMEOUT`: the smaller of the two applies. A `DML`/`DDL` statement
returns a header with an empty schema and `rows_affected`.

**Results.** The header's `arrow_ipc_schema` is one Arrow IPC *Schema message*, and
each `arrow_batch` is one IPC *RecordBatch message* with no schema, up to 8192
rows or about 1 MiB of text and bytes. The schema followed by the batches is a
valid Arrow IPC stream, so a client concatenates them and reads with any Arrow
reader. Types map as `BOOL`→bool, `INT64`→int64, `FLOAT64`→float64,
`NUMERIC`→decimal128 (`BIGNUMERIC`→decimal256), `STRING`/`JSON`/`GEOGRAPHY`→utf8,
`BYTES`→binary, `DATE`→date32, `TIME`→time64[us], `DATETIME`→timestamp[us],
`TIMESTAMP`→timestamp[us, UTC], `INTERVAL`→month-day-nano interval,
`ARRAY`→list, `STRUCT`→struct. A column with no portable bucket (BigQuery
`RANGE`) is `UNKNOWN` in the catalog with its spelling in `native_type`, and a
query that returns one is `Unimplemented`.

**Catalog.** Listings carry BigQuery's names only, so each entry's detail is a
metadata request of its own; pages default to 50 entries and never exceed 500.
`partition_by` is one column at day granularity (`_PARTITIONTIME` for ingestion
time); more than one is `Unimplemented`. Names are checked against what BigQuery
allows before they are used.

**`InsertRows`.** Arrow comes in the same framing: the header's schema message
and one RecordBatch message per batch, or, with no schema in the header, a first
message that is a schema message followed by its batch. A message that is not
Arrow (cut short, empty, a second schema where a batch belongs) or that states a
size over a limit (metadata over 1 MiB, a body over `SWH_MAX_ARROW_MESSAGE_BYTES`)
is `InvalidArgument`. Rows are sent as streaming inserts of at most 500 rows and
about 8 MiB. The call is not atomic: BigQuery is asked to skip invalid rows, so
valid rows are stored and refused ones come back as `errors`, each with its
`row_index` (counted from 0 across every batch of the call) and a `reason`:

| `RowRefusal` | meaning | set by `bigquery` when |
|--------------|---------|------------------------|
| `INVALID_VALUE` | a value cannot be stored in its column | BigQuery refuses the row's content, or a value cannot be encoded (text that is not UTF-8, an out-of-range time) |
| `ROW_TOO_LARGE` | the row alone is bigger than a row may be | the row passes 8 MiB as BigQuery encodes it |
| `SCHEMA_MISMATCH` | the table, not the row, disagrees | the batch has a column the table lacks, or lacks a required column; every row of the batch is refused and none is sent |
| `UNSPECIFIED` | refused, no reason stated | not set by `bigquery` |

A failure that says nothing against a row (a throttle, a quota, a timeout, a dead
backend) fails the call with a status code instead and is never a row refusal;
rows of earlier requests of that call may already be stored, so a retry can store
a row twice — delivery is at least once. A schema mismatch is found by comparing
the batch's columns with the table's metadata, so no vendor text decides it; a
table altered while the call runs can surface as `INVALID_VALUE`. An Arrow type
BigQuery has no equivalent of (map, duration, dictionary) is `Unimplemented`.

## Configuration (env)

| Var | Default | Notes |
|-----|---------|-------|
| `SWH_LISTEN` | `:9465` | gRPC listen address |
| `SWH_BACKEND` | `duckdb` | `mem` \| `duckdb` \| `bigquery` \| `snowflake` \| `redshift` \| `clickhouse` |
| `SWH_DATABASE` | — | required for cloud backends (BQ project / DB name); DuckDB path |
| `SWH_DATASET` | — | default namespace for unqualified names (for BigQuery also the default dataset of a query's SQL) |
| `SWH_LOCATION` | — | region for datasets/jobs (BigQuery: where jobs run and where a dataset is created when the request names none) |
| `SWH_DSN` | — | backend-native connection string (overrides discrete fields); `bigquery` has none and refuses it |
| `SWH_HOST` / `SWH_PORT` | — | SQL backend host/port; `bigquery` refuses them |
| `SWH_USER` / `SWH_PASSWORD` | — | credentials; `bigquery` refuses them |
| `SWH_ACCOUNT` | — | Snowflake account identifier; `bigquery` refuses it |
| `SWH_CREDENTIALS_FILE` | — | BigQuery service-account JSON (else ADC) |
| `SWH_MAX_QUERY_BYTES` | `0` | per-query scan cap (0 = backend default); for BigQuery the ceiling of `maximumBytesBilled`, which a request may only lower |
| `SWH_QUERY_TIMEOUT` | `0` | per-query timeout (Go duration; 0 = backend default); a request may only shorten it |
| `SWH_MAX_ARROW_MESSAGE_BYTES` | `67108864` (64 MiB) | largest body of one Arrow IPC message a client may send (`InsertRows`); a larger one is `InvalidArgument`, refused before any of it is allocated. Must be positive. grpc-go's own 4 MiB receive limit is checked first |

The `bigquery` backend reads `SWH_DATABASE` (required), `SWH_DATASET`,
`SWH_LOCATION`, `SWH_CREDENTIALS_FILE`, `SWH_MAX_QUERY_BYTES`,
`SWH_QUERY_TIMEOUT` and `SWH_MAX_ARROW_MESSAGE_BYTES`, and adds no variable of its
own. A setting it never reads is a startup error, not something silently ignored.

## Distribution and SBOM evidence

This repository **publishes no container image**. It builds to the
`service-warehouse` Go binary; nothing here builds, pushes, or deploys an image,
and the tree carries no Codefly agent manifest and no `Builder` implementation —
so no `Builder.SBOM` RPC is served and none is claimed.

Under the fleet image-SBOM contract ([`codefly-dev/core` `docs/sbom.md`][sbom],
released in `v0.3.29`) that is the `NO_IMAGE_REASON_NO_IMAGE` case: a service
that legitimately ships no image, which is a different thing from an agent whose
implementation is missing (`UNSUPPORTED`). A source or lockfile inventory never
counts as image coverage, so the absence of an image is recorded here as a
status — the Go module's dependency list does not satisfy it.

`TestRepositoryShipsNoImage` in
[`cmd/service-warehouse/distribution_test.go`](cmd/service-warehouse/distribution_test.go)
gates that status. It fails when this repository gains a way to **build** an
image (a Dockerfile, an `agent.codefly.yaml`, a compose file, or a workflow
running `docker build`/`docker buildx`/`docker/build-push-action`) *or* a way to
**deploy** one (a Kubernetes manifest, Helm values, or a Kustomize overlay
naming an image). Both matter: evidence binds to the digest actually built or
selected for deployment, so an image built elsewhere and deployed from here owes
coverage just the same.

The gate is scoped to this repository, which is the limit of what it can prove.
If another repository ever packages this binary into an image, the service ships
an image that nothing here can see — that case has to be recorded where that
image is built.

Introducing image distribution means this lands **with** the image rather than
after it:

- a valid CycloneDX SBOM for each final runtime image — OS packages and
  installed application dependencies — covering every shipped platform and every
  service-owned runtime, init, migration, and sidecar image;
- evidence bound to the immutable digest and platform actually built or
  deployed, served at image scope through the shared contract
  (`BuilderWrapper.SBOMImages`) and checked by `sbom.ValidateCoverage`;
- the SBOM artifact, checksum, image digest, platform, and service identity
  carried into the build/release report and retrievable with the image;
- failures reported as failures — a failed scan, a missing image, a stale digest
  or an omitted platform must never read as complete coverage.

The scanner itself stays in `core`; this repository does not reimplement it.

[sbom]: https://github.com/codefly-dev/core/blob/v0.3.29/docs/sbom.md

## Develop

```bash
# regenerate stubs from proto (requires the codefly CLI + Docker)
codefly generate proto --proto ./proto --output ./gen

go build ./...
go vet ./...
go test -race ./...      # unit tests: mem, a bufconn server, and the bigquery
                         # backend against a fake of BigQuery's REST API
```

The `bigquery` unit tests contact nothing: they run the real client against a
fake of BigQuery's REST API served from the test process, so they check what the
backend sends and how it reads an answer, not what BigQuery does. The one test
that talks to BigQuery, `TestAgainstRealBigQuery` in
[`internal/backend/bigquery/integration_test.go`](internal/backend/bigquery/integration_test.go),
is skipped unless `SWH_TEST_BIGQUERY_PROJECT` names a project it may use. It
creates a scratch dataset of its own with a random name, works only inside it, and
deletes it. `SWH_TEST_BIGQUERY_LOCATION` and `SWH_TEST_BIGQUERY_CREDENTIALS_FILE`
are optional (Application Default Credentials otherwise). CI runs no BigQuery, so
that test has not run in CI.

`buf` is never run on the host. `codefly generate proto` runs it inside the
versioned proto companion image (`ghcr.io/codefly-dev/proto`), so the plugin
versions and the `goimports` pass are the image's, fixed by its tag — two
machines regenerate the same bytes. `proto/buf.gen.yaml` is the config that
image runs, which is why it sits with the protos rather than at the root.

Running `buf generate` directly would resolve `protoc-gen-go` and
`protoc-gen-go-grpc` from your `PATH` at whatever versions happen to be
installed. That drift shows up only as the `// versions:` banner in each
generated file, so it reads as harmless and is easy to commit by accident.
`TestGeneratedStubsMatchPinnedGenerators` in
[`cmd/service-warehouse/generation_test.go`](cmd/service-warehouse/generation_test.go)
fails when the committed stubs carry a generator version other than the
companion's — the drift gate, without putting `buf` in CI.

## Layout

```
proto/codefly/warehouse/v0/   the uniform API
proto/buf.{yaml,gen.yaml}     buf config, read by the proto companion
gen/                          generated gRPC stubs
internal/backend/             Backend interface + mem, duckdb, bigquery (+ more cloud backends)
internal/arrowipc/            the Arrow IPC framing every backend shares (schema message, batch messages)
internal/server/              gRPC Warehouse implementation + proto↔backend mapping
internal/config/              env configuration
internal/serr/                normalized error model
cmd/service-warehouse/        the server binary
```
