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
