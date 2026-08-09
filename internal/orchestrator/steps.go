package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
	"github.com/GitOpsHub/kubespin/internal/registry"
)

// Cloud bundles the provisioners for one cloud. Assembling them here keeps the
// per-cloud construction in one place, so adding GCP and Azure is a matter of
// building this struct differently rather than changing the orchestrator.
type Cloud struct {
	Cluster  provisioner.ClusterProvisioner
	Identity provisioner.IdentityProvisioner
	Network  provisioner.NetworkProvisioner

	// IngestionEndpoint is the Central Ingestion API the status reporter pushes
	// to, and the only destination a cluster's egress must permit.
	IngestionEndpoint provisioner.EgressDestination

	// Wait tunes how cluster creation is polled.
	Wait provisioner.WaitOptions
}

// ProvisioningSteps builds the steps that drive a real cluster from pending to
// ready.
//
// Only the first two phases are implemented. Repository seeding (M3) and the
// Argo CD bootstrap (M5) remain no-ops, so a run reaches ready with a real
// cluster and a real workload identity but no addons — which is exactly the M2
// gate and no more.
func ProvisioningSteps(cloud Cloud, logger *slog.Logger) map[core.Phase]Step {
	if logger == nil {
		logger = slog.Default()
	}

	steps := DefaultSteps()
	steps[core.PhasePending] = StepFunc{
		Label: "create cluster",
		Fn:    createClusterStep(cloud, logger),
	}
	steps[core.PhaseClusterCreated] = StepFunc{
		Label: "bind workload identity",
		Fn:    bindIdentityStep(cloud, logger),
	}
	return steps
}

// createClusterStep requests the cluster, waits for it to become active, then
// reconciles node pools and opens the status reporter's egress path.
//
// Waiting here rather than returning early is deliberate: the phase is only
// recorded once the cluster is genuinely usable, so a resumed run never
// believes a still-creating cluster is finished.
func createClusterStep(
	cloud Cloud, logger *slog.Logger,
) func(context.Context, core.ClusterSpec, registry.Record) error {
	return func(ctx context.Context, spec core.ClusterSpec, _ registry.Record) error {
		if err := cloud.Cluster.Create(ctx, spec); err != nil {
			return fmt.Errorf("requesting cluster %s: %w", spec.ID, err)
		}

		logger.Info("waiting for the control plane", "cluster", spec.ID)
		if _, err := provisioner.WaitUntilActive(ctx, cloud.Cluster, spec, cloud.Wait); err != nil {
			return fmt.Errorf("waiting for cluster %s: %w", spec.ID, err)
		}

		// Node pools are attached after the control plane is active, so this
		// reconcile is what actually creates them on a first run.
		change, err := cloud.Cluster.Reconcile(ctx, spec)
		if err != nil {
			return fmt.Errorf("reconciling cluster %s: %w", spec.ID, err)
		}
		if change.Changed {
			logger.Info("reconciled cluster", "cluster", spec.ID, "changes", change.Details)
		}

		return openEgress(ctx, cloud, spec, logger)
	}
}

// openEgress opens the one outbound path fleet state travels on.
func openEgress(
	ctx context.Context, cloud Cloud, spec core.ClusterSpec, logger *slog.Logger,
) error {
	if cloud.Network == nil || cloud.IngestionEndpoint.Host == "" {
		// Without a configured ingestion endpoint there is nothing to allow.
		// Said out loud, because a cluster that cannot report is invisible to
		// the fleet rather than obviously broken.
		logger.Warn("no ingestion endpoint configured; the cluster will not be able to report status",
			"cluster", spec.ID)
		return nil
	}

	change, err := cloud.Network.AllowEgress(ctx, spec, cloud.IngestionEndpoint)
	if err != nil {
		return fmt.Errorf("opening egress to the ingestion API: %w", err)
	}
	if change.Changed {
		logger.Info("opened egress", "cluster", spec.ID, "changes", change.Details)
	}
	return nil
}

// bindIdentityStep gives the status reporter a cloud-native identity.
func bindIdentityStep(
	cloud Cloud, logger *slog.Logger,
) func(context.Context, core.ClusterSpec, registry.Record) error {
	return func(ctx context.Context, spec core.ClusterSpec, _ registry.Record) error {
		comp := provisioner.StatusReporter()

		binding, err := cloud.Identity.ProvisionForComponent(ctx, spec, comp)
		if err != nil {
			return fmt.Errorf("binding identity for %s: %w", comp.Name, err)
		}

		logger.Info("bound workload identity",
			"cluster", spec.ID, "component", comp.Name, "identity", binding.Identifier)
		return nil
	}
}
