package repo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// state is the .state.yaml contract: the last-applied hash used for
// idempotent diffing. Not user-authored.
//
// Only addons are hashed here. Infra drift is detected by
// ClusterProvisioner.Reconcile diffing the spec directly against live cloud
// state (M2), which needs no hash of its own; hashing it here as well would
// just be a second, redundant source of truth to keep in sync.
type state struct {
	AddonsHash string `yaml:"addonsHash"`
}

// Render produces the desired cluster.yaml and addons.yaml content for spec
// and its resolved profile.
func Render(spec core.ClusterSpec, profile core.Profile) (clusterYAML, addonsYAML []byte, err error) {
	clusterYAML, err = yaml.Marshal(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("rendering %s: %w", ClusterFile, err)
	}

	addonsYAML, err = yaml.Marshal(struct {
		Addons []core.AddonRef `yaml:"addons"`
	}{Addons: profile.Addons})
	if err != nil {
		return nil, nil, fmt.Errorf("rendering %s: %w", AddonsFile, err)
	}

	return clusterYAML, addonsYAML, nil
}

func hashAddons(addonsYAML []byte) string {
	sum := sha256.Sum256(addonsYAML)
	return hex.EncodeToString(sum[:])
}

// Seed creates and seeds a cluster's repository on its first apply: create
// (idempotent) then one initial commit of cluster.yaml, addons.yaml, and
// .state.yaml.
//
// It is itself idempotent — a repository that is already seeded (its
// addons.yaml already matches) is left alone — so a resumed run that reaches
// this step again does not create a second commit.
func Seed(ctx context.Context, rp Provisioner, spec core.ClusterSpec, profile core.Profile) error {
	if err := rp.Create(ctx, spec); err != nil {
		return fmt.Errorf("creating repository for %s: %w", spec.ID, err)
	}

	_, err := reconcile(ctx, rp, spec, profile, "kubespin: seed cluster.yaml and addons.yaml")
	return err
}

// ReconcileAddons brings a cluster's addons.yaml in line with its resolved
// profile.
//
// It reports whether it made a commit. `apply` proves it made no git commits
// when nothing differs, which is why this hashes addons.yaml against
// .state.yaml rather than relying on the caller to have diffed beforehand.
func ReconcileAddons(ctx context.Context, rp Provisioner, spec core.ClusterSpec, profile core.Profile) (bool, error) {
	return reconcile(ctx, rp, spec, profile, "kubespin: update addons.yaml")
}

func reconcile(
	ctx context.Context, rp Provisioner, spec core.ClusterSpec, profile core.Profile, message string,
) (bool, error) {
	clusterYAML, addonsYAML, err := Render(spec, profile)
	if err != nil {
		return false, err
	}
	desiredHash := hashAddons(addonsYAML)

	checkout, err := rp.Clone(ctx, spec)
	if err != nil {
		return false, fmt.Errorf("reading repository for %s: %w", spec.ID, err)
	}

	if current, ok := checkout.File(StateFile); ok {
		var currentState state
		if err := yaml.Unmarshal(current, &currentState); err != nil {
			return false, fmt.Errorf("parsing %s for %s: %w", StateFile, spec.ID, err)
		}
		if currentState.AddonsHash == desiredHash {
			return false, nil
		}
	}

	stateYAML, err := yaml.Marshal(state{AddonsHash: desiredHash})
	if err != nil {
		return false, fmt.Errorf("rendering %s: %w", StateFile, err)
	}

	committed, err := rp.Push(ctx, checkout, map[string][]byte{
		ClusterFile: clusterYAML,
		AddonsFile:  addonsYAML,
		StateFile:   stateYAML,
	}, message)
	if err != nil {
		return false, fmt.Errorf("pushing %s: %w", spec.ID, err)
	}
	return committed, nil
}
