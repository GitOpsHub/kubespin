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
// extracts each ./bin/kubespin invocation, and asserts it gets far enough to
// be a real attempt: the subcommand resolves, every flag exists, and — for
// apply and delete, which validate a whole ClusterSpec up front — the spec
// the flags describe is valid.
//
// It deliberately stops short of running anything. What it cannot catch is a
// missing credential or a wrong region; what it does catch is the whole class
// of "this example was never valid" errors.

// docPrompt is the prefix every documented invocation carries.
const docPrompt = "./bin/kubespin"

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

	// flagsOnly marks an example carrying a placeholder the reader has to
	// substitute ("$GITHUB_ORG", "<subscription-id>"). Flag names are still
	// worth checking; the values are not ours to validate.
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
		return // bare `./bin/kubespin`, or a global-flag-only line
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

// commandLine matches a documented invocation and everything after it,
// including backslash-continued lines.
var commandLine = regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(docPrompt) + `((?:[^\n\\]|\\\n)*)`)

// extractInvocations pulls every ./bin/kubespin invocation out of a document,
// joining continuation lines and substituting reader-supplied placeholders so
// the flag names around them can still be checked.
func extractInvocations(doc string) []invocation {
	var out []invocation

	for _, match := range commandLine.FindAllStringSubmatch(doc, -1) {
		line := strings.ReplaceAll(match[1], "\\\n", " ")

		fields := strings.Fields(line)
		inv := invocation{args: make([]string, 0, len(fields))}
		for _, f := range fields {
			// A trailing "2>/dev/null" is shell redirection, not an argument.
			if strings.HasPrefix(f, "2>") || strings.HasPrefix(f, ">") {
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
