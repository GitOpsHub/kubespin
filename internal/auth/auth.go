// Package auth authenticates the operator to every cloud kubespin talks to.
//
// It exists because kubespin shells out to the same CLIs (aws, gcloud, az) an
// operator would use by hand, rather than keeping its own credential store —
// login sessions live wherever those CLIs already put them
// (~/.aws/sso/cache, ~/.config/gcloud, ~/.azure), so kubespin has nothing
// extra to secure or expire.
package auth

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// StatusDetail is what a Provider reports about its own session, beyond the
// plain authenticated/not-authenticated bit.
type StatusDetail struct {
	// Message is a short human-readable summary, e.g. "4 accounts reachable"
	// or "logged in as you@org.com".
	Message string

	// ExpiresAt is when the current session expires, if the provider can
	// determine it. Nil means unknown or not applicable — not "never expires".
	ExpiresAt *time.Time
}

// Provider is one cloud's authentication surface. Every provider is reached
// through this interface so the orchestrator below, and every command that
// needs credentials, never branches on which cloud it's talking to — adding
// Oracle Cloud or any other provider later is a new file implementing this,
// not a change to how login/status/logout work.
type Provider interface {
	// Name identifies the provider for --only, status output, and error
	// messages, e.g. "aws".
	Name() string

	// IsAuthenticated reports whether the provider's session currently looks
	// valid. It makes a real call (not just "does a token file exist") so a
	// stale or revoked session is reported accurately. A false result is not
	// itself an error — err is reserved for failures unrelated to auth state,
	// such as the provider's CLI not being installed.
	IsAuthenticated(ctx context.Context) (bool, StatusDetail, error)

	// Login authenticates interactively (typically opening a browser). It is
	// expected to block until the flow completes or fails.
	Login(ctx context.Context) error

	// Logout clears the provider's cached session.
	Logout(ctx context.Context) error
}

// Result is the outcome of running one operation against one provider. Every
// orchestrator function below returns a slice of these — one per provider,
// in the same order the providers were given — rather than failing the whole
// batch on the first error, so a login run against three clouds still reports
// all three even if one fails.
type Result struct {
	Provider      string
	Authenticated bool
	Status        StatusDetail
	Err           error
}

// Registry holds every configured provider, in the order status/login/logout
// report them.
type Registry struct {
	providers []Provider
}

// NewRegistry builds a registry over the given providers, in report order.
func NewRegistry(providers ...Provider) *Registry {
	return &Registry{providers: providers}
}

// Select returns the providers named (case-insensitively), in registry
// order, or an error naming any name that matched nothing. An empty names
// list selects every provider — this is what backs --only.
func (r *Registry) Select(names []string) ([]Provider, error) {
	if len(names) == 0 {
		return r.providers, nil
	}

	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[strings.ToLower(strings.TrimSpace(n))] = true
	}

	var selected []Provider
	for _, p := range r.providers {
		key := strings.ToLower(p.Name())
		if want[key] {
			selected = append(selected, p)
			delete(want, key)
		}
	}

	if len(want) > 0 {
		unknown := make([]string, 0, len(want))
		for n := range want {
			unknown = append(unknown, n)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown provider(s): %s", strings.Join(unknown, ", "))
	}
	return selected, nil
}

// Status checks every provider concurrently. It has no side effects and never
// returns an error itself — per-provider failures are carried in each
// Result — so it is safe to run as often as a caller likes, including as a
// preflight before every cloud-calling command.
func Status(ctx context.Context, providers []Provider) []Result {
	return run(providers, func(p Provider) Result {
		ok, detail, err := p.IsAuthenticated(ctx)
		return Result{Provider: p.Name(), Authenticated: ok, Status: detail, Err: err}
	})
}

// Login authenticates every provider concurrently — each may pop open a
// browser, and there is no dependency between them, so running them one at a
// time would just be a needless wait. A provider whose session already looks
// valid is left alone unless force is set.
func Login(ctx context.Context, providers []Provider, force bool) []Result {
	return run(providers, func(p Provider) Result {
		if !force {
			if ok, detail, err := p.IsAuthenticated(ctx); err == nil && ok {
				return Result{Provider: p.Name(), Authenticated: true, Status: detail}
			}
		}

		loginErr := p.Login(ctx)
		ok, detail, statusErr := p.IsAuthenticated(ctx)
		err := loginErr
		if err == nil {
			err = statusErr
		}
		return Result{Provider: p.Name(), Authenticated: ok, Status: detail, Err: err}
	})
}

// Logout clears every provider's cached session concurrently.
func Logout(ctx context.Context, providers []Provider) []Result {
	return run(providers, func(p Provider) Result {
		return Result{Provider: p.Name(), Err: p.Logout(ctx)}
	})
}

// EnsureAll is the preflight every command that calls a cloud SDK should run
// before it does anything else: fail fast with "run kubespin login" rather
// than a cryptic SDK auth error three steps into cluster creation.
func EnsureAll(ctx context.Context, providers []Provider) error {
	var missing []string
	for _, r := range Status(ctx, providers) {
		if !r.Authenticated {
			missing = append(missing, r.Provider)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("not authenticated to %s; run: kubespin login --only %s",
		strings.Join(missing, ", "), strings.Join(missing, ","))
}

// run fans work out across providers concurrently and collects one Result
// per provider, in the original order. The worker never returns an error to
// the errgroup — a provider failure belongs in its own Result, not in
// stopping the other providers' work early.
func run(providers []Provider, work func(Provider) Result) []Result {
	results := make([]Result, len(providers))
	var g errgroup.Group
	for i, p := range providers {
		g.Go(func() error {
			results[i] = work(p)
			return nil
		})
	}
	_ = g.Wait()
	return results
}

// commandRunner abstracts shelling out to a CLI interactively, so Login/
// Logout are testable without actually invoking aws/gcloud/az.
type commandRunner func(ctx context.Context, name string, args ...string) error

// execRunner runs a real command, with the operator's own stdio attached —
// `aws sso login`/`gcloud auth login`/`az login` all print a code or open a
// browser, so they must be interactive rather than captured.
func execRunner(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // name/args are fixed CLI invocations, not user input
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// commandOutput abstracts shelling out to a CLI and capturing its stdout —
// used for checks that need a value back (an account name, a token), as
// opposed to commandRunner's interactive pass-through used for Login/Logout.
type commandOutput func(ctx context.Context, name string, args ...string) (string, error)

// execOutput runs a real command and returns its trimmed stdout.
func execOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // name/args are fixed CLI invocations, not user input
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("running %s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// checkBinary reports a clear, actionable error when a provider's CLI isn't
// on PATH, rather than letting exec.Command fail with a raw "executable file
// not found" once Login/Logout actually tries to run it.
func checkBinary(name, installHint string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s CLI not found in PATH; install it: %s", name, installHint)
	}
	return nil
}
