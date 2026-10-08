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
| `SWH_AUTH_TOKEN` | — | shared secret every caller must present; required unless `SWH_ALLOW_ANONYMOUS=true` |
| `SWH_ALLOW_ANONYMOUS` | `false` | `true` accepts callers with no token; refused together with `SWH_AUTH_TOKEN` |

## Authentication

The server holds the warehouse credentials and runs SQL against the database it
is bound to, and the listen address says nothing about who can reach it. So it
**refuses to start on an unauthenticated listener** unless the operator says
otherwise: `SWH_AUTH_TOKEN` must be set, or `SWH_ALLOW_ANONYMOUS` must be `true`
(for a listener confined to a private boundary enforced elsewhere, such as a
cluster NetworkPolicy or service-mesh mTLS).

With `SWH_AUTH_TOKEN` set, every RPC — unary and streaming, `Capabilities`
included — must carry the secret as the `x-codefly-token` gRPC metadata header,
the key the Codefly host already uses to authenticate agent plugins. Anything
else is rejected with `UNAUTHENTICATED` before it reaches a backend. The token
is compared in constant time and never logged. The standard gRPC health service
(`grpc.health.v1.Health`) is the one exemption: a readiness probe carries no
token. It reports that the process is serving, not that a warehouse is
reachable.

This is authentication only — whether the caller holds the deployment's token.
The server still authorizes nothing: which caller may run which statement is
settled before the request arrives.

## Running as a codefly service

