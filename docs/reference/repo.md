# internal/repo

`internal/repo` manages each cluster's own GitHub repository — the
`Provisioner` that `apply`/`delete` use to create, read, update, and archive
it. It reaches GitHub entirely through the REST and Git Data APIs
(`github.com/google/go-github`), never a literal `git clone`/`git push`: that
keeps a "clone" to just the files this package cares about, and lets a
multi-file update land as one atomic commit (create tree → create commit →
update ref) instead of one REST call per file. Every cluster gets its own
**private** repo because the repo *is* the cluster's desired-state contract —
`cluster.yaml`, `addons.yaml`, and `.state.yaml`, whose distinct roles are the
"Cluster repo contract" in the project's `CLAUDE.md` — and Argo CD in that
cluster syncs from it directly, so there is no shared/central repo for one
cluster's changes to collide with another's.

## Types

### `Provisioner` (interface, `provisioner.go`)

The interface every caller (orchestrator, `Seed`/`ReconcileAddons`/
`ReconcileAppOfApps`) programs against. Two implementations exist:
`githubProvisioner` (real GitHub) and `Memory` (in-memory, for tests).

```go
type Provisioner interface {
	Exists(ctx context.Context, spec core.ClusterSpec) (bool, error)
	Create(ctx context.Context, spec core.ClusterSpec) error
	Clone(ctx context.Context, spec core.ClusterSpec) (*Checkout, error)
	Push(ctx context.Context, checkout *Checkout, files map[string][]byte, message string) (bool, error)
	Archive(ctx context.Context, spec core.ClusterSpec) error
	RepoURL(ctx context.Context, spec core.ClusterSpec) (string, error)
	Credentials() (username, password string)
}
```

- **`Exists`** — reports whether the cluster's repository has been created.
- **`Create`** — creates the repository, protects its default branch
  (requiring CODEOWNERS review), and seeds a `CODEOWNERS` file. Idempotent:
  creating an existing repo is a no-op. Branch protection is best-effort — a
  GitHub plan that doesn't offer it on private repos leaves the repo
  unprotected with a logged warning rather than failing the whole `apply`.
- **`Clone`** — reads the repository's tracked files off its default branch
  into a `Checkout`.
- **`Push`** — commits every file in `files` that differs from what `Checkout`
  read, as one atomic commit, and advances the default branch. Reports
  whether it made a commit; a `files` argument that already matches the
  checkout is a no-op API-call-wise, which is what makes a no-change `apply`
  produce zero commits.
- **`Archive`** — archives the repository rather than deleting it, so its
  history survives teardown. Idempotent: archiving an already-archived or
  never-created repository is a no-op, so a retried teardown converges
  instead of failing.
- **`RepoURL`** — the repository's clone URL, what `internal/argocd`'s root
  Application manifest points Argo CD's repo-server at. Must be a URL the
  repo-server can actually clone, not merely an identifier.
- **`Credentials`** — the username/password pair Argo CD's repo-server should
  authenticate with. Since `Create` always makes the repo private, these must
  be registered as a `repo-creds` Secret alongside the root Application, or
  Argo CD's first reconcile fails with "authentication required" and never
  discovers a single addon.

Both implementations are statically asserted against the interface in
`provisioner.go`:

```go
var (
	_ Provisioner = (*githubProvisioner)(nil)
	_ Provisioner = (*Memory)(nil)
)
```

### `Checkout` (struct, `provisioner.go`)

A snapshot of a cluster repo's tracked files, read through the GitHub
Contents API rather than a literal git clone — this package only ever needs
`cluster.yaml`, `addons.yaml`, `.state.yaml`, the `apps/` directory, and
`CODEOWNERS`, not the repository's full history or working tree.

```go
type Checkout struct {
	spec          core.ClusterSpec
	branch        string
	baseCommitSHA string
	files         map[string][]byte
}

func (c *Checkout) File(path string) ([]byte, bool)
```

- `baseCommitSHA` is the branch tip `Checkout` was read from. `Push` builds
  its new commit on top of it, and `UpdateRef` fails — rather than silently
  discarding a concurrent change — if the branch has moved since.
