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
as an explicit local-development opt-in. The generated deployment requires
authentication and never enables anonymous access.

With `SWH_AUTH_TOKEN` set, every RPC — unary and streaming, `Capabilities`
included — must carry the secret as the `x-codefly-token` gRPC metadata header,
the key the Codefly host already uses to authenticate agent plugins. Anything
else is rejected with `UNAUTHENTICATED` before it reaches a backend. The token
is compared in constant time and never logged by core's `callertoken` package.
Only `grpc.health.v1.Health/Check` and `/Watch` are exempt; `/List` still needs
the token. These probes report that the process is serving, not that a warehouse
is reachable. An interceptor configured with an empty server token fails closed;
the explicit anonymous mode installs no token interceptor.

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
fresh 64-character lowercase hex token (256 random bits) for every local run
unless an operator pins one, so a run revokes the previous run's.

**Locally** (`codefly run`) the agent starts the gateway container, publishes its
port, waits until the gateway reports itself serving *and* accepts the agent's
token, and hands the endpoint and token to consumers. That wait attests the
process and the credential, not the warehouse: the gateway does not probe its
backend and no RPC reports that. Set `backend:` in `service.codefly.yaml` to
choose the engine. Newly generated services declare `backend: mem` (the
catalog-only backend, with no query execution). Removing that setting falls back
to the gateway's unsupported `duckdb` default and startup fails. The agent's
`Test` step performs the same health and authenticated `Capabilities` check as
`Start`, so conformance's Init → Test path rejects a dead gateway. The agent delivers `SWH_BACKEND`, `SWH_DATABASE`,
`SWH_DATASET` and `SWH_LOCATION`, and refuses `SWH_CREDENTIALS_FILE`, which
nothing could project into the container. `SWH_GATEWAY_IMAGE` makes the agent
run a locally built image instead of the pinned one (`docker build -t
service-warehouse:local .`); it configures the agent, not the gateway.

**Deployed** the agent renders a Deployment, Service, dedicated ServiceAccount
and NetworkPolicy, with gRPC health probes on the gateway's overall service.
The listener requires a token. Supply `SWH_AUTH_TOKEN` through the deployment's
Kubernetes `secret_references`, referencing a secret provisioned by the
environment's secret manager; plaintext tokens in deployment requests are
refused. Both ephemeral and restricted output profiles use `secretKeyRef`:

```yaml
secret_references:
  SWH_AUTH_TOKEN:
    name: warehouse-auth
    key: token
```

