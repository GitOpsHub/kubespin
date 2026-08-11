package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The documentation promises that every example runs as written. That promise
// is only worth something if it is checked: the examples used to omit
// --profile and --registry-region, which meant a copy-pasted apply failed
// before making a single cloud call.
//
// This walks every prose document and every generated reference page,
// extracts each kubespin invocation, and asserts it gets far enough to be a
// real attempt: the subcommand resolves, every flag exists, and — for apply
// and delete, which validate a whole ClusterSpec up front — the spec the
// flags describe is valid.
//
// It deliberately stops short of running anything. What it cannot catch is a
// missing credential or a wrong region; what it does catch is the whole class
// of "this example was never valid" errors.

// docPrompt is the prefix every documented invocation carries. It is the bare
// binary name because that is how the docs present commands — kubespin is
// installed onto PATH by `make build`.
const docPrompt = "kubespin"

func TestDocumentedExamplesAreRunnable(t *testing.T) {
	root := repoRoot(t)

	docs := []string{filepath.Join(root, "README.md")}
	for _, pattern := range []string{"docs/*.md", "docs/cli/*.md"} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatalf("globbing %s: %v", pattern, err)
		}
		docs = append(docs, matches...)
	}

	var checked int
	for _, path := range docs {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}

		rel, _ := filepath.Rel(root, path)
		for _, inv := range extractInvocations(string(raw)) {
			checked++
			t.Run(rel+": "+strings.Join(inv.args, " "), func(t *testing.T) {
				assertInvocationParses(t, inv)
			})
		}
	}

	// A refactor that breaks extraction would otherwise turn this whole test
	// into a silent pass.
	if checked < 30 {
		t.Fatalf("only %d documented invocations found; extraction is probably broken", checked)
	}
}

// invocation is one documented command line.
type invocation struct {
	args []string

	// flagsOnly marks a line carrying a placeholder the reader has to
	// substitute ("$GITHUB_ORG", "<subscription-id>", or cobra's "[flags]").
	// Flag names are still worth checking; the values are not ours to
	// validate, and a synopsis describes no values at all.
	flagsOnly bool
}

// assertInvocationParses resolves an invocation against a fresh command tree
// and reports anything that would fail before the command did real work.
func assertInvocationParses(t *testing.T, inv invocation) {
	t.Helper()

	root := NewRootCommand()
	cmd, remaining, err := root.Find(inv.args)
	if err != nil {
		t.Fatalf("resolving subcommand: %v", err)
	}
	if cmd == root {
		return // bare `kubespin`, or a global-flag-only line
	}

	if err := cmd.ParseFlags(remaining); err != nil {
		t.Fatalf("parsing flags: %v", err)
	}
	if positional := cmd.Flags().Args(); len(positional) > 0 {
		t.Fatalf("unexpected positional arguments %q; every command takes none", positional)
	}
	if inv.flagsOnly {
		return
	}

	// apply and delete build and validate a ClusterSpec before touching
	// anything, so an example missing --profile fails there rather than in
	// the cloud. --spec points at a file that only exists in the reader's
	// checkout, so those lines can only be flag-checked.
	switch cmd.Name() {
	case "apply", "delete":
		if specPath, _ := cmd.Flags().GetString("spec"); specPath != "" {
			return
		}
		if _, err := loadSpec(cmd); err != nil {
			t.Fatalf("the flags in this example do not describe a valid cluster spec: %v", err)
		}
	}
}

// fence matches the start or end of a fenced code block.
var fence = regexp.MustCompile("^\\s*```")

// extractInvocations pulls every kubespin invocation out of a document,
// joining continuation lines and substituting reader-supplied placeholders so
// the flag names around them can still be checked.
//
// Only fenced code blocks are searched. Now that commands are written as the
// bare binary name, prose is full of sentences that begin with it ("kubespin
// apply is idempotent and resumable"), and treating those as command lines
// would fail on the following word as a stray positional argument. A code
// fence is the thing that actually distinguishes "this is a command" from
// "this is a sentence about a command".
func extractInvocations(doc string) []invocation {
	var out []invocation

	lines := strings.Split(doc, "\n")
	inFence := false

	for i := 0; i < len(lines); i++ {
		if fence.MatchString(lines[i]) {
			inFence = !inFence
			continue
		}
		if !inFence {
			continue
		}

		trimmed := strings.TrimSpace(lines[i])
		if trimmed != docPrompt && !strings.HasPrefix(trimmed, docPrompt+" ") {
			continue
		}

		// Join backslash-continued lines, which is how every multi-flag
		// example in these documents is wrapped.
		line := trimmed
		for strings.HasSuffix(line, `\`) && i+1 < len(lines) {
			i++
			line = strings.TrimSuffix(line, `\`) + " " + strings.TrimSpace(lines[i])
		}

		fields := strings.Fields(strings.TrimPrefix(line, docPrompt))
		inv := invocation{args: make([]string, 0, len(fields))}
		for _, f := range fields {
			// A trailing "2>/dev/null" is shell redirection, not an argument.
			if strings.HasPrefix(f, "2>") || strings.HasPrefix(f, ">") {
				break
			}
			// "[flags]" and "[command]" come from cobra's usage synopsis on
			// each generated reference page, which is a shape rather than a
			// runnable line. The command name in front of them is still worth
			// resolving, so the line is truncated rather than skipped — but
			// not held to describing a complete cluster spec, which a
			// synopsis by definition does not.
			if strings.HasPrefix(f, "[") {
				inv.flagsOnly = true
				break
			}

			f = strings.Trim(f, `"`)
			// "<subscription-id>" and "$GITHUB_ORG" stand in for a reader's
			// own value. Substituting keeps the surrounding flag names under
			// test without asserting anything about values we don't have.
			if strings.HasPrefix(f, "<") || strings.Contains(f, "$") {
				inv.flagsOnly = true
				f = "placeholder"
			}
			inv.args = append(inv.args, f)
		}

		if len(inv.args) == 0 {
			continue
		}
		out = append(out, inv)
	}
	return out
}

// repoRoot walks up from the package directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the package directory")
		}
		dir = parent
	}
}
