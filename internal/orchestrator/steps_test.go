package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GitOpsHub/kubespin/internal/catalog"
	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
	"github.com/GitOpsHub/kubespin/internal/registry"
	"github.com/GitOpsHub/kubespin/internal/repo"
)

// fakeCloud records the provisioner calls a run makes, in order.
type fakeCloud struct {
	calls []string

	statuses  []provisioner.Status
	describes int

	createErr   error
	identityErr error
	egressErr   error
	networkErr  error

	// ensuredSubnets, if set, is what EnsureNetwork resolves spec.Subnets to
	// — standing in for Azure creating a network when none was supplied.
	ensuredSubnets []string
	// createdSubnets records spec.Subnets as Create actually received it, so
	// a test can assert EnsureNetwork's result reached Cluster.Create.
	createdSubnets []string
}

func newFakeCloud() *fakeCloud {
	return &fakeCloud{statuses: []provisioner.Status{provisioner.StatusActive}}
}

func (f *fakeCloud) Provider() core.Provider { return core.ProviderAWS }

func (f *fakeCloud) Create(_ context.Context, spec core.ClusterSpec) error {
	f.calls = append(f.calls, "Create")
	f.createdSubnets = spec.Subnets
	return f.createErr
}

func (f *fakeCloud) Describe(context.Context, core.ClusterSpec) (provisioner.ClusterState, error) {
	f.calls = append(f.calls, "Describe")

	status := f.statuses[min(f.describes, len(f.statuses)-1)]
	f.describes++
	return provisioner.ClusterState{Status: status, NetworkID: "sg-cluster", OIDCIssuer: "https://issuer"}, nil
}

func (f *fakeCloud) Reconcile(context.Context, core.ClusterSpec) (provisioner.Change, error) {
	f.calls = append(f.calls, "Reconcile")
	return provisioner.Change{}, nil
}

func (f *fakeCloud) Delete(context.Context, core.ClusterSpec) error {
	f.calls = append(f.calls, "Delete")
	return nil
}

func (f *fakeCloud) ProvisionForComponent(
	context.Context, core.ClusterSpec, provisioner.Component,
) (provisioner.Binding, error) {
	f.calls = append(f.calls, "ProvisionForComponent")
	if f.identityErr != nil {
		return provisioner.Binding{}, f.identityErr
	}
	return provisioner.Binding{Identifier: "arn:aws:iam::123456789012:role/reporter"}, nil
}

func (f *fakeCloud) Deprovision(context.Context, core.ClusterSpec, provisioner.Component) error {
	f.calls = append(f.calls, "Deprovision")
	return nil
}

func (f *fakeCloud) AllowEgress(
	context.Context, core.ClusterSpec, provisioner.EgressDestination,
) (provisioner.Change, error) {
	f.calls = append(f.calls, "AllowEgress")
	return provisioner.Change{Changed: true}, f.egressErr
}

func (f *fakeCloud) EnsureNetwork(
	_ context.Context, spec core.ClusterSpec,
) (provisioner.NetworkResult, error) {
	f.calls = append(f.calls, "EnsureNetwork")
	if f.networkErr != nil {
		return provisioner.NetworkResult{}, f.networkErr
	}
	subnets := spec.Subnets
	if f.ensuredSubnets != nil {
		subnets = f.ensuredSubnets
	}
	return provisioner.NetworkResult{SubnetIDs: subnets}, nil
}

