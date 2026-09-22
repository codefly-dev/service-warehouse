# service-warehouse

The entry point for this repo. A **gateway**: one gRPC contract for the SQL
warehouses (BigQuery / Snowflake / Redshift / ClickHouse, with DuckDB as the
embedded local engine), so a client speaks SQL in and Arrow out and never links
a vendor SDK. [README.md](README.md) owns *what* the service promises — the API
table, the backend matrix, the env surface, the distribution status.
`proto/codefly/warehouse/v0/warehouse.proto` is the contract of record. This
file owns *how to work here*; if it disagrees with the code, the code wins and
this file needs a fix in the same PR.

## Boundaries — non-negotiable

- **Owns:** all provider-specificity for the SQL warehouses — written once,
  every backend compiled into one binary and selected by config. The portable
  type bucket (`internal/backend/backend.go`), the normalized error model
  (`internal/serr/serr.go`), and the Arrow encoding of results.
- **Never contains:** anything a caller knows and this server does not — a
  tenant, a permission, a product noun, a business rule, or which application
  is asking. The server is bound to one database and authorizes nothing; the
  caller's authority is settled before the request arrives.
- **Depends on:** the warehouse drivers and the gRPC contract, nothing else.
  The SBOM scanner and the image-SBOM contract live in `codefly-dev/core` and
  are not reimplemented here.

Three rules follow from that:

1. **The API is the honest intersection.** An operation goes into the uniform
   contract only when every backend can mean the same thing by it. Anything
   backend-specific is reached through `Native` — not through a new RPC, and
   not through an option one backend silently ignores.
2. **A gap is `Unimplemented`, never a wrong answer.** `mem` serves the whole
   catalog/DDL plane and returns `serr.Unsupported` for query and data
   movement, which `internal/server/errors.go` maps to gRPC `Unimplemented`.
   A client introspects `Capabilities` and is never told something false. Do
   not paper a gap over with a plausible empty result.
3. **Clients branch on codes, never on strings.** Every backend error is
   mapped into a `serr.Code`. Never let a vendor SQLSTATE or reason string
   reach a client, and never parse one to make a decision here.

## How to behave when something does not work

Fleet standard, tracked by `obin-ai/handbook#68`. These are not style
preferences; each is a rule an agent broke at real cost.

1. **A gap in the tooling is a bug in the tooling** — never a reason to reach
   around it. Not as a "workaround", not "just this once", not "until the
   backend lands". `gen/` is generated: if a stub is wrong, fix the proto or
   the generator wiring, never hand-edit the output. If a gate is wrong, fix
   the gate.
2. **Never hack. Always provide the best fix, even when it spans repos.** The
   right fix living in `codefly-dev/core` or `codefly-dev/cli` is not a reason
   to work around it here — open the pull request there. If it genuinely
   cannot be fixed now, the deliverable is a precise issue against the owner
   plus an explicitly labelled stopgap, never an unlabelled one.
3. **Classify every change that makes something work**, in the PR body: a
   *fix* at the place that owns the behaviour, or a *hack*. A hack does not
   become a fix by working, by being small, by being local, or by the real fix
   belonging to someone else.
4. **Never hardcode what the system resolves.** `internal/config/config.go` is
   the entire configuration surface and every knob is an `SWH_*` variable — a
   listen address, a DSN, a credentials path, a query cap. If you are typing a
   connection string, a port, or a credential into Go source, a test, or a
   manifest, you are encoding something true only on your machine for the next
   ten minutes. A test that needs a backend uses `mem`, which needs nothing.
5. **Diagnose, do not pattern-match.** "It started working when I set X" is
   not a diagnosis — set X back and confirm it breaks. Do not trust an error
   message before checking it. Measured here: `SWH_BACKEND=mem` used to fail
   with `SWH_DATABASE (or SWH_DSN) is required for backend "mem"`, and the
   message was wrong — `mem` never reads `Database`. Supplying a placeholder
   database would have "worked" and taught the wrong thing; the fix was the
   guard.
6. **Say what you did not verify.** Unverified is not the same as working. No
   cloud backend can be exercised from this repo — there is no BigQuery,
   Snowflake, Redshift, or ClickHouse in CI and no fake for one — so a change
   touching a cloud path is a change whose behaviour is unproven, and the PR
   says so.

## Build and test

Derived from [`.github/workflows/ci.yml`](.github/workflows/ci.yml): those
three commands are the whole gate, so a green local run of all three is the
definition of done. CI pins the Go version as a literal in that workflow
(`go-version: "1.27.0"`) rather than reading `go.mod`, so bumping `go.mod`
alone moves your toolchain and not CI's — change both.

```sh
go build ./...
go vet ./...
go test -race ./...
```

Run the server locally against the zero-dependency backend — it needs no
database, no credential, and no network:

```sh
SWH_BACKEND=mem go run ./cmd/service-warehouse
```

The default `SWH_BACKEND=duckdb` refuses to start, by design: the DuckDB
backend is a registered skeleton whose constructor returns `Unsupported`
rather than degrading into something that serves wrong answers. A hard failure
at startup is the intended behaviour, not a bug to route around.

**Regenerating stubs is not a gate, and that is a trap.** CI never runs `buf`,
so nothing catches generated-file drift. The committed stubs reproduce
byte-identically only under `protoc-gen-go v1.36.11` and
`protoc-gen-go-grpc v1.6.1`; a newer plugin rewrites the version banner and
nothing else, so an unrelated PR silently carries a `gen/` diff. Neither
version is pinned anywhere in the repo (codefly-dev/service-warehouse#7). If
`buf generate` leaves you with a diff that is only those banner lines, revert
it — you changed your toolchain, not the contract.

## Where the depth is

Keep this file short; depth belongs in the file that owns the subject.

- [README.md](README.md) — the API table, the backend matrix, the full `SWH_*`
  env surface, the repo layout, and the distribution/SBOM status.
- `proto/codefly/warehouse/v0/warehouse.proto` — the contract of record.
- `internal/backend/backend.go` — the provider-agnostic interface every
  backend implements, and the portable type model.
- [`cmd/service-warehouse/distribution_test.go`](cmd/service-warehouse/distribution_test.go)
  — gates the claim that this repo ships no container image. It fails the day
  the repo gains a way to build or deploy one; that is the day image SBOM
  coverage is owed, and the gate is scoped to this repo only.
- A repeated multi-step procedure belongs in a `.claude/skills/<name>/SKILL.md`
  of its own, or in a nested `AGENTS.md` next to the code it describes —
  not in another paragraph here. There are none yet: this repo has three
  commands and no procedure long enough to earn one.