- `File` returns a tracked file's content and whether it exists.

### `githubProvisioner` (struct, `provisioner.go`)

The real `Provisioner`, backed by GitHub's REST and Git Data APIs.

```go
type githubProvisioner struct {
	c      *Clients
	logger *slog.Logger
}
```

Constructed via `NewProvisioner`. `AutoInit: true` on repo creation is
deliberate: it gives the new repository an initial commit on its default
branch, so `Clone`/`Push` always have a ref to build on instead of a special
case for a completely empty repository.

Invariant: `protectBranch` treats a 403 whose message contains "Upgrade to
GitHub" (`planLacksBranchProtection`) as an account-plan fact, not a
misconfiguration or transient failure — it logs a warning and converges
without protection rather than failing the apply. Any other error still
fails.

### `Option` / `WithLogger` (`provisioner.go`)

```go
type Option func(*githubProvisioner)

func WithLogger(logger *slog.Logger) Option
```

Functional option for `NewProvisioner`. Without `WithLogger`, a provisioner
logs to `slog.Default()`. A nil logger passed to `WithLogger` is ignored.

### `Memory` (struct, `memory.go`)

An in-memory `Provisioner`, so every component built on `Provisioner` — the
orchestrator above all — is testable without a GitHub token or a live
organization. It honors the same commit-only-on-change contract the real
provisioner does: `Push` is a no-op, reporting no change, when every given
file already matches what `Clone` read.

```go
type Memory struct {
	mu       sync.Mutex
	repos    map[string]map[string][]byte // repo name -> path -> content
	archived map[string]bool
}

func NewMemory() *Memory
func (m *Memory) Archived(spec core.ClusterSpec) bool
```

- `RepoURL` returns an obviously-fake clone URL
  (`https://example.invalid/kubespin/<repo-name>`) since `Memory` has no real
  GitHub host — nothing in the codebase parses this string, only renders it
  into a manifest a real Argo CD would clone from.
- `Credentials` returns an obviously-fake credential pair
  (`"x-access-token", "fake-token"`) matching `RepoURL`'s fake host.
- `Archived` is test-only visibility into archive state that `Archive` itself
  doesn't expose, mirroring how this codebase's other fakes expose their
  calls for assertions.

### `Clients` (struct, `repo.go`)

Bundles the go-github clients this package uses, scoped to one organization.
The organization is fixed at construction — the same way an AWS `Clients`
fixes a region — because it's operator configuration, not cluster desired
state.

```go
type Clients struct {
	org   string
	repo  repositoriesAPI
	git   gitAPI
	token string
}

func NewClients(org, baseURL, uploadURL, token string) (*Clients, error)
func (c *Clients) ReadFile(ctx context.Context, repoName, path string) ([]byte, bool, error)
```

`NewClients` requires a non-empty `org` and `token`; `baseURL`/`uploadURL`
configure a GitHub Enterprise instance and are left empty for github.com.

`ReadFile` (in `read.go`) is *not* scoped to a cluster's own repository:
`internal/catalog`'s repo-backed `Resolver` uses it to read profile
definitions out of the platform-profiles repository — a different repo in the
same org. Like `Checkout.File`, it reports `ok=false` rather than an error
when the file is absent, so a missing profile version is an ordinary
"not found" rather than a surprise.

### `repositoriesAPI` / `gitAPI` (interfaces, `repo.go`)

The narrow slices of go-github's `Repositories` and `Git` services this
package actually calls, so tests can fake them without a live GitHub client.

