// Package kubeconfig updates the operator's local kubeconfig once a cluster
// is ready, by shelling out to the same cloud CLI a human would run by hand
// (aws eks update-kubeconfig / gcloud container clusters get-credentials /
// az aks get-credentials).
//
// This deliberately does not synthesize a kubeconfig entry in-process the way
// provisioner.RESTConfigProvisioner does for the Argo CD installer: that
// path's bearer tokens are minted fresh and live seconds (AWS STS presign,
// GCP ADC), fine for one Helm install but useless baked into a static
// kubeconfig file. Each cloud's own CLI instead writes an exec-based entry
// that re-authenticates itself indefinitely — the same three CLIs
// internal/auth already requires on the operator's machine for
// login/status/logout.
package kubeconfig

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// commandRunner abstracts shelling out, so dispatch is testable without
// invoking real CLIs — mirrors internal/auth's execRunner/commandRunner.
type commandRunner func(ctx context.Context, name string, env []string, args ...string) error

// execRunner runs a real command with the operator's stdio attached; these
// commands can print progress or prompt (e.g. az's overwrite confirmation).
func execRunner(ctx context.Context, name string, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // name/args are fixed CLI invocations, not user input
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// checkBinary reports a clear, actionable error when a provider's CLI isn't
// on PATH, rather than letting exec.Command fail with a raw "executable file
// not found" — matches internal/auth's checkBinary.
func checkBinary(name, installHint string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s CLI not found in PATH; install it: %s", name, installHint)
	}
	return nil
}

// Options carries the values Update needs that are not part of
// core.ClusterSpec: the kubeconfig path override, and the GCP project /
// Azure subscription the cluster lives in (neither is a ClusterSpec field —
// both come from apply's own --gcp-project/--azure-subscription flags).
type Options struct {
	// Path is the kubeconfig file to update. Empty means each CLI's own
	// default (typically ~/.kube/config, or $KUBECONFIG if set).
	Path string

	// GCPProject is required when spec.Provider == core.ProviderGCP.
	GCPProject string

	// AzureSubscription is required when spec.Provider == core.ProviderAzure.
	AzureSubscription string
}

// Update adds or refreshes a kubeconfig context for spec's cluster, and
// returns the name of the context it wrote (and made current) so the caller
// can tell the operator exactly what to `kubectl config use-context`.
//
// The cluster must already be active — this is meant to run once apply
// reaches phase ready. It is idempotent: rerunning it against the same
// cluster refreshes the existing entry rather than duplicating it, so a
// repeated apply against an already-ready cluster can call this safely.
func Update(ctx context.Context, spec core.ClusterSpec, opts Options) (string, error) {
	if err := checkCloudBinary(spec.Provider); err != nil {
		return "", err
	}
	return update(ctx, execRunner, spec, opts)
}

// checkCloudBinary reports a clear, actionable error when the CLI Update
// would need isn't on PATH, rather than letting exec.Command fail with a raw
// "executable file not found" partway through — kept separate from update()
// so unit tests can exercise argument construction without any cloud CLI
// installed.
func checkCloudBinary(p core.Provider) error {
	switch p {
	case core.ProviderAWS:
		return checkBinary("aws", "https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html")
	case core.ProviderGCP:
		return checkBinary("gcloud", "https://cloud.google.com/sdk/docs/install")
	case core.ProviderAzure:
		return checkBinary("az", "https://learn.microsoft.com/cli/azure/install-azure-cli")
	default:
		return fmt.Errorf("%w: unknown provider %q", core.ErrInvalidSpec, p)
	}
}

func update(ctx context.Context, run commandRunner, spec core.ClusterSpec, opts Options) (string, error) {
	switch spec.Provider {
	case core.ProviderAWS:
		return updateAWS(ctx, run, spec, opts)
	case core.ProviderGCP:
		return updateGCP(ctx, run, spec, opts)
	case core.ProviderAzure:
		return updateAzure(ctx, run, spec, opts)
	default:
		return "", fmt.Errorf("%w: unknown provider %q", core.ErrInvalidSpec, spec.Provider)
	}
}

// updateAWS pins the context name to the cluster ID via --alias. Without it,
// aws eks update-kubeconfig names the context after the cluster's full ARN
// (account ID and all), which the caller has no way to predict or print.
func updateAWS(ctx context.Context, run commandRunner, spec core.ClusterSpec, opts Options) (string, error) {
	contextName := spec.ID.String()
	args := []string{
		"eks", "update-kubeconfig", "--name", spec.ID.String(), "--region", spec.Region, "--alias", contextName,
	}
	if opts.Path != "" {
		args = append(args, "--kubeconfig", opts.Path)
	}
	return contextName, run(ctx, "aws", nil, args...)
}

// updateGCP does not compute the location it reports back independently of
// what get-credentials was actually told, so the returned context name stays
// correct even if spec.Zone/spec.Region disagree with reality.
func updateGCP(ctx context.Context, run commandRunner, spec core.ClusterSpec, opts Options) (string, error) {
	if opts.GCPProject == "" {
		return "", fmt.Errorf("%w: GCPProject is required for provider gcp", core.ErrInvalidSpec)
	}

	location := spec.Region
	args := []string{"container", "clusters", "get-credentials", spec.ID.String(), "--project", opts.GCPProject}
	if spec.Zone != "" {
		location = spec.Zone
		args = append(args, "--zone", location)
	} else {
		args = append(args, "--region", location)
	}

	// gcloud has no --kubeconfig flag; it honours $KUBECONFIG instead.
	var env []string
	if opts.Path != "" {
		env = []string{"KUBECONFIG=" + opts.Path}
	}

	// gcloud's own context-naming convention — see
	// https://cloud.google.com/sdk/gcloud/reference/container/clusters/get-credentials.
	contextName := fmt.Sprintf("gke_%s_%s_%s", opts.GCPProject, location, spec.ID.String())
	return contextName, run(ctx, "gcloud", env, args...)
}

func updateAzure(ctx context.Context, run commandRunner, spec core.ClusterSpec, opts Options) (string, error) {
	if opts.AzureSubscription == "" {
		return "", fmt.Errorf("%w: AzureSubscription is required for provider azure", core.ErrInvalidSpec)
	}

	args := []string{
		"aks", "get-credentials",
		"--name", spec.ID.String(),
		"--resource-group", "kubespin-" + spec.ID.String(),
		"--subscription", opts.AzureSubscription,
		// Keeps a repeated apply idempotent: without this, get-credentials
		// prompts interactively to overwrite an existing context, which apply
		// has no way to answer.
		"--overwrite-existing",
	}
	if opts.Path != "" {
		args = append(args, "--file", opts.Path)
	}
	// az aks get-credentials names the context after the cluster name alone.
	return spec.ID.String(), run(ctx, "az", nil, args...)
}
