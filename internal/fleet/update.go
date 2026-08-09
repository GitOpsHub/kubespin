package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/GitOpsHub/kubespin/internal/catalog"
	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/registry"
	"github.com/GitOpsHub/kubespin/internal/repo"
)

// UpdateResult is one cluster's outcome from an update wave.
type UpdateResult struct {
	ClusterID core.ClusterID
	Committed bool
	Err       error
}

// UpdateOne patches a single cluster's repository so its override for
// component pins version, then re-renders and commits addons.yaml.
//
// It reuses catalog.Merge and repo.ReconcileAddons rather than duplicating
// their logic: an update wave is just many clusters each getting the same
// one-addon override applied, on top of whatever override patch they already
// carry.
func UpdateOne(
	ctx context.Context, resolver catalog.Resolver, repoProv repo.Provisioner,
	spec core.ClusterSpec, component, version string,
) (bool, error) {
	profile, err := resolver.Resolve(ctx, spec.Profile)
	if err != nil {
		return false, fmt.Errorf("resolving profile %s for %s: %w", spec.Profile, spec.ID, err)
	}

	overrides := setComponentVersion(spec.Overrides, component, version)

	merged, err := catalog.Merge(profile, overrides)
	if err != nil {
		return false, fmt.Errorf("applying update to %s: %w", spec.ID, err)
	}

	committed, err := repo.ReconcileAddons(ctx, repoProv, spec, merged)
	if err != nil {
		return false, fmt.Errorf("committing update for %s: %w", spec.ID, err)
	}
	return committed, nil
}

// setComponentVersion returns overrides with component's version pinned,
// adding a new override if the cluster did not already have one for it and
// leaving every other override untouched.
func setComponentVersion(overrides []core.AddonOverride, component, version string) []core.AddonOverride {
	out := make([]core.AddonOverride, len(overrides))
	copy(out, overrides)

	for i, o := range out {
		if o.Name == component {
			out[i].Version = version
			return out
		}
	}
	return append(out, core.AddonOverride{Name: component, Version: version})
}

// Update rolls component to version across every cluster matching filter,
// bounded by concurrency. Like Audit, one cluster's failure does not abort
// the wave: a rate-limited GitHub API or one cluster with a malformed
// override must not block updating the rest of the fleet.
func Update(
	ctx context.Context, reg registry.Registry, filter registry.Filter,
	resolver catalog.Resolver, repoProv repo.Provisioner,
	component, version string, concurrency int, opts ...Option,
) ([]UpdateResult, error) {
	logger := resolveOptions(opts).logger

	records, err := reg.List(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("listing fleet registry: %w", err)
	}
	if concurrency < 1 {
		concurrency = 1
	}
	logger.Info("starting fleet update wave",
		"component", component, "version", version, "clusters", len(records),
		"provider_filter", filter.Provider, "concurrency", concurrency)

	results := make([]UpdateResult, len(records))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, rec := range records {
		wg.Add(1)
		go func(i int, rec registry.Record) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			results[i] = updateRecord(ctx, rec, resolver, repoProv, component, version, logger)
		}(i, rec)
	}
	wg.Wait()

	return results, nil
}

// updateRecord reads a cluster's own cluster.yaml before updating it, so its
// existing override patch (which the Fleet Registry does not carry — only the
// repository does) survives the wave rather than being replaced by one
// containing only this update's override.
func updateRecord(
	ctx context.Context, rec registry.Record, resolver catalog.Resolver, repoProv repo.Provisioner, component, version string,
	logger *slog.Logger,
) UpdateResult {
	minimal := core.ClusterSpec{ID: rec.ClusterID, Provider: rec.Provider, Region: rec.Region, Profile: rec.Profile}

	spec, err := desiredSpec(ctx, repoProv, minimal)
	if err != nil {
		logger.Debug("cluster update skipped: could not read desired state", "cluster", rec.ClusterID, "error", err)
		return UpdateResult{ClusterID: rec.ClusterID, Err: err}
	}

	committed, err := UpdateOne(ctx, resolver, repoProv, spec, component, version)
	switch {
	case err != nil:
		logger.Debug("cluster update failed", "cluster", rec.ClusterID, "error", err)
	case committed:
		// A push to a cluster's repository is a fleet-wide mutation, so it is
		// worth seeing without --log-level debug.
		logger.Info("pushed version bump to cluster repository",
			"cluster", rec.ClusterID, "component", component, "version", version)
	default:
		logger.Debug("cluster already up to date",
			"cluster", rec.ClusterID, "component", component, "version", version)
	}
	return UpdateResult{ClusterID: rec.ClusterID, Committed: committed, Err: err}
}