```go
type repositoriesAPI interface {
	Get(ctx context.Context, owner, repo string) (*github.Repository, *github.Response, error)
	Create(ctx context.Context, org string, repo *github.Repository) (*github.Repository, *github.Response, error)
	GetContents(ctx context.Context, owner, repo, path string, opts *github.RepositoryContentGetOptions) (*github.RepositoryContent, []*github.RepositoryContent, *github.Response, error)
	UpdateBranchProtection(ctx context.Context, owner, repo, branch string, preq *github.ProtectionRequest) (*github.Protection, *github.Response, error)
	Edit(ctx context.Context, owner, repo string, repository *github.Repository) (*github.Repository, *github.Response, error)
}

type gitAPI interface {
	GetRef(ctx context.Context, owner, repo, ref string) (*github.Reference, *github.Response, error)
	GetCommit(ctx context.Context, owner, repo, sha string) (*github.Commit, *github.Response, error)
	CreateTree(ctx context.Context, owner, repo, baseTree string, entries []*github.TreeEntry) (*github.Tree, *github.Response, error)
	CreateCommit(ctx context.Context, owner, repo string, commit github.Commit, opts *github.CreateCommitOptions) (*github.Commit, *github.Response, error)
	UpdateRef(ctx context.Context, owner, repo, ref string, updateRef github.UpdateRef) (*github.Reference, *github.Response, error)
}
```

### `names` (struct, `repo.go`)

Derives every GitHub resource name from the cluster ID, so a cluster's
repository is identifiable and a second cluster cannot collide with it.

```go
type names struct{ spec core.ClusterSpec }

func (n names) repoName() string { return "kubespin-" + n.spec.ID.String() }
```

### `state` (struct, `seed.go`)

The `.state.yaml` contract: the last-applied hashes used for idempotent
diffing. Not user-authored.

```go
type state struct {
	AddonsHash string `yaml:"addonsHash"`
	AppsHash   string `yaml:"appsHash,omitempty"`
}
```

`AddonsHash` and `AppsHash` are reconciled independently (an `addons.yaml`
change and an app-of-apps Application-manifest change don't happen on the
same schedule), so every writer must read through `loadState` first and only
overwrite its own field — never both — to avoid clobbering the other's hash.
Infra drift (`cluster.yaml`) is detected by `ClusterProvisioner.Reconcile`
diffing the spec directly against live cloud state, so it needs no hash field
here.

## Constants

```go
const (
	ClusterFile = "cluster.yaml"
	AddonsFile  = "addons.yaml"
	StateFile   = ".state.yaml"
)
```

File paths inside every cluster repository (`repo.go`). Their roles must stay
distinct — see the "Cluster repo contract" in the project's `CLAUDE.md`.

```go
const codeownersTeam = "@GitOpsHub/platform-team"
```

The `CODEOWNERS` entry seeded into every cluster repo (`repo.go`). A single
platform team is the correct default until there's a reason to vary it
per-cluster.

## Functions

### `NewProvisioner`

```go
func NewProvisioner(c *Clients, opts ...Option) Provisioner
```

Builds a `Provisioner` over the given `Clients`, defaulting its logger to
`slog.Default()` unless overridden by `WithLogger`. Defined in
`provisioner.go`.

### `NewClients`

```go
func NewClients(org, baseURL, uploadURL, token string) (*Clients, error)
```

Builds a real GitHub client (`repo.go`). Errors if `org` or `token` is empty.
If `baseURL` is non-empty, configures GitHub Enterprise URLs via
`client.WithEnterpriseURLs`.

### `NewMemory`

```go
func NewMemory() *Memory
```

Builds an in-memory `Provisioner` (`memory.go`) with empty `repos` and
`archived` maps.

### `Render`

```go
func Render(spec core.ClusterSpec, profile core.Profile) (clusterYAML, addonsYAML []byte, err error)
```

Defined in `seed.go`. Marshals `spec` to YAML for `cluster.yaml`, and
`profile.Addons` (wrapped as `{addons: [...]}`) to YAML for `addons.yaml`.
Pure rendering — makes no GitHub calls.

### `Seed`

```go
func Seed(ctx context.Context, rp Provisioner, spec core.ClusterSpec, profile core.Profile) error
```

Defined in `seed.go`. Creates and seeds a cluster's repository on its first
`apply`: calls `rp.Create` (idempotent), then commits `cluster.yaml`,
`addons.yaml`, and `.state.yaml` in one push via the internal `reconcile`
helper with the message `"kubespin: seed cluster.yaml and addons.yaml"`.
Idempotent overall — a repository whose `addons.yaml` already matches the
desired hash is left alone, so a resumed run that reaches this step again
makes no second commit.

