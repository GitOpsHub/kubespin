// Command docsgen renders the CLI reference under docs/cli from the live cobra
// command tree.
//
// The reference is generated rather than written so it cannot drift: CI
// regenerates it and fails if the result differs from what is committed.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"github.com/GitOpsHub/kubespin/internal/cli"
)

const outputDir = "docs/cli"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "docsgen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", outputDir, err)
	}

	root := cli.NewRootCommand()

	// Without this, cobra stamps the current date into every file and the
	// generated output differs from one day to the next — which would make the
	// CI drift check fail for no reason.
	disableAutoGenTag(root)

	// The version string carries the build commit, which would otherwise churn
	// the root page on every rebuild.
	root.Version = ""

	if err := doc.GenMarkdownTree(root, outputDir); err != nil {
		return fmt.Errorf("generating markdown: %w", err)
	}
	return nil
}

func disableAutoGenTag(cmd *cobra.Command) {
	cmd.DisableAutoGenTag = true
	for _, child := range cmd.Commands() {
		disableAutoGenTag(child)
	}
}
