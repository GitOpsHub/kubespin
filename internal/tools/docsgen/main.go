// Command docsgen renders the CLI reference under docs/cli from the live cobra
// command tree.
//
// The reference is generated rather than written so it cannot drift: CI
// regenerates it and fails if the result differs from what is committed.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	return polishAll(outputDir)
}

// polishAll rewrites every generated page into the shape the hand-written
// documentation uses. Doing it here rather than by hand keeps the reference
// generated — the property that stops it drifting from the command tree —
// while still letting it sit alongside the prose pages without looking like
// output from a different project.
func polishAll(dir string) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return fmt.Errorf("listing generated pages: %w", err)
	}

	for _, path := range paths {
		raw, err := os.ReadFile(path) //nolint:gosec // path comes from our own Glob of outputDir
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		if err := os.WriteFile(path, []byte(polish(string(raw))), 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return nil
}

// polish applies the per-page rewrites.
//
// Three things cobra emits do not match the rest of the site:
//
//   - The page title is an H2, so the page has no H1 at all. MkDocs derives a
//     page's title and its table of contents from the H1, so every reference
//     page arrived untitled and with its sections one level too deep.
//   - Code fences carry no language, so nothing is syntax-highlighted — the
//     examples are the part of these pages people actually read.
//   - "SEE ALSO" is shouted, where every hand-written heading is sentence case.
func polish(md string) string {
	lines := strings.Split(md, "\n")
	out := make([]string, 0, len(lines))

	var section string
	inFence := false

	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "```"):
			if !inFence {
				out = append(out, "```"+fenceLanguage(section))
			} else {
				out = append(out, "```")
			}
			inFence = !inFence
			continue

		case inFence:
			// Never rewrite anything inside a code block: a "###" there is a
			// shell comment, not a heading.

		case strings.HasPrefix(line, "### "):
			section = strings.TrimPrefix(line, "### ")
			if section == "SEE ALSO" {
				section = "See also"
			}
			out = append(out, "## "+section)
			continue

		case strings.HasPrefix(line, "## "):
			// Cobra's page title. Promoted to the H1 the page is missing.
			out = append(out, "# "+strings.TrimPrefix(line, "## "))
			continue

		case strings.HasPrefix(line, "* ["):
			// "See also" entries separate link and description with a tab,
			// which renders as a ragged gap.
			out = append(out, strings.ReplaceAll(line, "\t", ""))
			continue
		}

		out = append(out, line)
	}

	return strings.Join(out, "\n")
}

// fenceLanguage picks the highlighter for a code block by the section it sits
// in. Only the examples are shell; the usage synopsis and the option lists are
// output shapes, and highlighting them as shell mis-colours "[flags]" and the
// default values.
func fenceLanguage(section string) string {
	if section == "Examples" {
		return "bash"
	}
	return "text"
}

func disableAutoGenTag(cmd *cobra.Command) {
	cmd.DisableAutoGenTag = true
	for _, child := range cmd.Commands() {
		disableAutoGenTag(child)
	}
}
