// Package provisioner defines the cloud-facing interfaces every cluster is
// built through, and the shared types they exchange.
//
// The three clouds differ enormously in their APIs and barely at all in what
// kubespin asks of them. Everything cloud-specific lives behind these
// interfaces in the per-cloud subpackages; no provider conditional belongs in
// command, orchestrator, or catalog code.
package provisioner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// Sentinel errors shared across implementations.
var (
	// ErrNotFound means the cluster does not exist in the cloud.
	ErrNotFound = errors.New("cluster does not exist")
	// ErrUnsupported means the provider cannot express what the spec asks for.
	ErrUnsupported = errors.New("unsupported by this provider")
	// ErrClusterFailed means the cloud reports the cluster in a failed state,
	// which no amount of waiting will resolve.
	ErrClusterFailed = errors.New("cluster is in a failed state")
)

// Status is a cluster's lifecycle state, normalised across the three clouds.
type Status string

// Cluster lifecycle states.
const (
	StatusAbsent   Status = "absent"
	StatusCreating Status = "creating"
	StatusActive   Status = "active"
	StatusUpdating Status = "updating"
	StatusDeleting Status = "deleting"
	StatusFailed   Status = "failed"
)

// Settled reports whether the status is one that will not change on its own.
func (s Status) Settled() bool {
	return s == StatusActive || s == StatusFailed || s == StatusAbsent
}

// ClusterState is what the cloud currently reports about a cluster.
type ClusterState struct {
	Status   Status
	Endpoint string

	// OIDCIssuer is the cluster's identity issuer URL. Workload identity cannot
	// be bound until the cluster is active and this is populated, which is why
	// identity binding is a separate phase rather than part of creation.
	OIDCIssuer string

	Version   string
	Access    core.Access
	NodePools []core.NodePool

	// NetworkID identifies the cluster's network scope for egress rules: the
	// security group on AWS, the network on GCP, the NSG on Azure.
	NetworkID string
}

// Change is the outcome of a Reconcile.
//
// Changed is reported as data rather than inferred by the caller diffing state
// before and after. `apply` must be able to prove it made no cloud calls when
// nothing differs, and a caller-side diff cannot distinguish "nothing to do"
// from "changed and changed back".
type Change struct {
	Changed bool
	Details []string
}

// Merge folds another change into this one.
func (c *Change) Merge(other Change) {
	c.Changed = c.Changed || other.Changed
	c.Details = append(c.Details, other.Details...)
}

// ClusterProvisioner manages a cluster's lifecycle on one cloud.
//
// Create is asynchronous on every cloud — provisioning takes 10 to 30 minutes —
// so it returns as soon as the request is accepted and the caller polls
// Describe. A blocking Create would have to outlive the orchestrator's lease
// renewal window, which is a bug generator.
type ClusterProvisioner interface {
	// Provider identifies which cloud this implementation serves.
	Provider() core.Provider

	// Create requests a new cluster. It is idempotent: creating a cluster that
	// already exists is a no-op, so a resumed run does not fail here.
	Create(ctx context.Context, spec core.ClusterSpec) error

	// Describe reports current state. It returns StatusAbsent rather than an
	// error when the cluster does not exist, because "not there yet" is a
	// normal answer during polling.
	Describe(ctx context.Context, spec core.ClusterSpec) (ClusterState, error)

	// Reconcile brings an existing cluster in line with the spec — node pool
	// sizing, access configuration — and reports whether it changed anything.
	Reconcile(ctx context.Context, spec core.ClusterSpec) (Change, error)

	// Delete tears the cluster down. Deleting an absent cluster is a no-op, so
	// a retried teardown converges rather than failing.
	Delete(ctx context.Context, spec core.ClusterSpec) error
}

// Component is an in-cluster workload that needs a cloud identity.
type Component struct {
	Name           string
	Namespace      string
	ServiceAccount string
}

// Binding is the cloud identity bound to a component, and how to attach it.
type Binding struct {
	// Identifier is the cloud-native handle: an IAM role ARN, a Google service
	// account email, or an Azure client ID.
	Identifier string

	// Annotations go on the Kubernetes ServiceAccount to complete the binding.
	// Each cloud uses a different key, so the caller applies them blind rather
	// than knowing which cloud it is on.
	Annotations map[string]string
}

// IdentityProvisioner binds a cloud-native workload identity to an in-cluster
// service account.
//
// The identity exists to be *proven*, not to grant cloud access:
// fleet-status-reporter uses it to sign its push to the Central Ingestion API,
// which verifies the signature. That is why Component carries no permission
// set — a component that needed cloud permissions would be a different
// interface, and adding one should be a deliberate decision rather than a
// convenient extension of this one.
type IdentityProvisioner interface {
	Provider() core.Provider

	// ProvisionForComponent is idempotent, returning the existing binding when
	// one is already in place.
	ProvisionForComponent(ctx context.Context, spec core.ClusterSpec, comp Component) (Binding, error)

	// Deprovision removes the identity. Used by teardown; removing an absent
	// identity is a no-op.
	Deprovision(ctx context.Context, spec core.ClusterSpec, comp Component) error
}