### `ReconcileAddons`

```go
func ReconcileAddons(ctx context.Context, rp Provisioner, spec core.ClusterSpec, profile core.Profile) (bool, error)
```

Defined in `seed.go`. Brings a cluster's `addons.yaml` in line with its
resolved profile, via the same internal `reconcile` helper as `Seed`, using
commit message `"kubespin: update addons.yaml"`. Returns whether it made a
commit — `apply` relies on this to prove zero git commits when nothing
differs. Internally: renders desired `cluster.yaml`/`addons.yaml`, hashes
`addonsYAML`, clones the repo, loads `.state.yaml`, and only pushes (updating
`AddonsHash`) if the hash differs from what's recorded.

### `ReconcileAppOfApps`

```go
func ReconcileAppOfApps(ctx context.Context, rp Provisioner, spec core.ClusterSpec, profile core.Profile) (bool, error)
```

Defined in `appofapps.go`. Applies the same idempotent-diff discipline to the
app-of-apps addon Applications under `argocd.AppsDir`: renders them via
`argocd.RenderAddonApplications(profile)`, hashes the combined set with
`hashApps` (order-independent of Go's randomized map iteration), and commits
only when that hash differs from `.state.yaml`'s `AppsHash`, using commit
message `"kubespin: sync app-of-apps Applications"`. It never touches the
root Application itself — that one is applied straight to the cluster via
`internal/argocd.KubeApplier`, not committed to the repository it manages.

### `(*Clients) ReadFile`

```go
func (c *Clients) ReadFile(ctx context.Context, repoName, path string) ([]byte, bool, error)
```

Defined in `read.go`. Reads one file off a repository's default branch in
this package's configured organization, for repos other than a cluster's own
(see `Clients` above). Returns `ok=false`, not an error, when the file
doesn't exist.

## Unexported helpers worth knowing

- `notFound(resp *github.Response) bool` (`repo.go`) — true when a go-github
  response is a 404, the shared way `Exists`, `Clone`, `Archive`, and
  `ReadFile` distinguish "genuinely absent" from a real API error.
- `planLacksBranchProtection(err error) bool` (`provisioner.go`) — matches a
  GitHub 403 whose message contains `"Upgrade to GitHub"`, distinguishing an
  account-plan limitation (converge without protection) from a real
  misconfiguration such as a token missing admin scope (fail loudly).
- `(*githubProvisioner) cloneBranch` (`provisioner.go`) — the shared read path
  behind both `Clone` and `Create`'s CODEOWNERS seeding: reads the branch
  ref's SHA, then `ClusterFile`, `AddonsFile`, `StateFile`, `CODEOWNERS`, and
  every file currently under `argocd.AppsDir` (via `listAppsDir`), into a
  `Checkout`.
- `(*githubProvisioner) listAppsDir` (`provisioner.go`) — lists the
  app-of-apps directory's current files so `cloneBranch` can track them in
  `Checkout` the same way it tracks the three fixed files. Without this,
  every addon Application under `argocd.AppsDir` would look "new" on every
  `Push` (`Checkout` would never have seen it), and a no-change `apply` would
  recommit the whole directory instead of making no commit at all.
- `changedEntries(checkout *Checkout, files map[string][]byte) []*github.TreeEntry`
  (`provisioner.go`) — returns tree entries only for files that differ from
  `checkout`, so `Push` never rewrites an unchanged file into the commit and
  an empty diff yields zero tree entries (and thus no commit at all).
- `hashAddons([]byte) string` / `hashApps(map[string][]byte) string`
  (`seed.go`, `appofapps.go`) — SHA-256 content hashes feeding `.state.yaml`'s
  `AddonsHash`/`AppsHash`. `hashApps` sorts paths before hashing so map
  iteration order never changes the result.
- `loadState(checkout *Checkout) (state, error)` (`seed.go`) — reads
  `.state.yaml` off a `Checkout`, or returns a zero `state` if none exists
  yet (first apply).