A workspace composes this gateway as `codefly.dev/warehouse`, the same way it
composes [`service-object-storage`](https://github.com/codefly-dev/service-object-storage)
as `codefly.dev/object-storage`. The agent is the repo-root `main` package
([`main.go`](main.go), [`runtime.go`](runtime.go), [`builder.go`](builder.go)),
declared by [`agent.codefly.yaml`](agent.codefly.yaml); it is released as a GitHub
release asset a consumer pins by version. The gateway it runs is a container
image, pinned by digest in [`gateway-image.json`](gateway-image.json).

**What a consumer receives.** The configuration group `warehouse`, with
`connection` (`grpc://host:port`), `endpoint` (`host:port`) and `token`, a
secret value to send as the `x-codefly-token` metadata header. The agent mints a
fresh token for every local run unless an operator pins one, so a run revokes the
previous run's.

**Locally** (`codefly run`) the agent starts the gateway container, publishes its
port, waits until the gateway reports itself serving *and* accepts the agent's
token, and hands the endpoint and token to consumers. That wait attests the
process and the credential, not the warehouse: the gateway does not probe its
backend and no RPC reports that. Set `backend:` in `service.codefly.yaml` to
choose the engine; left unset, the gateway's own default applies, which today is
`duckdb` and refuses to start, so a local run presently needs `backend: mem`
(the catalog-only backend). The agent delivers `SWH_BACKEND`, `SWH_DATABASE`,
`SWH_DATASET` and `SWH_LOCATION`, and refuses `SWH_CREDENTIALS_FILE`, which
nothing could project into the container. `SWH_GATEWAY_IMAGE` makes the agent
run a locally built image instead of the pinned one (`docker build -t
service-warehouse:local .`); it configures the agent, not the gateway.

**Deployed** the agent renders a Deployment, Service and its own ServiceAccount
(so a cloud warehouse can grant exactly this workload through a workload
identity), with gRPC health probes on the gateway's overall service. The
deployed listener is `SWH_ALLOW_ANONYMOUS=true`: caller identity is enforced by
the cluster (NetworkPolicy and service-mesh mTLS), because nothing delivers a
secret to the consumers' `warehouse` configuration for a deployment. A configured
`SWH_AUTH_TOKEN` or `SWH_CREDENTIALS_FILE` is therefore refused rather than
rendered, and so is a value the manifest cannot carry. The rest of the `SWH_*`
surface reaches the gateway by the environment's service configuration being
bound onto the container by name, after the render.

## Distribution and SBOM evidence

This repository publishes **one container image**, the gateway
(`ghcr.io/codefly-dev/service-warehouse`, for `linux/amd64` and `linux/arm64`),
and the agent binary that runs it. The service ships no other image: it has no
init, migration or sidecar image, and the warehouse behind it is not run by this
service.

Under the fleet image-SBOM contract ([`codefly-dev/core` `docs/sbom.md`][sbom])
that image owes evidence, and the agent serves it. `Builder.SBOM` at image scope
returns one CycloneDX inventory per shipped platform, covering OS packages and
installed application dependencies, each bound to the image digest and platform
that was scanned and attributed to the service. The scanner is core's
(`sbom.Image`, through `BuilderWrapper.SBOMImages`), and the response is checked
against core's `sbom.ValidateCoverage`.

- [`internal/imageevidence`](internal/imageevidence/imageevidence.go) holds the
  one list of shipped platforms and the subjects derived from it; the agent, the
  release command and the tests all read it.
- [`cmd/image-sbom`](cmd/image-sbom/main.go) writes the documents. A release runs
  it against the digest [`gateway-image.json`](gateway-image.json) records and
  attaches them as the `gateway-image-sbom-<version>` workflow artifact, before
  the tags consumers resolve are created, so an image whose scan failed is never
  pullable by the version it was built for.
- CI builds the image and checks the same contract against it (`image-sbom`), and
  runs the agent against it (`agent-runtime-e2e`).
- `ValidationCapabilities.image_sbom` is **served but not advertised**: core
  holds the advertisement back until every consumer that evaluates it runs a core
  that can represent the phase, and a present `Validation` is authoritative for
  every operation it omits. `TestImageSBOMIsServedButNotAdvertised` marks it.

`TestEveryImageThisRepositoryShipsIsCovered` in
[`cmd/service-warehouse/distribution_test.go`](cmd/service-warehouse/distribution_test.go)
gates this. It lists every path that builds an image (a Dockerfile, an
`agent.codefly.yaml`, a compose file, a workflow running
`docker build`/`docker buildx`/`docker/build-push-action`) or deploys one (a
Kubernetes manifest, Helm values, a Kustomize overlay naming an image), and fails
on any that is not accounted for, or on one that stopped producing its signal,
or when the tests holding the evidence are gone. A second image therefore has to
arrive with its evidence and with an edit to that list.

The gate is scoped to this repository, which is the limit of what it can prove.
If another repository ever packages this binary into an image, the service ships
an image that nothing here can see, and that case has to be recorded where that
image is built.

### Releasing

The digest is only known once the image is built, so the order is fixed, and
`release.yml` refuses to tag a tree that skips a step:

1. Find the upcoming version with `codefly publish --dry-run`.
2. Run the `publish-gateway-image` workflow with that version (no `v`). It
   pushes the image by digest, carrying no tag, and prints `gateway-image.json`.
3. Commit that file in a reviewed change. Until it is committed the lock records
   no digest, and the agent, the deployment and the release all refuse to name an
   image.
4. `codefly publish` tags the release. The tag workflow tests, checks that the
   tag matches `agent.codefly.yaml`, that the recorded digest was built for this
   version, inventories it, and only then creates the `:<version>` and `:latest`
   tags and the GitHub release carrying the agent.

The release call needs a `GH_PAT` secret in the repository (`secrets: inherit`).

[sbom]: https://github.com/codefly-dev/core/blob/v0.16.0/docs/sbom.md

## Develop

```bash
# regenerate stubs from proto (requires the codefly CLI + Docker)
codefly generate proto --proto ./proto --output ./gen

go build ./...
go vet ./...
go test -race ./...      # unit tests (mem backend + bufconn server) and the agent's
```

The agent's end-to-end tests need Docker and are behind the `e2e` tag; the SBOM
one also needs `syft` on `PATH`:

```bash
docker build -t service-warehouse:e2e .
SWH_GATEWAY_IMAGE=service-warehouse:e2e go test -tags e2e -count=1 .
```

With `SWH_GATEWAY_IMAGE` unset they skip, and a skipped run still prints `ok`:
read `-v` for `--- PASS`.

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
internal/backend/             Backend interface + mem, duckdb (+ cloud backends)
internal/server/              gRPC Warehouse implementation + proto↔backend mapping
internal/auth/                caller authentication (the x-codefly-token interceptors)
internal/config/              env configuration
internal/serr/                normalized error model
cmd/service-warehouse/        the server binary
cmd/image-sbom/               writes the image's SBOM evidence (run by the release)
internal/imageevidence/       the shipped platforms and the SBOM subjects derived from them
main.go runtime.go builder.go the codefly agent (codefly.dev/warehouse) that runs the gateway
agent.codefly.yaml            the agent's manifest and version
gateway-image.json            the gateway image the agent pins, by digest
Dockerfile                    the gateway image
templates/                    the agent's README, factory files and Kubernetes manifests
```
