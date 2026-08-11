package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/GitOpsHub/kubespin/internal/core"
)

// TestConfirmDelete covers the prompt guarding an irreversible teardown.
// Anything but the exact cluster ID must decline, and declining must not look
// like a command failure.
func TestConfirmDelete(t *testing.T) {
	spec := core.ClusterSpec{ID: "demo-aws"}

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"the cluster id confirms", "demo-aws\n", true},
		{"surrounding whitespace is tolerated", "  demo-aws  \n", true},
		{"a bare newline declines", "\n", false},
		{"EOF declines", "", false},
		{"a different cluster id declines", "demo-gcp\n", false},
		{"yes is not enough", "yes\n", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetIn(strings.NewReader(tc.input))
			cmd.SetOut(&strings.Builder{})

			got, err := confirmDelete(cmd, spec)
			if err != nil {
				t.Fatalf("confirmDelete: %v, want declining to be a clean outcome", err)
			}
			if got != tc.want {
				t.Errorf("confirmDelete = %v, want %v", got, tc.want)
			}
		})
	}
}
