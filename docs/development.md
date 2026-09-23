# Development guide

## Toolchain

Go 1.26, pinned in `go.mod`, `.tool-versions`, and CI. `golangci-lint` is the
only tool not vendored through `go.mod`:

```bash
make bootstrap
```

Then the default target — lint, test, build:

```bash
make
```

| Target | What it does |
|---|---|
| `make build` | Builds `bin/kubespin`, then installs it onto `PATH` |
| `make install` | Copies an already-built `bin/kubespin` to `INSTALL_DIR` |
| `make test` | Unit tests with `-race -cover` |
| `make integration` | Adds `-tags=integration`; needs a reachable Postgres (`KUBESPIN_POSTGRES_TEST_DSN`) |
| `make lint` | `golangci-lint run` |
| `make docs` | Regenerates `docs/cli` from the command tree |
| `make changelog VERSION=vX.Y.Z` | Previews a `CHANGELOG.md` section locally; the release workflow runs this for real — see [Releases](#releases) |
| `make fmt` | `go fmt` plus `go mod tidy` |
| `make spot [aws] [gcp] [azure]` | Spins up a `--spot` dev cluster on the named clouds (all three by default) in parallel. `make autopilot` and `make destroy[-spot\|-autopilot]` take the same cloud goals. See [Low-cost dev clusters: make spot](low-cost-dev-clusters.md#make-spot-all-three-clouds-at-once) |

`INSTALL_DIR` defaults to `~/.local/bin`, which is on `PATH` on macOS and
writable without `sudo`. Point it elsewhere with
`make build INSTALL_DIR=/usr/local/bin`. The install step is skipped when `CI`
is set, so a build on a runner never writes outside the repository.

## Layout

```
cmd/kubespin/              binary entrypoint; wires signals and exit codes
internal/cli/              cobra command tree and configuration resolution
internal/core/             shared domain types
internal/auth/             operator cloud auth behind login/status/logout
internal/registry/         cluster registry client (Postgres), lease primitive, in-memory implementation
internal/orchestrator/     sequences one cluster's provisioning through the phases
internal/provisioner/      cloud-facing interfaces; one subpackage per cloud
internal/repo/             cluster repositories over GitHub
internal/catalog/          size resolution: small/medium/large, fully builtin
internal/argocd/           app-of-apps rendering, access-mode templating, install
internal/kubeconfig/       operator kubeconfig update after apply (shells out to aws/gcloud/az)
internal/tools/            build-time tools (docs generation, changelog generation)
internal/version/          build metadata stamped in via -ldflags
docs/cli/                  generated — never edit by hand
```

**`internal/core` imports nothing from `internal/`.** No cloud SDKs, no I/O, no
other internal package. Everything imports core, so keeping it a leaf is what
prevents the import cycles that otherwise appear as the tree grows. If you find
yourself wanting to import something into core, the type probably belongs
elsewhere.

## Error conventions

Sentinel errors, wrapped, matched with `errors.Is`/`errors.As` — never by string
comparison. Each package exposes its own: `core.ErrInvalidSpec`,
`core.ErrInvalidTransition`, `cli.ErrConfig`, `registry.ErrNotFound`,
`registry.ErrAlreadyExists`, `orchestrator.ErrBusy`.

`wrapcheck` is enabled, so any error crossing a package boundary must be
wrapped with context.

Validation functions return **all** problems at once via `errors.Join`. Fixing a
spec one error per run is miserable; see `ClusterSpec.Validate` in
[internal/core/cluster.go](https://github.com/GitOpsHub/kubespin/blob/main/internal/core/cluster.go).

## Testing

Unit tests run without credentials and without network. Anything needing a
real Postgres goes behind the build tag:

```go
//go:build integration
```

Those run via `make integration` and nightly in CI, never on a pull request.

To run the registry integration tests locally:

```bash
docker run -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:16
```

```bash
KUBESPIN_POSTGRES_TEST_DSN=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable make integration
```

Note that `make test` alone reports low coverage for `internal/registry`: the
Postgres implementation (`internal/registry/postgres.go`) is only reachable
under the integration tag. The number is not a measure of how well that
package is tested.

### The registry contract

`internal/registry/contract_test.go` is the behaviour every implementation must
satisfy, written once and run against both the in-memory registry and a real
Postgres (`internal/registry/postgres_integration_test.go`). Add new registry
behaviour there rather than in an implementation's own test file — a guarantee
proven against only one implementation is not a guarantee.

The in-memory registry deliberately enforces the same conditions as Postgres.
It is not a simplified stand-in: if it accepted writes Postgres would reject,
the orchestrator tests built on it would pass while production failed.

The load-bearing case is `concurrent acquisition elects exactly one holder`,
which races sixteen goroutines at a single lease. It runs against real Postgres
too, because that is the only place the conditional `UPDATE` expression itself
is under test. A sequential simulation of this would pass against a broken
lock.

The phase transition test asserts the **full cartesian product** of phases
against a hand-written table rather than a rule shared with the implementation.
That is what caught the original `Phase.Valid()` bug, where validity was derived
from having a successor and `ready` therefore reported itself invalid.

## Changing the CLI

`docs/cli` is generated by `internal/tools/docsgen`. After adding or changing a
command or flag:

```bash
make docs
```

CI regenerates and fails on any difference, so a stale reference blocks the
merge rather than misleading a reader. The generator disables cobra's
auto-generated date tag and blanks the version string — both would otherwise
churn the output on every run.

A command's `Example` block is the *only* place the reference gets examples
from, so it has to be runnable as written: every flag the command actually
requires, spelled out. `apply` and `delete` validate a whole `ClusterSpec`,
which means an example missing `--cluster-id` fails before doing anything; every
registry-touching command needs the registry DSN, which has no default and —
deliberately — no flag, so it must come from `KUBESPIN_REGISTRY_DSN` (or a
`.env` file); and every repository-touching command needs `--github-org`. Examples are written
as plain `kubespin`, which `make build` puts on your `PATH`, so they can be
pasted straight into a terminal from any directory.

`cmd/kubespin/main.go` exits `1` on any error from `Execute` and `0`
otherwise; there are no other exit codes.

Global flags belong in `registerGlobalFlags`, which takes a `*pflag.FlagSet`
rather than a command so the precedence tests can exercise it without building
the whole tree. If you add one, extend `Config` and its validation, and remember
the precedence contract: **flags > `KUBESPIN_*` env > config file > defaults**.
`TestLoadConfig_Precedence` covers every pairing, including the boolean case that
viper's flag binding is easiest to get wrong on.

## CI

`.github/workflows/ci.yml` runs on every pull request:

- **lint** — golangci-lint, plus checks that `go mod tidy` and `make docs` were
  run and committed
- **test** — race-enabled unit tests with coverage
- **build** — linux/amd64, linux/arm64, darwin/arm64

`.github/workflows/integration.yml` runs nightly against a Postgres service
container, and against real AWS via OIDC once `AWS_INTEGRATION_ROLE_ARN` is
configured. There are no long-lived cloud credentials in this repository — the
same identity discipline the product enforces on clusters.

## Releases

kubespin uses [Semantic Versioning](https://semver.org/): tags are
`vMAJOR.MINOR.PATCH`, and `kubespin --version` reports whatever tag `git
describe` resolves to at build time (`internal/version`, stamped in by the
`Makefile`'s `LDFLAGS`).

Releases are cut automatically — **there is no manual tagging step.**
`.github/workflows/release.yml` runs after `.github/workflows/ci.yml`
succeeds on `main`. It classifies every [Conventional
Commits](https://www.conventionalcommits.org/)-style commit subject since the
last tag:

| Commit prefix | Bump |
|---|---|
| `feat!:`, `fix!:`, or a `BREAKING CHANGE:` footer | major |
| `feat:` | minor |
| `fix:`, `perf:` | patch |
| anything else (`chore:`, `docs:`, `refactor:`, plain messages, …) | none |

If nothing warrants a release, the workflow is a no-op. Otherwise it:

1. computes the next tag and writes a dated `CHANGELOG.md` section for it,
   grouped into Added/Fixed/Changed (`internal/tools/changeloggen`, the same
   generated-not-hand-written pattern as `internal/tools/docsgen`)
2. commits that as `chore(release): vX.Y.Z`, tags it, and pushes both to `main`
3. builds `linux/amd64`, `linux/arm64`, and `darwin/arm64` binaries
4. publishes a GitHub Release with the new changelog section as its notes and
   the binaries attached

`CHANGELOG.md` is generated per version and should never be hand-edited past
entries — write commit subjects that read well as changelog bullets instead.
To preview what a release would contain without cutting one:

```bash
go run ./internal/tools/changeloggen next     # next tag, or exit 1 if none is due
make changelog VERSION=v1.2.3                 # preview the section it would render
```
