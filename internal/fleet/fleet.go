package fleet

import (
	"context"
	"fmt"
	"sync"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
	"github.com/GitOpsHub/kubespin/internal/registry"
	"github.com/GitOpsHub/kubespin/internal/repo"
)

// ClusterProvisionerFactory builds the ClusterProvisioner for one cluster's
// provider and region. The CLI supplies one that constructs real cloud SDK
// clients (internal/provisioner/{aws,gcp,azure}); tests supply a fake — the
// same seam internal/cli's buildCloud exists behind for `apply`.
type ClusterProvisionerFactory func(ctx context.Context, provider core.Provider, region string) (provisioner.ClusterProvisioner, error)

// AuditResult is one cluster's audit outcome: either findings, or an error
// that kept the audit from completing for that cluster.
type AuditResult struct {
	ClusterID core.ClusterID
	Findings  []Finding
	Err       error
}

// Audit runs AuditOne across every cluster matching filter, bounded by
// concurrency.
//
// One cluster's failure — an unreachable cloud API, a repo that 404s —
// cannot abort the run: audit exists to survive exactly that kind of partial
// failure and report what it could, which is why each cluster's outcome is
// its own AuditResult rather than the first error stopping everything.
func Audit(
	ctx context.Context, reg registry.Registry, filter registry.Filter,
	clusters ClusterProvisionerFactory, repoProv repo.Provisioner, concurrency int,
) ([]AuditResult, error) {
	records, err := reg.List(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("listing fleet registry: %w", err)
	}
	if concurrency < 1 {
		concurrency = 1
	}

	results := make([]AuditResult, len(records))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i, rec := range records {
		wg.Add(1)
		go func(i int, rec registry.Record) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			results[i] = auditRecord(ctx, rec, clusters, repoProv)
		}(i, rec)
	}
	wg.Wait()

	return results, nil
}

func auditRecord(
	ctx context.Context, rec registry.Record, clusters ClusterProvisionerFactory, repoProv repo.Provisioner,
) AuditResult {
	cluster, err := clusters(ctx, rec.Provider, rec.Region)
	if err != nil {
		return AuditResult{ClusterID: rec.ClusterID, Err: fmt.Errorf("building provisioner for %s: %w", rec.ClusterID, err)}
	}

	findings, err := AuditOne(ctx, cluster, repoProv, rec.ClusterID, rec.Provider, rec.Region)
	return AuditResult{ClusterID: rec.ClusterID, Findings: findings, Err: err}
}