Provision a random 64-character hex token once per environment and bind **the
same secret key** to each consumer's `warehouse.token` secret configuration.
For a consumer that uses Codefly's configuration carriers, its secret-reference
map names `CODEFLY__SERVICE_SECRET_CONFIGURATION__<MODULE>__<SERVICE>__WAREHOUSE__TOKEN`
(the warehouse producer's module and service, uppercased) and points to that
same `warehouse-auth` / `token` key. No token is embedded in an endpoint or
manifest. The agent does not create or rotate deployed secrets, or automatically
copy secret references between services: the environment owns both bindings.
An absent secret prevents pod startup; an absent or wrong consumer token is
rejected before any SQL reaches the backend.

The NetworkPolicy allows TCP ingress to the gateway port from pods in its own
namespace only. Enforcement requires a CNI that implements NetworkPolicy. This
is a namespace boundary, not a declared-consumer allowlist; same-namespace pods
still need the token. Cross-namespace consumers require an explicit policy
overlay. No service mesh or mTLS is installed by this agent.

`SWH_ALLOW_ANONYMOUS=true` is refused in deployment configuration. A configured
`SWH_CREDENTIALS_FILE` is also refused because nothing mounts that file: grant
the dedicated ServiceAccount a workload identity, or supply a mount and variable
through an external overlay. Other `SWH_*` values are bound onto the container
by name from the environment's service configuration after the render.

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

- [`internal/gatewayimage`](internal/gatewayimage/image.go) declares the shipped
  platforms and role. Core's `sbom.ImageSubjects` derives one subject per declared
  platform; local images get their digest and platform from the Docker daemon.
- [`cmd/image-sbom`](cmd/image-sbom/main.go) is a thin entry point to core's
  `CollectImageEvidence` and `WriteImageEvidenceIndex`. A release runs it
  against the digest [`gateway-image.json`](gateway-image.json) records and
  saves them as the `gateway-image-sbom-<version>` workflow artifact before
  the image tags are created, then attaches every CycloneDX document and the
  index to the GitHub release. Release assets retain the evidence after workflow
  artifacts expire; `attach-image-evidence` must finish green before release
  verification is complete.
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

**Publisher stopgap pending [core#770](https://github.com/codefly-dev/core/issues/770):**
GHCR checks the workflow's defining repository. Core's reusable publisher was
denied `installation not allowed to Write organization package` when publishing
a service package. `publish-gateway-image.yml` therefore defines the job here,
logs in with its own `GITHUB_TOKEN` and `packages: write`, refuses unmerged
commits before login, checks out with `persist-credentials: false`, and pushes
by digest with no tags. Replace these local publishing steps with core 0.18.2's
composite action once core#770 is available and adopted; keep the job in this
repository. The other core integrations remain pinned to v0.18.1.

The coordinator performs the following first-release sequence. The manifest's
seed is `0.0.1`; `codefly publish` increments it, so the **first release is
`v0.0.2`** and the image publisher must receive **`0.0.2`**.

1. Merge the reviewed gateway PR after its CI is green.
2. On clean, synced `main`, run `codefly publish --dry-run` and confirm
   `0.0.1` → `0.0.2`.
3. Dispatch `publish-gateway-image` on `main` with `version=0.0.2` (no `v`).
   The in-repository publisher pushes by digest and records `gateway-image.json`
   in the `gateway-image-lock-0.0.2` artifact and run summary. It uses
   `persist-credentials: false` and refuses a commit outside the default branch's
   history.
4. Make the new `ghcr.io/codefly-dev/service-warehouse` package **public** in
   GitHub package settings. Verify an anonymous pull by the returned digest
   using a fresh Docker configuration with no registry login.
5. Commit the returned lock in a reviewed PR. Let CI inventory both declared
   platforms (`linux/amd64`, `linux/arm64`), then merge that PR.
6. Run the full `codefly agent ci` on synced `main`, including its audit and
   generated-service `codefly ci run`. After it has passed, enable the PR trigger
   in `agent-ci.yml`.
7. Confirm the repository's `GH_PAT` release secret is configured, and retain
   source packager `codefly.dev/go:0.0.69` and the patched Go 1.27.2 toolchain.
8. From clean, synced `main` with Docker available, run `codefly publish`.
   Verify that the tag matches `agent.codefly.yaml`, the locked image's version
   labels match `0.0.2`, image tags resolve to the locked digest, and the workflow
   attaches the image SBOM documents and index alongside the agent archives.
9. Verify the release and the production loader's archive resolution. Close
   [#12](https://github.com/codefly-dev/service-warehouse/issues/12) only then,
   with the release URL and the per-row answer below.

**Per-row error answer at this revision:** `RowError` contains `row_index` and
free-form `string error`; it has **no permanent/retryable code**. Consumers cannot
branch on retryability without parsing text. The separate API work in
[#14](https://github.com/codefly-dev/service-warehouse/pull/14) must land before
claiming otherwise. This agent PR references #12; it does not close it.

The lock is parsed by core's `resources.ParseImageLock`: it must name
`ghcr.io/codefly-dev/service-warehouse`, contain only `name` and `digest`, and
use `sha256:` followed by 64 lowercase hex characters. An empty digest records
an unpublished image and is refused by the agent and release. CI permits the
initial empty lock only with complete history, no `v*` tag, and no prior nonempty
lock in fetched history. Clearing an established lock fails CI even before the
first tag.

[sbom]: https://github.com/codefly-dev/core/blob/v0.18.1/docs/sbom.md

## Develop

```bash
# regenerate stubs from proto (requires the codefly CLI + Docker)
codefly generate proto --proto ./proto --output ./gen

go build ./...
go vet ./...
go test -race ./...      # unit tests (mem backend + bufconn server) and the agent's
```

The release-gate regression tests use `git` and `jq` (also present in CI).

The source packager is pinned to `codefly.dev/go:0.0.69`, whose image recipes use
immutable digests. `go.mod` and CI require Go 1.27.2; the gateway Dockerfile pins
the Go 1.27.2 image index and sets `GOTOOLCHAIN=go1.27.2`. For local conformance,
run `GOTOOLCHAIN=go1.27.2 codefly agent ci` so host toolchain selection is explicit.
Before the first image lock, the agent-binary audit can pass but full generated
service conformance still fails closed on the unpublished image.

The agent's end-to-end tests need Docker and are behind the `e2e` tag; the SBOM
one also needs `syft` on `PATH`:

```bash
docker build -t service-warehouse:e2e .
SWH_GATEWAY_IMAGE=service-warehouse:e2e go test -v -tags e2e -count=1 .
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
internal/config/              env configuration
internal/serr/                normalized error model
cmd/service-warehouse/        the server binary
cmd/image-sbom/               writes the image's SBOM evidence (run by the release)
internal/gatewayimage/        the shipped platforms and runtime role; core derives evidence
main.go runtime.go builder.go the codefly agent (codefly.dev/warehouse) that runs the gateway
agent.codefly.yaml            the agent's manifest and version
gateway-image.json            the gateway image the agent pins, by digest
Dockerfile                    the gateway image
templates/                    the agent's README, factory files and Kubernetes manifests
```
