// Package argocd renders the manifests that bootstrap a cluster's local Argo
// CD instance and hand it the app-of-apps that syncs every addon, and
// installs Argo CD itself via the Helm Go library.
//
// Nothing here reaches into a cluster except the Helm install in installer.go
// — RenderRootApplication and RenderAddonApplications are pure functions over
// a Profile, so the manifests app-of-apps depends on are fully testable
// without a cluster.
package argocd

import "log/slog"

// options carries the settings the rendering entry points accept. It is a
// struct rather than a field on a type because everything this package does
// today is a pure function over a Profile — there is no installer object to
// hang a logger off yet.
type options struct {
	logger *slog.Logger
}

// Option configures a rendering call.
type Option func(*options)

// WithLogger sets the logger. Without it, rendering logs to slog.Default().
func WithLogger(logger *slog.Logger) Option {
	return func(o *options) {
		if logger != nil {
			o.logger = logger
		}
	}
}

// resolve applies opts over the defaults.
func resolve(opts []Option) options {
	o := options{logger: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// inClusterServer is the destination every rendered Application points at.
// Argo CD is local to the cluster it manages — there is no central hub — so
// this is always the in-cluster API server, never a remote one.
const inClusterServer = "https://kubernetes.default.svc"

// Namespace is where Argo CD and every Application resource it manages live.
const Namespace = "argocd"

// Application is the subset of the Argo CD Application CRD
// (argoproj.io/v1alpha1) this package renders. It is a local, minimal
// mirror of that CRD rather than a dependency on Argo CD's own Go module:
// kubespin only ever writes these fields, never reads or reconciles them.
type Application struct {
	APIVersion string              `yaml:"apiVersion"`
	Kind       string              `yaml:"kind"`
	Metadata   ApplicationMetadata `yaml:"metadata"`
	Spec       ApplicationSpec     `yaml:"spec"`
}

// ApplicationMetadata is the CRD's metadata block.
type ApplicationMetadata struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`

	// Finalizers ensures deleting the Application also deletes the resources
	// it manages, rather than orphaning them — the resources-finalizer Argo
	// CD documents for exactly this.
	Finalizers []string `yaml:"finalizers,omitempty"`
}

// ApplicationSpec is the CRD's spec block.
type ApplicationSpec struct {
	Project     string                 `yaml:"project"`
	Source      ApplicationSource      `yaml:"source"`
	Destination ApplicationDestination `yaml:"destination"`
	SyncPolicy  *ApplicationSyncPolicy `yaml:"syncPolicy,omitempty"`
}

// ApplicationSource points at the Helm chart (an addon) or directory (the
// root app-of-apps) an Application syncs from.
type ApplicationSource struct {
	RepoURL        string                 `yaml:"repoURL"`
	Path           string                 `yaml:"path,omitempty"`
	Chart          string                 `yaml:"chart,omitempty"`
	TargetRevision string                 `yaml:"targetRevision,omitempty"`
	Helm           *ApplicationSourceHelm `yaml:"helm,omitempty"`
}

// ApplicationSourceHelm carries an addon's resolved values straight through:
// ValuesObject takes a structured map, so overrides applied in
// internal/catalog need no re-serialization to reach the chart.
type ApplicationSourceHelm struct {
	ValuesObject map[string]any `yaml:"valuesObject,omitempty"`
}

// ApplicationDestination is always the in-cluster API server; only the
// namespace varies per addon.
type ApplicationDestination struct {
	Server    string `yaml:"server"`
	Namespace string `yaml:"namespace"`
}

// ApplicationSyncPolicy is automated, pruning, and self-healing on every
// Application this package renders: an addon that drifted or was deleted by
// hand converges back to its Application on its own, the same way `apply`
// converges cloud infra and the repo.
type ApplicationSyncPolicy struct {
	Automated   *ApplicationSyncPolicyAutomated `yaml:"automated,omitempty"`
	SyncOptions []string                        `yaml:"syncOptions,omitempty"`
}

// ApplicationSyncPolicyAutomated configures automated sync.
type ApplicationSyncPolicyAutomated struct {
	Prune    bool `yaml:"prune"`
	SelfHeal bool `yaml:"selfHeal"`
}
