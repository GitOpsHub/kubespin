package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name       string
		subject    string
		body       string
		wantBucket string
		wantBump   string
	}{
		{"feat", "feat: add spot instance support", "", "Added", "minor"},
		{"fix", "fix(cli): correct flag parsing", "", "Fixed", "patch"},
		{"perf", "perf: speed up reconcile loop", "", "Changed", "patch"},
		{"chore", "chore: bump dependency", "", "Changed", ""},
		{"non-conventional", "Refactor registry implementation", "", "Changed", ""},
		{"breaking bang", "feat!: drop legacy profile support", "", "Added", "major"},
		{"breaking footer", "fix: change registry schema", "BREAKING CHANGE: schema v2 required", "Fixed", "major"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, bump := classify(commit{subject: tc.subject, body: tc.body})
			if bucket != tc.wantBucket {
				t.Errorf("bucket = %q, want %q", bucket, tc.wantBucket)
			}
			if bump != tc.wantBump {
				t.Errorf("bump = %q, want %q", bump, tc.wantBump)
			}
		})
	}
}

func TestNextVersion(t *testing.T) {
	cases := []struct {
		last, bump, want string
	}{
		{"", "minor", "v0.1.0"},
		{"v0.1.0", "patch", "v0.1.1"},
		{"v0.1.5", "minor", "v0.2.0"},
		{"v1.2.3", "major", "v2.0.0"},
	}

	for _, tc := range cases {
		got, err := nextVersion(tc.last, tc.bump)
		if err != nil {
			t.Fatalf("nextVersion(%q, %q) error: %v", tc.last, tc.bump, err)
		}
		if got != tc.want {
			t.Errorf("nextVersion(%q, %q) = %q, want %q", tc.last, tc.bump, got, tc.want)
		}
	}
}

func TestMaxBump(t *testing.T) {
	if got := maxBump("patch", "minor"); got != "minor" {
		t.Errorf("maxBump(patch, minor) = %q, want minor", got)
	}
	if got := maxBump("major", "minor"); got != "major" {
		t.Errorf("maxBump(major, minor) = %q, want major", got)
	}
	if got := maxBump("", "patch"); got != "patch" {
		t.Errorf("maxBump(\"\", patch) = %q, want patch", got)
	}
}

func TestSection(t *testing.T) {
	commits := []commit{
		{subject: "feat: add GKE spot pools"},
		{subject: "fix: correct subnet tagging"},
		{subject: "chore: tidy go.mod"},
	}

	got := section(commits)
	want := "### Added\n- Add GKE spot pools\n\n### Fixed\n- Correct subnet tagging\n\n### Changed\n- Tidy go.mod"
	if got != want {
		t.Errorf("section() = %q, want %q", got, want)
	}
}

func TestSectionEmpty(t *testing.T) {
	if got := section(nil); got != "" {
		t.Errorf("section(nil) = %q, want empty", got)
	}
}

func TestRunExtract(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, changelogPath)
	content := `# Changelog

## [Unreleased]

## [v0.2.0] - 2026-01-02
### Added
- Second thing

## [v0.1.0] - 2026-01-01
### Added
- First thing
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	restore := chdir(t, dir)
	defer restore()

	// runExtract prints to stdout; verify indirectly by checking it doesn't
	// error for an existing version and does for a missing one.
	if err := runExtract([]string{"v0.2.0"}); err != nil {
		t.Errorf("runExtract(v0.2.0) error: %v", err)
	}
	if err := runExtract([]string{"v9.9.9"}); err == nil {
		t.Errorf("runExtract(v9.9.9) expected error, got nil")
	}
}

func chdir(t *testing.T, dir string) func() {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Chdir(old) }
}
