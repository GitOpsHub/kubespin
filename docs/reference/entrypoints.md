# Entrypoints and tooling

This page covers the code under `cmd/` — the binaries kubespin actually
ships — plus the internal packages that support the build: the docs and
changelog generators and the version banner. Each is deliberately thin; the
real logic lives in `internal/`.

## Quick reference

| Component | Role | Summary |
|---|---|---|
| [`cmd/kubespin`](#cmdkubespin) | Operator-facing CLI binary | Wires up a cancellable context and delegates entirely to `internal/cli.NewRootCommand()`. |
| [`internal/tools/docsgen`](#internaltoolsdocsgen) | `make docs` generator | Regenerates `docs/cli/*.md` from the live cobra command tree so the CLI reference cannot drift. |
| [`internal/tools/changeloggen`](#internaltoolschangeloggen) | `make changelog` generator | Derives next SemVer tag and `CHANGELOG.md` sections from Conventional Commits since the last tag. |
| [`internal/version`](#internalversion) | Build metadata package | Carries `Version`/`Commit`/`BuildDate` stamped in via `-ldflags`, and renders the `--version` banner. |

## cmd/kubespin

The operator-facing CLI binary. `main()` in
[cmd/kubespin/main.go](https://github.com/GitOpsHub/kubespin/blob/main/cmd/kubespin/main.go)
does nothing but wire up a cancellable context and delegate to
`internal/cli.NewRootCommand()` — all command definitions, flags, and
business logic live there, not in `cmd/kubespin`. The context is built with
`signal.NotifyContext` against `os.Interrupt` and `syscall.SIGTERM` so that
an interrupted `apply`/`delete` (which can run for tens of minutes) can
release its registry lease instead of leaving a cluster wedged mid-phase.

<details>
<summary>Signature: `func main()`</summary>

```go
func main()
```

- **Behavior:** No parameters, no return. Builds the interrupt-cancellable
  context, calls `cli.NewRootCommand().ExecuteContext(ctx)`, and on error
  prints `"kubespin: %v\n"` to stderr and exits with status 1.

</details>

## internal/tools/docsgen

Backs `make docs`. Regenerates `docs/cli/*.md` from the live cobra command
tree defined in `internal/cli`, so the CLI reference cannot drift from the
actual flags/commands — per the package comment in
[internal/tools/docsgen/main.go](https://github.com/GitOpsHub/kubespin/blob/main/internal/tools/docsgen/main.go),
CI regenerates it and fails if the result differs from what's committed.

<details>
<summary>Signature: `func main()`</summary>

```go
func main()
```

- **Behavior:** Calls `run()`; on error prints `"docsgen: %v\n"` to
  stderr and exits 1.

</details>

<details>
<summary>Signature: `func run() error`</summary>

```go
func run() error
```

- **Behavior:** Creates `docs/cli` (`outputDir`) if needed, builds
  `cli.NewRootCommand()`, calls `disableAutoGenTag` on it (so cobra
  doesn't stamp the current date into generated files, which would make
  every rebuild look dirty), clears `root.Version` (so the build commit
  embedded in the version string doesn't churn the root page on every
  rebuild), runs `doc.GenMarkdownTree(root, outputDir)` (cobra's own
  doc generator), then calls `polishAll(outputDir)`.

</details>

<details>
<summary>Signature: `func polishAll(dir string) error`</summary>

```go
func polishAll(dir string) error
```

- **Behavior:** Globs `dir/*.md`, and for each file rewrites its
  content via `polish` and writes it back.

</details>

<details>
<summary>Signature: `func polish(md string) string`</summary>

```go
func polish(md string) string
```

- **Behavior:** Rewrites cobra's generated markdown into the shape the
  rest of the hand-written docs use, per line, tracking whether it is
  currently inside a fenced code block (so headings inside shell
  comments aren't rewritten):
    - Cobra's page title is emitted as `##`; promoted to `#` since the
      site derives the page title and TOC from the H1, and used to emit
      a `title:` frontmatter block ahead of it for the Docusaurus
      sidebar.
    - `### ` section headings are demoted to `##`, and `SEE ALSO` is
      rewritten to sentence case `See also`.
    - Code fences (` ``` `) get a language tag from `fenceLanguage`,
      since cobra emits untagged fences and nothing gets
      syntax-highlighted otherwise.
    - "See also" list entries (`* [`) have tab characters stripped,
      since a tab between the link and its description renders as a
      ragged gap.

</details>

<details>
<summary>Signature: `func fenceLanguage(section string) string`</summary>

```go
func fenceLanguage(section string) string
```

- **Behavior:** Returns `"bash"` if the current section is
  `"Examples"`, else `"text"` — only the examples are shell; usage
  synopsis and option lists are output shapes that would be
  mis-highlighted as shell (e.g. `[flags]`).

</details>

<details>
<summary>Signature: `func disableAutoGenTag(cmd *cobra.Command)`</summary>

```go
func disableAutoGenTag(cmd *cobra.Command)
```

- **Behavior:** Recursively sets `cmd.DisableAutoGenTag = true` on the
  command and all of its children.

</details>

## internal/tools/changeloggen

Backs `make changelog`. Derives the next SemVer tag and `CHANGELOG.md` release entry from Conventional Commit subjects (`git log`) since the last tag, per the package comment in [internal/tools/changeloggen/main.go](https://github.com/GitOpsHub/kubespin/blob/main/internal/tools/changeloggen/main.go).

It supports three subcommands:
- `next` — computes and prints the next `vX.Y.Z` tag based on commit types since the last tag, or exits 1 if no commits warrant a release.
- `render -version vX.Y.Z` — parses commits since the last tag, formats them into categorized groups (`Added`, `Fixed`, `Changed`), and prepends a dated section directly beneath `## [Unreleased]` in `CHANGELOG.md`.
- `extract vX.Y.Z` — extracts the changelog body for the specified version from `CHANGELOG.md` to stdout, suitable for populating a GitHub Release body.

<details>
<summary>Classification and SemVer Bump Rules</summary>

- **Major bump (`v(X+1).0.0`)**: any commit with `!` in the type prefix (e.g. `feat!:`) or containing `BREAKING CHANGE` in its commit body.
- **Minor bump (`vX.(Y+1).0`)**: commits with prefix `feat:`. Categorized under `### Added`.
- **Patch bump (`vX.Y.(Z+1)`)**: commits with prefix `fix:` or `perf:`. `fix:` commits are categorized under `### Fixed`.
- **Changed bucket**: other commit types (e.g. `docs:`, `refactor:`, `chore:`) or commits not matching the conventional commit pattern are categorized under `### Changed` and do not trigger a version bump on their own.
- **Initial tag**: if no prior git tag is reachable in repository history, the next version defaults to `v0.1.0`.

</details>

## internal/version

A single-file package carrying build metadata stamped in via `-ldflags` at
build time (see the Makefile). Per the package comment in
[internal/version/version.go](https://github.com/GitOpsHub/kubespin/blob/main/internal/version/version.go),
defaults keep `go run` usable without a full `make build`.

<details>
<summary>Signature: `var (Version, Commit, BuildDate string)`</summary>

```go
var (
    Version   = "dev"
    Commit    = "unknown"
    BuildDate = "unknown"
)
```

- **Behavior:** Package-level variables, overridden at build time via
  linker flags.

</details>

<details>
<summary>Signature: `func String() string`</summary>

```go
func String() string
```

- **Behavior:** Renders the version banner shown by
  `kubespin --version`:
  `"kubespin %s (commit %s, built %s, %s/%s, %s)"`, formatted with
  `Version`, `Commit`, `BuildDate`, `runtime.GOOS`, `runtime.GOARCH`,
  and `runtime.Version()`.

</details>