// EgressDestination is an outbound endpoint a cluster must be able to reach.
type EgressDestination struct {
	Host        string
	Port        int32
	CIDR        string
	Description string
}

// NetworkResult is what EnsureNetwork resolves a spec's subnets to.
type NetworkResult struct {
	SubnetIDs []string
	Change    Change
}

// NetworkProvisioner opens the one outbound path the architecture depends on,
// and resolves the network a cluster is created in.
//
// Nothing reaches into a cluster, so the status reporter's egress to the
// Central Ingestion API is the only way fleet state escapes. Provisioning it
// during cluster creation rather than later matters: every cluster built
// without it needs a network change before it can report at all.
type NetworkProvisioner interface {
	Provider() core.Provider

	// EnsureNetwork resolves the subnets a cluster will be created in. If
	// spec.Subnets is already set, implementations pass it through unchanged
	// (Change.Changed stays false) — kubespin never touches a network an
	// operator already supplied. If empty, every implementation creates a
	// network deterministically named from the cluster ID and adopts it on a
	// repeated call, so a resumed or repeated apply converges rather than
	// duplicating resources.
	EnsureNetwork(ctx context.Context, spec core.ClusterSpec) (NetworkResult, error)

	AllowEgress(ctx context.Context, spec core.ClusterSpec, dest EgressDestination) (Change, error)
}

// StatusReporter is the component whose identity and egress every cluster gets.
func StatusReporter() Component {
	return Component{
		Name:           "fleet-status-reporter",
		Namespace:      "kubespin-system",
		ServiceAccount: "fleet-status-reporter",
	}
}

// WaitOptions tunes WaitUntilActive and WaitUntilGone.
type WaitOptions struct {
	Interval time.Duration
	Timeout  time.Duration
}

// DefaultWaitOptions suit real cluster creation, which takes 10-30 minutes on
// every cloud.
func DefaultWaitOptions() WaitOptions {
	return WaitOptions{Interval: 30 * time.Second, Timeout: 45 * time.Minute}
}

// WaitUntilActive polls Describe until the cluster settles.
//
// Polling Describe rather than blocking inside Create is what lets the
// orchestrator keep renewing its lease across a long creation, and what makes
// a resumed run able to pick up a cluster another run started.
func WaitUntilActive(
	ctx context.Context, p ClusterProvisioner, spec core.ClusterSpec, opts WaitOptions,
) (ClusterState, error) {
	if opts.Interval <= 0 {
		opts.Interval = time.Second
	}

	deadline := time.Now().Add(opts.Timeout)
	if opts.Timeout <= 0 {
		deadline = time.Now().Add(DefaultWaitOptions().Timeout)
	}

	for {
		state, err := p.Describe(ctx, spec)
		if err != nil {
			return state, fmt.Errorf("describing %s: %w", spec.ID, err)
		}

		switch state.Status {
		case StatusActive:
			return state, nil
		case StatusFailed:
			return state, fmt.Errorf("%w: %s", ErrClusterFailed, spec.ID)
		case StatusAbsent:
			// Creation was requested but the cloud has not registered it yet;
			// keep polling rather than treating this as a failure.
		case StatusCreating, StatusUpdating, StatusDeleting:
		}

		if time.Now().After(deadline) {
			return state, fmt.Errorf("timed out waiting for %s to become active; last status %s",
				spec.ID, state.Status)
		}

		select {
		case <-ctx.Done():
			return state, fmt.Errorf("waiting for %s: %w", spec.ID, ctx.Err())
		case <-time.After(opts.Interval):
		}
	}
}

// WaitUntilGone polls Describe until the cluster no longer exists.
//
// Deletion is asynchronous on every cloud, exactly like creation: the API
// accepts the request and the cluster lingers in a deleting state for minutes.
// Teardown polls this before recording the cluster decommissioned, so the
// registry never claims a cluster is gone while the cloud is still tearing it
// down — and so a failed deletion surfaces as a failure rather than as a
// clean-looking record.
func WaitUntilGone(
	ctx context.Context, p ClusterProvisioner, spec core.ClusterSpec, opts WaitOptions,
) error {
	if opts.Interval <= 0 {
		opts.Interval = DefaultWaitOptions().Interval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultWaitOptions().Timeout
	}
	deadline := time.Now().Add(opts.Timeout)

	for {
		state, err := p.Describe(ctx, spec)
		if err != nil {
			return fmt.Errorf("describing %s: %w", spec.ID, err)
		}

		switch state.Status {
		case StatusAbsent:
			return nil
		case StatusFailed:
			// The cloud gave up mid-deletion; leaving the phase at
			// decommissioning is what lets a retried delete resume.
			return fmt.Errorf("%w: %s failed while deleting", ErrClusterFailed, spec.ID)
		case StatusDeleting, StatusActive, StatusCreating, StatusUpdating:
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s to be deleted; last status %s",
				spec.ID, state.Status)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s to be deleted: %w", spec.ID, ctx.Err())
		case <-time.After(opts.Interval):
		}
	}
}
