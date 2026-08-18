package kubeconfig

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// recordingRunner captures the shelled-out command instead of running it,
// mirroring internal/auth's recordingRunner.
type recordingRunner struct {
	name string
	env  []string
	args []string
	err  error
}

func (r *recordingRunner) run(_ context.Context, name string, env []string, args ...string) error {
	r.name, r.env, r.args = name, env, args
	return r.err
}

func TestUpdate_AWS(t *testing.T) {
	spec := core.ClusterSpec{ID: "demo-aws", Provider: core.ProviderAWS, Region: "us-east-1"}
	runner := &recordingRunner{}

	ctxName, err := update(t.Context(), runner.run, spec, Options{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if ctxName != "demo-aws" {
		t.Errorf("context name = %q, want demo-aws", ctxName)
	}

	if runner.name != "aws" {
		t.Errorf("name = %q, want aws", runner.name)
	}
	want := []string{
		"eks", "update-kubeconfig", "--name", "demo-aws", "--region", "us-east-1", "--alias", "demo-aws",
	}
	if !reflect.DeepEqual(runner.args, want) {
		t.Errorf("args = %v, want %v", runner.args, want)
	}
	if runner.env != nil {
		t.Errorf("env = %v, want nil", runner.env)
	}
}

func TestUpdate_AWS_WithPath(t *testing.T) {
	spec := core.ClusterSpec{ID: "demo-aws", Provider: core.ProviderAWS, Region: "us-east-1"}
	runner := &recordingRunner{}

	if _, err := update(t.Context(), runner.run, spec, Options{Path: "/tmp/kc"}); err != nil {
		t.Fatalf("update: %v", err)
	}

	want := []string{
		"eks", "update-kubeconfig", "--name", "demo-aws", "--region", "us-east-1",
		"--alias", "demo-aws", "--kubeconfig", "/tmp/kc",
	}
	if !reflect.DeepEqual(runner.args, want) {
		t.Errorf("args = %v, want %v", runner.args, want)
	}
}

func TestUpdate_GCP_Regional(t *testing.T) {
	spec := core.ClusterSpec{ID: "demo-gcp", Provider: core.ProviderGCP, Region: "us-central1"}
	runner := &recordingRunner{}

	ctxName, err := update(t.Context(), runner.run, spec, Options{GCPProject: "my-project"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if want := "gke_my-project_us-central1_demo-gcp"; ctxName != want {
		t.Errorf("context name = %q, want %q", ctxName, want)
	}

	if runner.name != "gcloud" {
		t.Errorf("name = %q, want gcloud", runner.name)
	}
	want := []string{
		"container", "clusters", "get-credentials", "demo-gcp",
		"--project", "my-project", "--region", "us-central1",
	}
	if !reflect.DeepEqual(runner.args, want) {
		t.Errorf("args = %v, want %v", runner.args, want)
	}
}

func TestUpdate_GCP_Zonal(t *testing.T) {
	spec := core.ClusterSpec{ID: "demo-gcp", Provider: core.ProviderGCP, Region: "us-central1", Zone: "us-central1-a"}
	runner := &recordingRunner{}

	ctxName, err := update(t.Context(), runner.run, spec, Options{GCPProject: "my-project", Path: "/tmp/kc"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if want := "gke_my-project_us-central1-a_demo-gcp"; ctxName != want {
		t.Errorf("context name = %q, want %q", ctxName, want)
	}

	want := []string{
		"container", "clusters", "get-credentials", "demo-gcp",
		"--project", "my-project", "--zone", "us-central1-a",
	}
	if !reflect.DeepEqual(runner.args, want) {
		t.Errorf("args = %v, want %v", runner.args, want)
	}
	wantEnv := []string{"KUBECONFIG=/tmp/kc"}
	if !reflect.DeepEqual(runner.env, wantEnv) {
		t.Errorf("env = %v, want %v", runner.env, wantEnv)
	}
}

func TestUpdate_GCP_RequiresProject(t *testing.T) {
	spec := core.ClusterSpec{ID: "demo-gcp", Provider: core.ProviderGCP, Region: "us-central1"}
	runner := &recordingRunner{}

	_, err := update(t.Context(), runner.run, spec, Options{})
	if !errors.Is(err, core.ErrInvalidSpec) {
		t.Errorf("error = %v, want one wrapping ErrInvalidSpec", err)
	}
}

func TestUpdate_Azure(t *testing.T) {
	spec := core.ClusterSpec{ID: "demo-azure", Provider: core.ProviderAzure, Region: "eastus"}
	runner := &recordingRunner{}

	ctxName, err := update(t.Context(), runner.run, spec, Options{AzureSubscription: "sub-id"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if ctxName != "demo-azure" {
		t.Errorf("context name = %q, want demo-azure", ctxName)
	}

	if runner.name != "az" {
		t.Errorf("name = %q, want az", runner.name)
	}
	want := []string{
		"aks", "get-credentials",
		"--name", "demo-azure",
		"--resource-group", "kubespin-demo-azure",
		"--subscription", "sub-id",
		"--overwrite-existing",
	}
	if !reflect.DeepEqual(runner.args, want) {
		t.Errorf("args = %v, want %v", runner.args, want)
	}
}

func TestUpdate_Azure_RequiresSubscription(t *testing.T) {
	spec := core.ClusterSpec{ID: "demo-azure", Provider: core.ProviderAzure, Region: "eastus"}
	runner := &recordingRunner{}

	_, err := update(t.Context(), runner.run, spec, Options{})
	if !errors.Is(err, core.ErrInvalidSpec) {
		t.Errorf("error = %v, want one wrapping ErrInvalidSpec", err)
	}
}

func TestUpdate_UnknownProvider(t *testing.T) {
	spec := core.ClusterSpec{ID: "demo", Provider: core.Provider("openstack")}
	runner := &recordingRunner{}

	_, err := update(t.Context(), runner.run, spec, Options{})
	if !errors.Is(err, core.ErrInvalidSpec) {
		t.Errorf("error = %v, want one wrapping ErrInvalidSpec", err)
	}
}

func TestCheckBinary_NotFound(t *testing.T) {
	err := checkBinary("kubespin-nonexistent-cli-xyz", "install it")
	if err == nil {
		t.Fatal("checkBinary: want error for missing binary")
	}
}
