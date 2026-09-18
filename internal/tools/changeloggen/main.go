// Command changeloggen derives the next SemVer tag and CHANGELOG.md entry
// from Conventional Commit subjects (git log) since the last tag.
//
// It has three subcommands:
//
//	next               prints the next vX.Y.Z tag, or exits 1 with no output
//	                    if nothing since the last tag warrants a release
//	render -version v  prepends a dated section for that version into
//	                    CHANGELOG.md, built from commits since the last tag
//	extract v          prints just that version's section body, for use as
//	                    a GitHub Release notes body
//
// This mirrors internal/tools/docsgen: a small generator run from the
// Makefile and CI rather than maintained by hand.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const changelogPath = "CHANGELOG.md"

const unreleasedMarker = "## [Unreleased]"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "changeloggen: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: changeloggen <next|render|extract> [flags]")
	}

	switch args[0] {
	case "next":
		return runNext()
	case "render":
		return runRender(args[1:])
	case "extract":
		return runExtract(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

// commit is one entry from `git log`, subject and body kept separate so a
// BREAKING CHANGE footer can be detected even when it isn't on the subject
// line.
type commit struct {
	subject string
	body    string
}

// recordSep/fieldSep are ASCII record/unit separators: they cannot appear in
// a commit message, so splitting on them is safe without escaping.
const (
	fieldSep  = "\x1f"
	recordSep = "\x1e"
)

// commitsSince returns non-merge commits after tag (exclusive), or every
// non-merge commit in the repo if tag is empty.
func commitsSince(tag string) ([]commit, error) {
	rng := "HEAD"
	if tag != "" {
		rng = tag + "..HEAD"
	}
	out, err := exec.Command("git", "log", "--no-merges",
		"--pretty=format:%s"+fieldSep+"%b"+recordSep, rng).Output()
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	var commits []commit
	for rec := range strings.SplitSeq(string(out), recordSep) {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, fieldSep, 2)
		c := commit{subject: strings.TrimSpace(parts[0])}
		if len(parts) == 2 {
			c.body = parts[1]
		}
		if c.subject == "" {
			continue
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// lastTag returns the most recent reachable tag, or "" if none exists yet.
func lastTag() (string, error) {
	out, err := exec.Command("git", "describe", "--tags", "--abbrev=0").Output()
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			return "", nil // no tags yet
		}
		return "", fmt.Errorf("git describe: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// conventionalRe matches a Conventional Commits subject: type(scope)!: subject
var conventionalRe = regexp.MustCompile(`^(\w+)(\([^)]*\))?(!)?:\s*(.+)$`)

// classify buckets a commit for the changelog and reports the SemVer bump it
// warrants ("major", "minor", "patch", or "" for none).
func classify(c commit) (bucket, bump string) {
	breaking := strings.Contains(c.body, "BREAKING CHANGE")

	m := conventionalRe.FindStringSubmatch(c.subject)
	if m == nil {
		bucket = "Changed"
	} else {
		if m[3] == "!" {
			breaking = true
		}
		switch m[1] {
		case "feat":
			bucket = "Added"
		case "fix":
			bucket = "Fixed"
		default:
			bucket = "Changed"
		}
	}

	switch {
	case breaking:
		bump = "major"
	case m != nil && m[1] == "feat":
		bump = "minor"
	case m != nil && (m[1] == "fix" || m[1] == "perf"):
		bump = "patch"
	}
	return bucket, bump
}

// displayText strips a matched Conventional Commits prefix so the changelog
// reads as prose rather than repeating "feat: "/"fix: " on every bullet.
func displayText(c commit) string {
	m := conventionalRe.FindStringSubmatch(c.subject)
	if m == nil {
		return c.subject
	}
	msg := m[4]
	return strings.ToUpper(msg[:1]) + msg[1:]
}

var bumpRank = map[string]int{"": 0, "patch": 1, "minor": 2, "major": 3}

func maxBump(a, b string) string {
	if bumpRank[b] > bumpRank[a] {
		return b
	}
	return a
}

// nextVersion applies bump to the last tag. An empty last tag always starts
// the project at v0.1.0, regardless of what the commits since the beginning
// of history would otherwise imply.
func nextVersion(last, bump string) (string, error) {
	if last == "" {
		return "v0.1.0", nil
	}
	major, minor, patch, err := parseSemVer(last)
	if err != nil {
		return "", err
	}
	switch bump {
	case "major":
		major, minor, patch = major+1, 0, 0
	case "minor":
		minor, patch = minor+1, 0
	case "patch":
		patch++
	default:
		return "", fmt.Errorf("no bump for version %q", last)
	}
	return fmt.Sprintf("v%d.%d.%d", major, minor, patch), nil
}

func parseSemVer(tag string) (major, minor, patch int, err error) {
	s := strings.TrimPrefix(tag, "v")
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("tag %q is not vMAJOR.MINOR.PATCH", tag)
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("tag %q is not vMAJOR.MINOR.PATCH: %w", tag, err)
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], nil
}

func runNext() error {
	last, err := lastTag()
	if err != nil {
		return err
	}
	commits, err := commitsSince(last)
	if err != nil {
		return err
	}

	bump := ""
	for _, c := range commits {
		_, b := classify(c)
		bump = maxBump(bump, b)
	}
	if bump == "" {
		return fmt.Errorf("no release-worthy commits since %s", orNone(last))
	}

	next, err := nextVersion(last, bump)
	if err != nil {
		return err
	}
	fmt.Println(next)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "the beginning of history"
	}
	return s
}

// section renders the grouped, bucketed body for a set of commits — the
// shape shared by both `render` (writing it into CHANGELOG.md) and the
// standalone preview.
func section(commits []commit) string {
	groups := map[string][]string{}
	order := []string{"Added", "Fixed", "Changed"}
	for _, c := range commits {
		bucket, _ := classify(c)
		groups[bucket] = append(groups[bucket], displayText(c))
	}

	var b strings.Builder
	for _, bucket := range order {
		items := groups[bucket]
		if len(items) == 0 {
			continue
		}
		sort.Strings(items)
		fmt.Fprintf(&b, "### %s\n", bucket)
		for _, item := range items {
			fmt.Fprintf(&b, "- %s\n", item)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func runRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	version := fs.String("version", "", "version to render, e.g. v1.2.3")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *version == "" {
		return fmt.Errorf("-version is required")
	}

	last, err := lastTag()
	if err != nil {
		return err
	}
	commits, err := commitsSince(last)
	if err != nil {
		return err
	}
	body := section(commits)
	if body == "" {
		return fmt.Errorf("no commits since %s to render into %s", orNone(last), *version)
	}

	raw, err := os.ReadFile(changelogPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", changelogPath, err)
	}
	content := string(raw)

	idx := strings.Index(content, unreleasedMarker)
	if idx == -1 {
		return fmt.Errorf("%s has no %q marker", changelogPath, unreleasedMarker)
	}
	insertAt := idx + len(unreleasedMarker)

	heading := fmt.Sprintf("\n\n## [%s] - %s\n", *version, time.Now().UTC().Format("2006-01-02"))
	newContent := content[:insertAt] + heading + body + "\n" + content[insertAt:]

	if err := os.WriteFile(changelogPath, []byte(newContent), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", changelogPath, err)
	}
	return nil
}

func runExtract(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: changeloggen extract vX.Y.Z")
	}
	version := args[0]

	raw, err := os.ReadFile(changelogPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", changelogPath, err)
	}
	content := string(raw)

	heading := fmt.Sprintf("## [%s]", version)
	start := strings.Index(content, heading)
	if start == -1 {
		return fmt.Errorf("%s has no section for %s", changelogPath, version)
	}
	// Skip past the heading line itself.
	rest := content[start:]
	if nl := strings.Index(rest, "\n"); nl != -1 {
		rest = rest[nl+1:]
	} else {
		rest = ""
	}

	end := strings.Index(rest, "\n## [")
	if end != -1 {
		rest = rest[:end]
	}

	fmt.Println(strings.TrimSpace(rest))
	return nil
}
