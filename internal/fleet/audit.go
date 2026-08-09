package fleet

import (
	"context"
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
	"github.com/GitOpsHub/kubespin/internal/repo"
)

// Finding is one drift a cluster's live infra shows against its cluster.yaml.
type Finding struct {
	ClusterID core.ClusterID
	Detail    string
}

// AuditOne audits a single cluster: describes its live cloud state and diffs
// it against the cluster.yaml in its own repository.
//
// It is read-only — it never calls Reconcile or Push. `fleet audit` exists to
// surface drift for a human (or `apply`) to act on, not to correct it
// silently; a manually resized node pool someone did on purpose should be
// seen, not overwritten out from under them.
func AuditOne(
	ctx context.Context, cluster provisioner.ClusterProvisioner, repoProv repo.Provisioner, id core.ClusterID, provider core.Provider, region string,
) ([]Finding, error) {
	spec := core.ClusterSpec{ID: id, Provider: provider, Region: region}

	state, err := cluster.Describe(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("describing %s: %w", id, err)
	}
	if state.Status == provisioner.StatusAbsent {
		return []Finding{{ClusterID: id, Detail: "registered in the Fleet Registry but does not exist in the cloud"}}, nil
	}

	desired, err := desiredSpec(ctx, repoProv, spec)
	if err != nil {
		return nil, err
	}

	var findings []Finding
	if state.Access != desired.Access {
		findings = append(findings, Finding{id, fmt.Sprintf(
			"access drifted: live=%s desired=%s", state.Access, desired.Access)})
	}

	live := make(map[string]core.NodePool, len(state.NodePools))
	for _, pool := range state.NodePools {
		live[pool.Name] = pool
	}
	for _, want := range desired.NodePools {
		got, ok := live[want.Name]
		if !ok {
			findings = append(findings, Finding{id, fmt.Sprintf("node pool %s is missing in the cloud", want.Name)})
			continue
		}
		if got.MinSize != want.MinSize || got.MaxSize != want.MaxSize || got.DesiredSize != want.DesiredSize {
			findings = append(findings, Finding{id, fmt.Sprintf(
				"node pool %s drifted: live=%d/%d/%d (min/desired/max) desired=%d/%d/%d",
				want.Name, got.MinSize, got.DesiredSize, got.MaxSize, want.MinSize, want.DesiredSize, want.MaxSize)})
		}
	}

	return findings, nil
}

// desiredSpec reads and parses the cluster.yaml a cluster's own repository
// holds — the desired state `fleet audit` diffs live infra against.
func desiredSpec(ctx context.Context, repoProv repo.Provisioner, spec core.ClusterSpec) (core.ClusterSpec, error) {
	checkout, err := repoProv.Clone(ctx, spec)
	if err != nil {
		return core.ClusterSpec{}, fmt.Errorf("reading repository for %s: %w", spec.ID, err)
	}

	content, ok := checkout.File(repo.ClusterFile)
	if !ok {
		return core.ClusterSpec{}, fmt.Errorf("%s has no %s in its repository", spec.ID, repo.ClusterFile)
	}

	var desired core.ClusterSpec
	if err := yaml.Unmarshal(content, &desired); err != nil {
		return core.ClusterSpec{}, fmt.Errorf("parsing %s for %s: %w", repo.ClusterFile, spec.ID, err)
	}
	return desired, nil
}
