# internal/repo

`internal/repo` manages each cluster's own GitHub repository — the
`Provisioner` that `apply`/`delete` use to create, read, update, and archive
it. It reaches GitHub entirely through the REST and Git Data APIs
(`github.com/google/go-github`), never a literal `git clone`/`git push`, so a
multi-file update lands as one atomic commit (create tree → create commit →
update ref). Every cluster gets its own **private** repo holding
`cluster.yaml`, `addons.yaml`, and `.state.yaml` — see "Cluster repo
contract" in the project's `CLAUDE.md` — and that cluster's Argo CD syncs
from it directly, so there is no shared/central repo for clusters to collide
on.

## Quick reference

| Name | Kind | File | Summary |
|---|---|---|---|
| [`Provisioner`](#provisioner) | Interface | provisioner.go | Interface every caller (orchestrator, Seed/Reconcile*) programs against |
| [`Checkout`](#checkout) | Struct | provisioner.go | Snapshot of a cluster repo's tracked files |
| [`Checkout.File`](#checkout) | Method | provisioner.go | Returns a tracked file's content and whether it exists |
| [`Option`](#option-and-withlogger) | Type | provisioner.go | Functional option for `NewProvisioner` |
| [`WithLogger`](#option-and-withlogger) | Function | provisioner.go | Overrides the default `slog.Default()` logger |
| [`NewProvisioner`](#newprovisioner) | Function | provisioner.go | Builds a `Provisioner` over a given `Clients` |
| [`Memory`](#memory) | Struct | memory.go | In-memory `Provisioner`, for tests |
| [`Memory.Archived`](#memory) | Method | memory.go | Test-only visibility into archive state |
| [`NewMemory`](#newmemory) | Function | memory.go | Builds an in-memory `Provisioner` |
| [`Clients`](#clients) | Struct | repo.go | Bundles go-github clients scoped to one org |
| [`NewClients`](#newclients) | Function | repo.go | Builds a real GitHub client |
| [`ClusterFile` / `AddonsFile` / `StateFile`](#constants) | Constants | repo.go | File paths inside every cluster repository |
| [`ReadFile`](#readfile) | Method | read.go | Reads one file off a repo's default branch (non-cluster repos) |
| [`Render`](#render) | Function | seed.go | Marshals `cluster.yaml`/`addons.yaml` YAML |
| [`Seed`](#seed) | Function | seed.go | Creates and seeds a cluster's repo on first `apply` |
| [`ReconcileAddons`](#reconcileaddons) | Function | seed.go | Brings `addons.yaml` in line with the resolved profile |
| [`ReconcileAppOfApps`](#reconcileappofapps) | Function | appofapps.go | Syncs app-of-apps addon Applications |

## repo.go

### `Clients`

??? abstract "Struct — bundles go-github clients scoped to one organization"
    ```go
    type Clients struct {
        org   string
        repo  repositoriesAPI
        git   gitAPI
        token string
    }
    ```
    - Organization is fixed at construction — operator configuration, not
      cluster desired state (the same way an AWS `Clients` fixes a region).

### `NewClients`

??? note "Signature"
    ```go
    func NewClients(org, baseURL, uploadURL, token string) (*Clients, error)
    ```
    - **Params** — `org`/`token` required (non-empty); `baseURL`/`uploadURL`
      configure a GitHub Enterprise instance and are left empty for
      github.com.
    - **Behavior** — errors if `org` or `token` is empty; if `baseURL` is
      non-empty, configures Enterprise URLs via `client.WithEnterpriseURLs`.

### `repositoriesAPI` / `gitAPI`

??? abstract "Interfaces — narrow slices of go-github's services actually called"
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
    - **Purpose** — lets tests fake the GitHub surface without a live client.

### `names`

??? abstract "Struct — derives GitHub resource names from the cluster ID"
    ```go
    type names struct{ spec core.ClusterSpec }

    func (n names) repoName() string { return "kubespin-" + n.spec.ID.String() }
    ```
    - **Invariant** — a second cluster cannot collide with an existing
      cluster's repo name.

### Constants

??? note "File paths and CODEOWNERS entry"
    ```go
    const (
        ClusterFile = "cluster.yaml"
        AddonsFile  = "addons.yaml"
        StateFile   = ".state.yaml"
    )
    ```
    - File paths inside every cluster repository. Roles must stay distinct —
      see "Cluster repo contract" in the project's `CLAUDE.md`.

    ```go
    const codeownersTeam = "@GitOpsHub/platform-team"
    ```
    - The `CODEOWNERS` entry seeded into every cluster repo. A single
      platform team is the correct default until there's a reason to vary it
      per-cluster.

### `notFound`

??? note "Unexported helper"
    ```go
    func notFound(resp *github.Response) bool
    ```
    - True when a go-github response is a 404 — the shared way `Exists`,
      `Clone`, `Archive`, and `ReadFile` distinguish "genuinely absent" from
      a real API error.

## provisioner.go

### `Provisioner`

??? abstract "Interface — the contract every caller programs against"
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
    - **Implementations** — `githubProvisioner` (real GitHub) and `Memory`
      (in-memory, for tests); both statically asserted against the
      interface:
      ```go
      var (
          _ Provisioner = (*githubProvisioner)(nil)
          _ Provisioner = (*Memory)(nil)
      )
      ```
    - **`Exists`** — reports whether the cluster's repository has been
      created.
    - **`Create`** — creates the repository, protects its default branch
      (requiring CODEOWNERS review), and seeds a `CODEOWNERS` file.
      Idempotent: creating an existing repo is a no-op. Branch protection is
      best-effort — a GitHub plan that doesn't offer it on private repos
      leaves the repo unprotected with a logged warning rather than failing
      the whole `apply`.
    - **`Clone`** — reads the repository's tracked files off its default
      branch into a `Checkout`.
    - **`Push`** — commits every file in `files` that differs from what
      `Checkout` read, as one atomic commit, and advances the default
      branch. Reports whether it made a commit; a `files` argument that
      already matches the checkout is a no-op API-call-wise, which is what
      makes a no-change `apply` produce zero commits.
    - **`Archive`** — archives the repository rather than deleting it, so
      its history survives teardown. Idempotent: archiving an
      already-archived or never-created repository is a no-op, so a retried
      teardown converges instead of failing.
    - **`RepoURL`** — the repository's clone URL, what `internal/argocd`'s
      root Application manifest points Argo CD's repo-server at. Must be a
      URL the repo-server can actually clone, not merely an identifier.
    - **`Credentials`** — the username/password pair Argo CD's repo-server
      should authenticate with. Since `Create` always makes the repo
      private, these must be registered as a `repo-creds` Secret alongside
      the root Application, or Argo CD's first reconcile fails with
      "authentication required" and never discovers a single addon.

### `Checkout`

??? abstract "Struct — a snapshot of a cluster repo's tracked files"
    ```go
    type Checkout struct {
        spec          core.ClusterSpec
        branch        string
        baseCommitSHA string
        files         map[string][]byte
    }

    func (c *Checkout) File(path string) ([]byte, bool)
    ```
    - **Scope** — read through the GitHub Contents API rather than a literal
      git clone; only ever needs `cluster.yaml`, `addons.yaml`,
      `.state.yaml`, the `apps/` directory, and `CODEOWNERS` — not the
      repository's full history or working tree.
    - **`baseCommitSHA`** — the branch tip `Checkout` was read from. `Push`
      builds its new commit on top of it, and `UpdateRef` fails — rather
      than silently discarding a concurrent change — if the branch has
      moved since.
    - **`File`** — returns a tracked file's content and whether it exists.

### `githubProvisioner`

??? abstract "Struct — the real Provisioner, backed by GitHub's REST and Git Data APIs"
    ```go
    type githubProvisioner struct {
        c      *Clients
        logger *slog.Logger
    }
    ```
    - Constructed via `NewProvisioner`.
    - **Invariant** — `AutoInit: true` on repo creation is deliberate: it
      gives the new repository an initial commit on its default branch, so
      `Clone`/`Push` always have a ref to build on instead of a special case
      for a completely empty repository.
    - **Invariant** — `protectBranch` treats a 403 whose message contains
      "Upgrade to GitHub" (`planLacksBranchProtection`) as an account-plan
      fact, not a misconfiguration or transient failure — it logs a warning
      and converges without protection rather than failing the apply. Any
      other error still fails.

### `Option` and `WithLogger`

??? note "Signature"
    ```go
    type Option func(*githubProvisioner)

    func WithLogger(logger *slog.Logger) Option
    ```
    - **Behavior** — functional option for `NewProvisioner`. Without
      `WithLogger`, a provisioner logs to `slog.Default()`. A nil logger
      passed to `WithLogger` is ignored.

### `NewProvisioner`

??? note "Signature"
    ```go
    func NewProvisioner(c *Clients, opts ...Option) Provisioner
    ```
    - **Behavior** — builds a `Provisioner` over the given `Clients`,
      defaulting its logger to `slog.Default()` unless overridden by
      `WithLogger`.

### Unexported helpers

??? note "planLacksBranchProtection, cloneBranch, listAppsDir, changedEntries"
    ```go
    func planLacksBranchProtection(err error) bool
    func (p *githubProvisioner) cloneBranch(...) (*Checkout, error)
    func (p *githubProvisioner) listAppsDir(...) (map[string][]byte, error)
    func changedEntries(checkout *Checkout, files map[string][]byte) []*github.TreeEntry
    ```
    - **`planLacksBranchProtection`** — matches a GitHub 403 whose message
      contains `"Upgrade to GitHub"`, distinguishing an account-plan
      limitation (converge without protection) from a real
      misconfiguration such as a token missing admin scope (fail loudly).
    - **`cloneBranch`** — the shared read path behind both `Clone` and
      `Create`'s CODEOWNERS seeding: reads the branch ref's SHA, then
      `ClusterFile`, `AddonsFile`, `StateFile`, `CODEOWNERS`, and every file
      currently under `argocd.AppsDir` (via `listAppsDir`), into a
      `Checkout`.
    - **`listAppsDir`** — lists the app-of-apps directory's current files so
      `cloneBranch` can track them in `Checkout` the same way it tracks the
      three fixed files. Without this, every addon Application under
      `argocd.AppsDir` would look "new" on every `Push` (`Checkout` would
      never have seen it), and a no-change `apply` would recommit the whole
      directory instead of making no commit at all.
    - **`changedEntries`** — returns tree entries only for files that differ
      from `checkout`, so `Push` never rewrites an unchanged file into the
      commit and an empty diff yields zero tree entries (and thus no commit
      at all).

## appofapps.go

### `ReconcileAppOfApps`

??? note "Signature"
    ```go
    func ReconcileAppOfApps(ctx context.Context, rp Provisioner, spec core.ClusterSpec, profile core.Profile) (bool, error)
    ```
    - **Behavior** — applies the same idempotent-diff discipline to the
      app-of-apps addon Applications under `argocd.AppsDir`: renders them
      via `argocd.RenderAddonApplications(profile)`, hashes the combined set
      with `hashApps` (order-independent of Go's randomized map iteration),
      and commits only when that hash differs from `.state.yaml`'s
      `AppsHash`, using commit message
      `"kubespin: sync app-of-apps Applications"`.
    - **Invariant** — never touches the root Application itself — that one
      is applied straight to the cluster via `internal/argocd.KubeApplier`,
      not committed to the repository it manages.

### `hashApps`

??? note "Unexported helper"
    ```go
    func hashApps(files map[string][]byte) string
    ```
    - SHA-256 content hash feeding `.state.yaml`'s `AppsHash`. Sorts paths
      before hashing so map iteration order never changes the result.

## memory.go

### `Memory`

??? abstract "Struct — in-memory Provisioner"
    ```go
    type Memory struct {
        mu       sync.Mutex
        repos    map[string]map[string][]byte // repo name -> path -> content
        archived map[string]bool
    }

    func (m *Memory) Archived(spec core.ClusterSpec) bool
    ```
    - **Purpose** — every component built on `Provisioner` — the
      orchestrator above all — is testable without a GitHub token or a live
      organization.
    - **Invariant** — honors the same commit-only-on-change contract the
      real provisioner does: `Push` is a no-op, reporting no change, when
      every given file already matches what `Clone` read.
    - **`RepoURL`** — returns an obviously-fake clone URL
      (`https://example.invalid/kubespin/<repo-name>`) since `Memory` has no
      real GitHub host — nothing in the codebase parses this string, only
      renders it into a manifest a real Argo CD would clone from.
    - **`Credentials`** — returns an obviously-fake credential pair
      (`"x-access-token", "fake-token"`) matching `RepoURL`'s fake host.
    - **`Archived`** — test-only visibility into archive state that
      `Archive` itself doesn't expose, mirroring how this codebase's other
      fakes expose their calls for assertions.

### `NewMemory`

??? note "Signature"
    ```go
    func NewMemory() *Memory
    ```
    - **Behavior** — builds an in-memory `Provisioner` with empty `repos`
      and `archived` maps.

## read.go

### `ReadFile`

??? note "Signature"
    ```go
    func (c *Clients) ReadFile(ctx context.Context, repoName, path string) ([]byte, bool, error)
    ```
    - **Scope** — *not* scoped to a cluster's own repository:
      `internal/catalog`'s repo-backed `Resolver` uses it to read profile
      definitions out of the platform-profiles repository — a different
      repo in the same org.
    - **Behavior** — reads one file off a repository's default branch in
      this package's configured organization.
    - **Invariant** — like `Checkout.File`, reports `ok=false` rather than
      an error when the file is absent, so a missing profile version is an
      ordinary "not found" rather than a surprise.

## seed.go

### `state`

??? abstract "Struct — the .state.yaml contract"
    ```go
    type state struct {
        AddonsHash string `yaml:"addonsHash"`
        AppsHash   string `yaml:"appsHash,omitempty"`
    }
    ```
    - Last-applied hashes used for idempotent diffing. Not user-authored.
    - **Invariant** — `AddonsHash` and `AppsHash` are reconciled
      independently (an `addons.yaml` change and an app-of-apps
      Application-manifest change don't happen on the same schedule), so
      every writer must read through `loadState` first and only overwrite
      its own field — never both — to avoid clobbering the other's hash.
    - Infra drift (`cluster.yaml`) is detected by
      `ClusterProvisioner.Reconcile` diffing the spec directly against live
      cloud state, so it needs no hash field here.

### `Render`

??? note "Signature"
    ```go
    func Render(spec core.ClusterSpec, profile core.Profile) (clusterYAML, addonsYAML []byte, err error)
    ```
    - **Behavior** — marshals `spec` to YAML for `cluster.yaml`, and
      `profile.Addons` (wrapped as `{addons: [...]}`) to YAML for
      `addons.yaml`.
    - **Invariant** — pure rendering; makes no GitHub calls.

### `Seed`

??? note "Signature"
    ```go
    func Seed(ctx context.Context, rp Provisioner, spec core.ClusterSpec, profile core.Profile) error
    ```
    - **Behavior** — creates and seeds a cluster's repository on its first
      `apply`: calls `rp.Create` (idempotent), then commits `cluster.yaml`,
      `addons.yaml`, and `.state.yaml` in one push via the internal
      `reconcile` helper with the message
      `"kubespin: seed cluster.yaml and addons.yaml"`.
    - **Invariant** — idempotent overall: a repository whose `addons.yaml`
      already matches the desired hash is left alone, so a resumed run that
      reaches this step again makes no second commit.

### `ReconcileAddons`

??? note "Signature"
    ```go
    func ReconcileAddons(ctx context.Context, rp Provisioner, spec core.ClusterSpec, profile core.Profile) (bool, error)
    ```
    - **Behavior** — brings a cluster's `addons.yaml` in line with its
      resolved profile, via the same internal `reconcile` helper as `Seed`,
      using commit message `"kubespin: update addons.yaml"`.
    - **Returns** — whether it made a commit; `apply` relies on this to
      prove zero git commits when nothing differs.
    - **Internally** — renders desired `cluster.yaml`/`addons.yaml`, hashes
      `addonsYAML`, clones the repo, loads `.state.yaml`, and only pushes
      (updating `AddonsHash`) if the hash differs from what's recorded.

### Unexported helpers

??? note "hashAddons, loadState"
    ```go
    func hashAddons(b []byte) string
    func loadState(checkout *Checkout) (state, error)
    ```
    - **`hashAddons`** — SHA-256 content hash feeding `.state.yaml`'s
      `AddonsHash`.
    - **`loadState`** — reads `.state.yaml` off a `Checkout`, or returns a
      zero `state` if none exists yet (first apply).