func (f *fakeCloud) cloud() Cloud {
	return Cloud{
		Cluster:  f,
		Identity: f,
		Network:  f,
		IngestionEndpoint: provisioner.EgressDestination{
			Host: "abc.execute-api.us-east-1.amazonaws.com", Port: 443,
		},
		Wait: provisioner.WaitOptions{Interval: time.Millisecond, Timeout: time.Second},
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func runWithCloud(t *testing.T, f *fakeCloud) (registry.Record, error) {
	t.Helper()

	reg := registry.NewMemory()
	o := New(reg,
		WithSteps(ProvisioningSteps(f.cloud(), repo.NewMemory(), catalog.NewBuiltinResolver(), reg, quietLogger())),
		WithHolder("test-runner"),
		WithLogger(quietLogger()),
	)
	return o.Apply(t.Context(), testSpec())
}

func TestProvisioningSteps_DriveTheProvisioners(t *testing.T) {
	f := newFakeCloud()

	rec, err := runWithCloud(t, f)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rec.Phase != core.PhaseReady {
		t.Errorf("Phase = %s, want ready", rec.Phase)
	}

	for _, want := range []string{"EnsureNetwork", "Create", "Describe", "Reconcile", "AllowEgress", "ProvisionForComponent"} {
		if !slices.Contains(f.calls, want) {
			t.Errorf("%s was never called; calls were %v", want, f.calls)
		}
	}

	// The network must be resolved before the cluster is requested — Create
	// needs the subnet EnsureNetwork returns.
	network := slices.Index(f.calls, "EnsureNetwork")
	create := slices.Index(f.calls, "Create")
	if network > create {
		t.Errorf("calls were %v, want the network ensured before the cluster is created", f.calls)
	}

	// Identity binding needs the issuer, which only exists once the control
	// plane is up — so it must follow the wait, not race it.
	identity := slices.Index(f.calls, "ProvisionForComponent")
	if create > identity {
		t.Errorf("calls were %v, want the cluster created before identity is bound", f.calls)
	}
}

// EnsureNetwork's result must reach Cluster.Create — this is what lets Azure
// create a network and have the cluster actually land in it.
func TestProvisioningSteps_NetworkResultReachesCreate(t *testing.T) {
	f := newFakeCloud()
	f.ensuredSubnets = []string{"/subscriptions/x/.../subnets/kubespin-created"}

	if _, err := runWithCloud(t, f); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if len(f.createdSubnets) != 1 || f.createdSubnets[0] != f.ensuredSubnets[0] {
		t.Errorf("Create received subnets %v, want EnsureNetwork's result %v", f.createdSubnets, f.ensuredSubnets)
	}
}

func TestProvisioningSteps_NetworkFailureStopsTheRun(t *testing.T) {
	f := newFakeCloud()
	f.networkErr = errors.New("no subnet available")

	_, err := runWithCloud(t, f)
	if err == nil {
		t.Fatal("expected the run to fail")
	}
	if slices.Contains(f.calls, "Create") {
		t.Error("Create was called despite EnsureNetwork failing")
	}
}

// The OIDC issuer has to land in the Fleet Registry: it is what the Central
// Ingestion API (M6) verifies fleet-status-reporter's signature against.
func TestProvisioningSteps_RecordsOIDCIssuer(t *testing.T) {
	f := newFakeCloud()
	reg := registry.NewMemory()
	o := New(reg,
		WithSteps(ProvisioningSteps(f.cloud(), repo.NewMemory(), catalog.NewBuiltinResolver(), reg, quietLogger())),
		WithHolder("test-runner"),
		WithLogger(quietLogger()),
	)

	if _, err := o.Apply(t.Context(), testSpec()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	stored, err := reg.Get(t.Context(), testSpec().ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.OIDCIssuer != "https://issuer" {
		t.Errorf("OIDCIssuer = %q, want %q", stored.OIDCIssuer, "https://issuer")
	}
}

// The phase must not advance while the control plane is still coming up, or a
// resumed run would believe a half-built cluster was finished.
func TestProvisioningSteps_WaitForTheControlPlane(t *testing.T) {
	f := newFakeCloud()
	f.statuses = []provisioner.Status{
		provisioner.StatusAbsent,
		provisioner.StatusCreating,
		provisioner.StatusActive,
	}

	if _, err := runWithCloud(t, f); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.describes < 3 {
		t.Errorf("Describe called %d times, want it to poll until active", f.describes)
	}
}

func TestProvisioningSteps_FailedClusterStopsTheRun(t *testing.T) {
	f := newFakeCloud()
	f.statuses = []provisioner.Status{provisioner.StatusFailed}

	rec, err := runWithCloud(t, f)
	if !errors.Is(err, provisioner.ErrClusterFailed) {
		t.Fatalf("error = %v, want one wrapping ErrClusterFailed", err)
	}
	// The phase stays where it was, so a retry re-runs cluster creation.
	if rec.Phase != core.PhasePending {
		t.Errorf("Phase = %s, want pending after a failed creation", rec.Phase)
	}
	if slices.Contains(f.calls, "ProvisionForComponent") {
		t.Error("identity was bound despite the cluster failing")
	}
}

func TestProvisioningSteps_EgressFailureStopsTheRun(t *testing.T) {
	// A cluster that cannot reach the ingestion API is invisible to the fleet,
	// so this is a failure rather than a warning.
	f := newFakeCloud()
	f.egressErr = errors.New("insufficient permissions")

	if _, err := runWithCloud(t, f); err == nil {
		t.Fatal("expected the egress failure to surface")
	}
}

// Without a configured endpoint there is nothing to allow, but the run should
// say so rather than silently produce a cluster that cannot report.
func TestProvisioningSteps_MissingIngestionEndpointIsNotFatal(t *testing.T) {
	f := newFakeCloud()
	cloud := f.cloud()
	cloud.IngestionEndpoint = provisioner.EgressDestination{}

	reg := registry.NewMemory()
	o := New(reg,
		WithSteps(ProvisioningSteps(cloud, repo.NewMemory(), catalog.NewBuiltinResolver(), reg, quietLogger())),
		WithLogger(quietLogger()),
	)

	rec, err := o.Apply(t.Context(), testSpec())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rec.Phase != core.PhaseReady {
		t.Errorf("Phase = %s, want ready", rec.Phase)
	}
	if slices.Contains(f.calls, "AllowEgress") {
		t.Error("egress was opened without a configured destination")
	}
}

// The Argo CD bootstrap (M5) is still a no-op, so a run reaches ready with a
// real cluster, identity, and seeded repository, but no addons synced —
// exactly the M3 gate and no more.
func TestProvisioningSteps_LaterPhasesRemainNoOps(t *testing.T) {
	steps := ProvisioningSteps(newFakeCloud().cloud(), repo.NewMemory(), catalog.NewBuiltinResolver(), registry.NewMemory(), quietLogger())

	for _, phase := range []core.Phase{core.PhaseRepoPushed, core.PhaseArgoCDInstalled} {
		step, ok := steps[phase]
		if !ok {
			t.Fatalf("no step registered for %s", phase)
		}
		if err := step.Run(t.Context(), testSpec(), registry.Record{}); err != nil {
			t.Errorf("step for %s returned %v, want a no-op", phase, err)
		}
	}
}

// PhaseIdentityBound now does real work: it must create and seed the
// cluster's repository.
func TestProvisioningSteps_SeedsRepository(t *testing.T) {
	repoProv := repo.NewMemory()
	steps := ProvisioningSteps(newFakeCloud().cloud(), repoProv, catalog.NewBuiltinResolver(), registry.NewMemory(), quietLogger())

	step, ok := steps[core.PhaseIdentityBound]
	if !ok {
		t.Fatal("no step registered for PhaseIdentityBound")
	}
	if err := step.Run(t.Context(), testSpec(), registry.Record{}); err != nil {
		t.Fatalf("seeding repository: %v", err)
	}

	exists, err := repoProv.Exists(t.Context(), testSpec())
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !exists {
		t.Error("expected the repository to have been created")
	}
}

// A cluster's override patch must land in the seeded addons.yaml: the whole
// point of M4's per-cluster override is that it is applied at seed time, not
// just accepted and ignored.
func TestProvisioningSteps_SeedsRepository_AppliesOverrides(t *testing.T) {
	repoProv := repo.NewMemory()
	steps := ProvisioningSteps(newFakeCloud().cloud(), repoProv, catalog.NewBuiltinResolver(), registry.NewMemory(), quietLogger())

	spec := testSpec()
	spec.Overrides = []core.AddonOverride{{Name: "cert-manager", Version: "1.16.0"}}

	step, ok := steps[core.PhaseIdentityBound]
	if !ok {
		t.Fatal("no step registered for PhaseIdentityBound")
	}
	if err := step.Run(t.Context(), spec, registry.Record{}); err != nil {
		t.Fatalf("seeding repository: %v", err)
	}

	checkout, err := repoProv.Clone(t.Context(), spec)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	addonsYAML, ok := checkout.File(repo.AddonsFile)
	if !ok {
		t.Fatal("expected addons.yaml to have been seeded")
	}
	if !strings.Contains(string(addonsYAML), "1.16.0") {
		t.Errorf("addons.yaml does not reflect the override: %s", addonsYAML)
	}
}
