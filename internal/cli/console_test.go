package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func consoleLogger(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	return slog.New(newConsoleHandler(buf, level))
}

// The whole point of the handler: the message and the cluster land in fixed
// columns, so a run can be read down rather than across.
func TestConsoleHandler_AlignsColumns(t *testing.T) {
	var buf bytes.Buffer
	l := consoleLogger(&buf, slog.LevelInfo)

	l.Info("Registered Cluster", "cluster", "eks-auto-dev", "phase", "pending")
	l.Info("Completed Step", "cluster", "eks-auto-dev", "phase", "ready")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}

	first := strings.Index(lines[0], "eks-auto-dev")
	second := strings.Index(lines[1], "eks-auto-dev")
	if first != second {
		t.Errorf("cluster column starts at %d then %d; the columns must line up", first, second)
	}
	for _, line := range lines {
		if !strings.HasSuffix(line, "phase=pending") && !strings.HasSuffix(line, "phase=ready") {
			t.Errorf("attributes did not land after the cluster column: %q", line)
		}
	}
}

// The cluster is hoisted out of the attributes rather than printed twice.
func TestConsoleHandler_HoistsCluster(t *testing.T) {
	var buf bytes.Buffer
	consoleLogger(&buf, slog.LevelInfo).Info("Starting Step", "cluster", "gke-spot-dev", "step", "create cluster")

	got := buf.String()
	if !strings.Contains(got, "gke-spot-dev") {
		t.Fatalf("cluster missing from %q", got)
	}
	if strings.Contains(got, "cluster=") {
		t.Errorf("cluster printed as an attribute as well as a column: %q", got)
	}
}

// A record with no cluster still has to produce a well-formed line — the auth
// and config paths log plenty of them.
func TestConsoleHandler_WithoutACluster(t *testing.T) {
	var buf bytes.Buffer
	consoleLogger(&buf, slog.LevelInfo).Info("Logging In", "providers", "aws")

	got := strings.TrimSpace(buf.String())
	if !strings.Contains(got, "Logging In") || !strings.HasSuffix(got, "providers=aws") {
		t.Errorf("line = %q, want the message and its attributes", got)
	}
	if strings.HasSuffix(got, " ") {
		t.Error("line kept trailing padding from the empty cluster column")
	}
}

func TestConsoleHandler_Levels(t *testing.T) {
	for _, tc := range []struct {
		log  func(*slog.Logger)
		want string
	}{
		{func(l *slog.Logger) { l.Debug("m") }, "DEBUG"},
		{func(l *slog.Logger) { l.Info("m") }, "INFO"},
		{func(l *slog.Logger) { l.Warn("m") }, "WARN"},
		{func(l *slog.Logger) { l.Error("m") }, "ERROR"},
	} {
		var buf bytes.Buffer
		tc.log(consoleLogger(&buf, slog.LevelDebug))
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("line %q does not carry level %s", buf.String(), tc.want)
		}
	}
}

func TestConsoleHandler_RespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	l := consoleLogger(&buf, slog.LevelInfo)

	l.Debug("registry chatter", "cluster", "eks-auto-dev")
	if buf.Len() != 0 {
		t.Errorf("debug record emitted at info level: %q", buf.String())
	}
}

// Writing to anything that is not a terminal must stay plain: the usual way
// to watch a multi-cloud run pipes every stream through sed.
func TestConsoleHandler_NoColorWhenNotATerminal(t *testing.T) {
	var buf bytes.Buffer
	consoleLogger(&buf, slog.LevelInfo).Info("Completed Step", "cluster", "eks-auto-dev")

	if strings.Contains(buf.String(), "\033[") {
		t.Errorf("ANSI escapes written to a non-terminal: %q", buf.String())
	}
}

