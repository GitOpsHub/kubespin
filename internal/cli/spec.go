package cli

import (
	"bytes"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// defaultPoolName is the node pool created when a spec is built from flags
// rather than a file.
const defaultPoolName = "default"

// loadSpec builds a cluster spec from --spec, or from the individual flags.
//
// The file form is the same cluster.yaml that lives in a cluster's repository,
// so what an operator passes here is what the repository will hold — rather
// than a separate input format that has to be kept in step with it.
func loadSpec(cmd *cobra.Command) (core.ClusterSpec, error) {
	path, err := cmd.Flags().GetString("spec")
	if err != nil {
		return core.ClusterSpec{}, fmt.Errorf("reading --spec: %w", err)
	}

	var spec core.ClusterSpec
	if path != "" {
		if spec, err = readSpecFile(path); err != nil {
			return core.ClusterSpec{}, err
		}
	}

	if err := applySpecFlags(cmd, &spec); err != nil {
		return core.ClusterSpec{}, err
	}

	if err := spec.Validate(); err != nil {
		return core.ClusterSpec{}, fmt.Errorf("invalid cluster spec: %w", err)
	}
	return spec, nil
}

func readSpecFile(path string) (core.ClusterSpec, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // operator-supplied path, by design
	if err != nil {
		return core.ClusterSpec{}, fmt.Errorf("reading spec file %s: %w", path, err)
	}

	var spec core.ClusterSpec
	// Strict: an unknown field is far more likely a typo in a key that silently
	// does nothing than a deliberate extension.
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)

	if err := decoder.Decode(&spec); err != nil {
		return core.ClusterSpec{}, fmt.Errorf("parsing spec file %s: %w", path, err)
	}
	return spec, nil
}

// applySpecFlags overlays explicitly-set flags onto the spec, so a file can be
// used as a base and a single field overridden without editing it.
func applySpecFlags(cmd *cobra.Command, spec *core.ClusterSpec) error {
	flags := cmd.Flags()

	for _, field := range []struct {
		name    string
		current func() string
		apply   func(string)
	}{
		{"cluster-id", func() string { return spec.ID.String() }, func(v string) { spec.ID = core.ClusterID(v) }},
		{"provider", func() string { return spec.Provider.String() }, func(v string) { spec.Provider = core.Provider(v) }},
		{"region", func() string { return spec.Region }, func(v string) { spec.Region = v }},
		{"access", func() string { return spec.Access.String() }, func(v string) { spec.Access = core.Access(v) }},
		{"kubernetes-version", func() string { return spec.KubernetesVersion }, func(v string) { spec.KubernetesVersion = v }},
		{"vpc-cidr", func() string { return spec.VPCCIDR }, func(v string) { spec.VPCCIDR = v }},
		{"vnet-cidr", func() string { return spec.VNetCIDR }, func(v string) { spec.VNetCIDR = v }},
		{"subnet-cidr", func() string { return spec.SubnetCIDR }, func(v string) { spec.SubnetCIDR = v }},
	} {
		// An explicitly-set flag always wins. A flag left at its default only
		// applies when the file did not set the field, so passing a spec file
		// does not have every value quietly replaced by flag defaults.
		if !flags.Changed(field.name) && field.current() != "" {
			continue
		}

		value, err := flags.GetString(field.name)
		if err != nil {
			return fmt.Errorf("reading --%s: %w", field.name, err)
		}
		if value != "" {
			field.apply(value)
		}
	}

	if flags.Changed("profile") || spec.Profile.Name == "" {
		profile, err := flags.GetString("profile")
		if err != nil {
			return fmt.Errorf("reading --profile: %w", err)
		}
		if profile != "" {
			ref, err := parseProfileRef(profile)
			if err != nil {
				return err
			}
			spec.Profile = ref
		}
	}

	if flags.Changed("subnets") || len(spec.Subnets) == 0 {
		subnets, err := flags.GetStringSlice("subnets")
		if err != nil {
			return fmt.Errorf("reading --subnets: %w", err)
		}
		if len(subnets) > 0 {
			spec.Subnets = subnets
		}
	}

	return applyNodePoolFlags(cmd, spec)
}

// applyNodePoolFlags builds a single default node pool when the spec has none.
// Richer topologies belong in a spec file: expressing several pools through
// flags is more error-prone than editing the file the repository will hold.
func applyNodePoolFlags(cmd *cobra.Command, spec *core.ClusterSpec) error {
	if len(spec.NodePools) > 0 {
		return nil
	}
	flags := cmd.Flags()

	instanceType, err := flags.GetString("instance-type")
	if err != nil {
		return fmt.Errorf("reading --instance-type: %w", err)
	}
	minSize, err := flags.GetInt32("min-size")
	if err != nil {
		return fmt.Errorf("reading --min-size: %w", err)
	}
	maxSize, err := flags.GetInt32("max-size")
	if err != nil {
		return fmt.Errorf("reading --max-size: %w", err)
	}
	desired, err := flags.GetInt32("desired-size")
	if err != nil {
		return fmt.Errorf("reading --desired-size: %w", err)
	}

	spec.NodePools = []core.NodePool{{
		Name:         defaultPoolName,
		InstanceType: instanceType,
		MinSize:      minSize,
		MaxSize:      maxSize,
		DesiredSize:  desired,
	}}
	return nil
}

// parseProfileRef splits a name@version reference.
func parseProfileRef(raw string) (core.ProfileRef, error) {
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] == '@' {
			return core.ProfileRef{Name: raw[:i], Version: raw[i+1:]}, nil
		}
	}
	return core.ProfileRef{}, fmt.Errorf(
		"%w: profile %q must be name@version, for example tier-small@1.0.0", core.ErrInvalidSpec, raw)
}
