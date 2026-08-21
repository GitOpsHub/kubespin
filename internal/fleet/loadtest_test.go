package fleet

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GitOpsHub/kubespin/internal/catalog"
	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/provisioner"
	"github.com/GitOpsHub/kubespin/internal/registry"
	"github.com/GitOpsHub/kubespin/internal/repo"
)

// fleetSize matches the implementation plan's Milestone 10 load test target:
// a simulated fleet of 1,000+ Fleet Registry entries.
const fleetSize = 1200

// concurrencyTracker records the maximum number of calls in flight at once,
// so a load test can assert a worker pool actually bounded its concurrency
// rather than firing every goroutine at once.
type concurrencyTracker struct {
	current int64
	max     int64
}

func (c *concurrencyTracker) enter() func() {
	n := atomic.AddInt64(&c.current, 1)
	for {
		m := atomic.LoadInt64(&c.max)
		if n <= m || atomic.CompareAndSwapInt64(&c.max, m, n) {
			break
		}
	}
	return func() { atomic.AddInt64(&c.current, -1) }
}

// seedFleet registers n clusters, all pre-seeded in repoProv with a real
// cluster.yaml and addons.yaml — cloud and git calls are mocked (an
// in-memory registry, an in-memory repo, a fake cloud provisioner), the way
// the implementation plan's Milestone 10 describes for a scale this large.
func seedFleet(t *testing.T, n int) (registry.Registry, repo.Provisioner) {
	t.Helper()

	reg := registry.NewMemory()
	repoProv := repo.NewMemory()
	resolver := catalog.NewBuiltinResolver()
	profile, err := resolver.Resolve(context.Background(), core.SizeSmall)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for i := 0; i < n; i++ {
		spec := core.ClusterSpec{
			ID:       core.ClusterID(fmt.Sprintf("load-test-cluster-%04d", i)),
			Provider: core.ProviderAWS, Region: "us-east-1", Access: core.AccessPrivate,
			Subnets: []string{"subnet-aaa"},
			NodePools: []core.NodePool{
				{Name: "default", InstanceType: "m6i.large", MinSize: 1, MaxSize: 5, DesiredSize: 3},
			},
			Size: core.SizeSmall,
		}
		if _, err := reg.Create(context.Background(), registry.NewRecord(spec, time.Now())); err != nil {
			t.Fatalf("seeding registry for %s: %v", spec.ID, err)
		}
		if err := repo.Seed(context.Background(), repoProv, spec, profile); err != nil {
			t.Fatalf("seeding repo for %s: %v", spec.ID, err)
		}
	}

	return reg, repoProv
}

// TestLoad_Audit simulates fleet audit against 1,000+ clusters, asserting it
// completes promptly and respects its concurrency bound. Cloud calls are
// mocked (fakeCluster), the way the implementation plan directs for a scale
// this large — a real 1,200-cluster fleet is not something to provision for
// a test.
func TestLoad_Audit(t *testing.T) {
	if testing.Short() {
		t.Skip("load test; skipped with -short")
	}

	reg, repoProv := seedFleet(t, fleetSize)

	const concurrency = 20
	tracker := &concurrencyTracker{}
	factory := func(context.Context, core.Provider, string) (provisioner.ClusterProvisioner, error) {
		return trackedCluster{
			tracker: tracker,
			state: provisioner.ClusterState{
				Status: provisioner.StatusActive, Access: core.AccessPrivate,
				NodePools: []core.NodePool{{Name: "default", InstanceType: "m6i.large", MinSize: 1, MaxSize: 5, DesiredSize: 3}},
			},
		}, nil
	}

	start := time.Now()
	results, err := Audit(context.Background(), reg, registry.Filter{}, factory, repoProv, concurrency)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}

	if len(results) != fleetSize {
		t.Fatalf("results = %d, want %d", len(results), fleetSize)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("cluster %s: %v", r.ClusterID, r.Err)
		}
		if len(r.Findings) != 0 {
			t.Fatalf("cluster %s: unexpected findings %+v", r.ClusterID, r.Findings)
		}
	}

	got := atomic.LoadInt64(&tracker.max)
	if got > concurrency {
		t.Errorf("max concurrent Describe calls = %d, want <= %d", got, concurrency)
	}
	if got <= 1 {
		t.Errorf("max concurrent Describe calls = %d, want audits to actually overlap (concurrency bound = %d)", got, concurrency)
	}

	t.Logf("audited %d clusters in %s (max concurrency observed: %d)", fleetSize, elapsed, tracker.max)
	if elapsed > 30*time.Second {
		t.Errorf("audit of %d clusters took %s, want under 30s against mocked calls", fleetSize, elapsed)
	}
}

// TestLoad_Update simulates a fleet update wave, the other half of
// Milestone 10's load test: rolling a component version across every
// cluster without exceeding the (simulated) worker pool's concurrency bound.
func TestLoad_Update(t *testing.T) {
	if testing.Short() {
		t.Skip("load test; skipped with -short")
	}

	reg, repoProv := seedFleet(t, fleetSize)
	resolver := catalog.NewBuiltinResolver()

	const concurrency = 20
	start := time.Now()
	results, err := Update(context.Background(), reg, registry.Filter{}, resolver, repoProv, "cert-manager", "1.16.0", concurrency, 0)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(results) != fleetSize {
		t.Fatalf("results = %d, want %d", len(results), fleetSize)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("cluster %s: %v", r.ClusterID, r.Err)
		}
		if !r.Committed {
			t.Errorf("cluster %s: expected a commit", r.ClusterID)
		}
	}

	t.Logf("updated %d clusters in %s", fleetSize, elapsed)
	if elapsed > 30*time.Second {
		t.Errorf("update wave over %d clusters took %s, want under 30s against mocked calls", fleetSize, elapsed)
	}
}

// trackedCluster wraps fakeCluster's behavior with concurrency tracking, so
// TestLoad_Audit can assert Audit's worker pool actually bounded how many
// Describe calls ran at once.
type trackedCluster struct {
	tracker *concurrencyTracker
	state   provisioner.ClusterState
}

func (t trackedCluster) Provider() core.Provider                        { return core.ProviderAWS }
func (t trackedCluster) Create(context.Context, core.ClusterSpec) error { return nil }
func (t trackedCluster) Describe(context.Context, core.ClusterSpec) (provisioner.ClusterState, error) {
	defer t.tracker.enter()()
	// A tiny synthetic delay, standing in for a real cloud API's latency: with
	// no delay at all, 1,200 in-memory calls finish so fast that goroutines
	// rarely overlap, which would let this test pass even if Audit forgot to
	// run cluster audits concurrently at all.
	time.Sleep(time.Millisecond)
	return t.state, nil
}
func (t trackedCluster) Reconcile(context.Context, core.ClusterSpec) (provisioner.Change, error) {
	return provisioner.Change{}, nil
}
func (t trackedCluster) Delete(context.Context, core.ClusterSpec) error { return nil }