// A value with spaces must not be readable as two attributes; one without
// must not be cluttered with quotes.
func TestConsoleHandler_QuotesOnlyWhenNeeded(t *testing.T) {
	var buf bytes.Buffer
	consoleLogger(&buf, slog.LevelInfo).Info("Starting Step",
		"step", "create and seed repository", "phase", "cluster-created", "empty", "")

	got := buf.String()
	for _, want := range []string{`step="create and seed repository"`, `phase=cluster-created`, `empty=""`} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q is missing %s", got, want)
		}
	}
}

func TestConsoleHandler_WithAttrsAndGroups(t *testing.T) {
	var buf bytes.Buffer
	l := consoleLogger(&buf, slog.LevelInfo).With("run", "abc123").WithGroup("cloud")

	l.Info("Created VPC", "vpc", "vpc-01", "cidr", "10.0.0.0/16")

	got := buf.String()
	for _, want := range []string{"run=abc123", "cloud.vpc=vpc-01", "cloud.cidr=10.0.0.0/16"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q is missing %s", got, want)
		}
	}
}

// slog requires an empty attr to be dropped and a group's members to be
// inlined under a dotted key; a handler that gets this wrong prints "!BADKEY"
// noise on records other packages legitimately produce.
func TestConsoleHandler_EmptyAndGroupAttrs(t *testing.T) {
	var buf bytes.Buffer
	h := newConsoleHandler(&buf, slog.LevelInfo)

	r := slog.Record{Level: slog.LevelInfo, Message: "m"}
	r.AddAttrs(
		slog.Attr{},
		slog.Group("lease", slog.String("holder", "mac/1"), slog.Int("ttl", 15)),
		slog.Group("empty"),
	)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "lease.holder=mac/1") || !strings.Contains(got, "lease.ttl=15") {
		t.Errorf("group attrs not inlined: %q", got)
	}
	if strings.Contains(got, "empty") || strings.Contains(got, "BADKEY") {
		t.Errorf("empty attr or group leaked into the line: %q", got)
	}
}

// Config.Logger has to honour every documented format, and default to console.
func TestConfigLogger_Formats(t *testing.T) {
	for _, format := range []string{"console", "text", "json"} {
		c := &Config{LogLevel: "info", LogFormat: format}
		if err := c.validate(); err != nil {
			t.Errorf("validate(%s): %v", format, err)
		}
	}

	c := &Config{LogLevel: "info", LogFormat: "yaml"}
	if err := c.validate(); err == nil {
		t.Error("validate accepted an unknown log format")
	}
	if defaultLogFormat != "console" {
		t.Errorf("defaultLogFormat = %q, want console", defaultLogFormat)
	}
}

// An error is what a reader scans a failed run for, so it always lands at the
// end of the line, whatever order the caller passed it in.
func TestConsoleHandler_ErrorLast(t *testing.T) {
	var buf bytes.Buffer
	consoleLogger(&buf, slog.LevelInfo).Error("Step Failed 1/4",
		"cluster", "eks-auto-dev", "error", errors.New("boom: quota exceeded"), "step", "create cluster")

	got := strings.TrimSpace(buf.String())
	if !strings.HasSuffix(got, `error="boom: quota exceeded"`) {
		t.Errorf("error attr is not last: %q", got)
	}
	if strings.Count(got, "error=") != 1 {
		t.Errorf("error attr printed more than once: %q", got)
	}
}

// Durations are rounded for a human; nanosecond precision on a multi-minute
// step is noise.
func TestConsoleHandler_RoundsDurations(t *testing.T) {
	for d, want := range map[time.Duration]string{
		12*time.Minute + 3481920317*time.Nanosecond: "took=12m3s",
		13*time.Second + 234*time.Millisecond:       "took=13.2s",
		42*time.Millisecond + 7*time.Microsecond:    "took=42ms",
	} {
		var buf bytes.Buffer
		consoleLogger(&buf, slog.LevelInfo).Info("Completed Step", "took", d)
		if !strings.HasSuffix(strings.TrimSpace(buf.String()), want) {
			t.Errorf("%v rendered as %q, want %s", d, buf.String(), want)
		}
	}
}
