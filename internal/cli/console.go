package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/GitOpsHub/kubespin/internal/orchestrator"
)

// consoleHandler renders logs for a human watching a provision go by, rather
// than for a log aggregator. slog's own TextHandler puts three fixed fields
// (time=, level=, msg=) in front of every line, which pushes the part that
// actually differs — what just happened, and to which cluster — out past
// column 60 and makes a run read as a wall of key=value:
//
//	time=2026-09-22T11:32:55.617-05:00 level=INFO msg="running step" cluster=eks-auto-dev step="create and seed repository" phase=cluster-created
//
// This handler prints the same record as fixed-width columns, so the eye can
// run down the message and the cluster instead of parsing each line:
//
//	11:32:55 ━━ Step 2/4 · create and seed repository ━━━━━━━━━━━━━━━━━━━━━ eks-auto-dev
//	11:33:08 INFO  Completed Step 2/4             eks-auto-dev         step="create and seed repository" phase=repo-pushed took=13.2s
//
// Each step opens with a header rule (a record carrying
// orchestrator.SectionLogKey), so a run reads as a list of steps with their
// records grouped under them, and a list of sentences, such as a step's
// changes, prints one bullet per line under its record:
//
//	11:38:55 INFO  Provisioned Network            eks-spot
//	               • created VPC vpc-01af752d7ad5ee6ed (10.0.0.0/16)
//	               • created subnet kubespin-eks-spot-subnet-us-east-1a (10.0.0.0/24)
//
// Messages are short Title Case labels; everything variable lives in the
// attributes. An error attribute is always moved to the end of the line and
// coloured, and a warning's or error's message takes its level's colour, so
// a failure in a long run is findable by eye rather than by grep.
//
// Colour is applied only when the output is a terminal, because the common
// way to watch a multi-cloud run is `make autopilot`, which pipes every
// stream through sed — so the plain form is the one that has to read well,
// and does. --log-format text and json are untouched for anything machine-read.
type consoleHandler struct {
	mu    *sync.Mutex
	w     io.Writer
	level slog.Leveler
	color bool

	// attrs and groups carry what WithAttrs/WithGroup accumulated, already
	// rendered, so Handle does no work per record that it can avoid. cluster
	// is a "cluster" attribute given to WithAttrs, kept out of attrs so it
	// still lands in the cluster column: the cloud provisioners log through a
	// cluster-scoped logger rather than passing the cluster on every call.
	attrs   string
	groups  []string
	cluster string
}

// Column widths. The message column fits every Info-or-above message this
// codebase logs; a longer one (a few Debug messages) simply runs over —
// overflow shifts one line's attributes right rather than truncating
// anything, since a truncated log is worse than a ragged one.
const (
	consoleMessageWidth = 30
	consoleClusterWidth = 20
)

// consoleErrorKey is the attribute moved to the end of the line and coloured.
const consoleErrorKey = "error"

// ANSI colours, kept to the 8 basic ones so they survive whatever theme the
// operator's terminal uses.
const (
	ansiReset  = "\033[0m"
	ansiDim    = "\033[2m"
	ansiBold   = "\033[1m"
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
)

// newConsoleHandler builds a consoleHandler writing to w. Colour is enabled
// only for a character device, and never when NO_COLOR is set (the
// no-color.org convention).
func newConsoleHandler(w io.Writer, level slog.Leveler) slog.Handler {
	return &consoleHandler{mu: &sync.Mutex{}, w: w, level: level, color: shouldColor(w)}
}

func shouldColor(w io.Writer) bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	clone := *h
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range attrs {
		if a.Key == "cluster" && len(h.groups) == 0 {
			clone.cluster = a.Value.String()
			continue
		}
		h.appendAttr(&b, h.groups, a)
	}
	clone.attrs = b.String()
	return &clone
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(slices.Clip(h.groups), name)
	return &clone
}

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	var line strings.Builder

	// The cluster is hoisted out of the attributes and given its own column:
	// it is the one field nearly every record carries, and the one that tells
	// three interleaved provisions apart.
	cluster, attrs := splitCluster(r)
	if cluster == "" {
		cluster = h.cluster
	}

	if section, rest := splitSection(attrs); section {
		return h.write(h.sectionHeader(r, cluster, rest))
	}

	line.WriteString(h.paint(r.Time.Format(time.TimeOnly), ansiDim))
	line.WriteByte(' ')
	line.WriteString(h.level5(r.Level))
	line.WriteByte(' ')

	line.WriteString(h.paint(pad(r.Message, consoleMessageWidth), messageColor(r.Level)))
	line.WriteByte(' ')
	line.WriteString(h.paint(pad(cluster, consoleClusterWidth), ansiBold))

	line.WriteString(h.attrs)
	var (
		errAttr *slog.Attr
		lists   []slog.Attr
	)
	for _, a := range attrs {
		if a.Key == consoleErrorKey && len(h.groups) == 0 {
			errAttr = &a
			continue
		}
		if _, ok := sentenceList(a.Value); ok {
			lists = append(lists, a)
			continue
		}
		h.appendAttr(&line, h.groups, a)
	}
	if errAttr != nil {
		line.WriteByte(' ')
		line.WriteString(h.paint(consoleErrorKey+"="+quoteIfNeeded(formatValue(errAttr.Value.Resolve())), ansiRed))
	}
	out := strings.TrimRight(line.String(), " ") + "\n"

	// A list of sentences (a step's "changes") reads as one bullet per line
	// under its record rather than a single line hundreds of columns wide.
	for _, a := range lists {
		items, _ := sentenceList(a.Value)
		for _, item := range items {
			out += consoleListIndent + h.paint("•", ansiDim) + " " + item + "\n"
		}
	}

	return h.write(out)
}

// consoleListIndent lines a sentence list's bullets up under the message
// column: past the time (8), the level (5), and the space after each.
const consoleListIndent = "               "

// consoleSectionWidth is how wide a section header's rule runs before the
// cluster, so every step's header lines up.
const consoleSectionWidth = 64

// consoleSectionKey marks a record as the start of a section (a provisioning
// step): the orchestrator sets it on "Starting Step", and the console renders
// that record as a header rule the step's records group under. Other handlers
// print it as an ordinary attribute.
const consoleSectionKey = orchestrator.SectionLogKey

// splitSection reports whether attrs mark a section start, returning attrs
// without the marker.
func splitSection(attrs []slog.Attr) (bool, []slog.Attr) {
	for i, a := range attrs {
		if a.Key == consoleSectionKey && a.Value.Kind() == slog.KindBool && a.Value.Bool() {
			return true, append(slices.Clip(attrs[:i]), attrs[i+1:]...)
		}
	}
	return false, attrs
}

// sectionHeader renders a section start as a blank line then a rule naming
// it, e.g. "11:32:55 ━━ Step 1/4 · create cluster ━━━━━━━━━━ eks-auto-dev".
// The "Starting " prefix is dropped (the rule already says a step starts
// here) and the step attribute is folded into the title.
func (h *consoleHandler) sectionHeader(r slog.Record, cluster string, attrs []slog.Attr) string {
	title := strings.TrimPrefix(r.Message, "Starting ")
	var rest strings.Builder
	for _, a := range attrs {
		if a.Key == "step" {
			title += " · " + a.Value.String()
			continue
		}
		h.appendAttr(&rest, h.groups, a)
	}

	rule := "━━ " + title + " "
	if n := consoleSectionWidth - utf8.RuneCountInString(rule); n > 0 {
		rule += strings.Repeat("━", n)
	}

	header := "\n" + h.paint(r.Time.Format(time.TimeOnly), ansiDim) + " " + h.paint(rule, ansiBold+ansiCyan)
	if cluster != "" {
		header += " " + h.paint(cluster, ansiBold)
	}
	return header + rest.String() + "\n"
}

// sentenceList returns v's items when v is a list of sentences — a []string
// with an item containing a space, like a step's "changes". A list of single
// words (instance types, provider names) stays inline, where it reads fine.
func sentenceList(v slog.Value) ([]string, bool) {
	v = v.Resolve()
	if v.Kind() != slog.KindAny {
		return nil, false
	}
	items, ok := v.Any().([]string)
	if !ok || len(items) == 0 {
		return nil, false
	}
	for _, item := range items {
		if strings.Contains(item, " ") {
			return items, true
		}
	}
	return nil, false
}

// write emits one rendered record, which may span several lines.
func (h *consoleHandler) write(s string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := io.WriteString(h.w, s); err != nil {
		return fmt.Errorf("writing log line: %w", err)
	}
	return nil
}

// splitCluster pulls the "cluster" attribute out of a record, returning it
// and everything else. Nested groups are left alone: a cluster buried in a
// group is not the top-level identifier this column is for.
func splitCluster(r slog.Record) (cluster string, rest []slog.Attr) {
	rest = make([]slog.Attr, 0, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "cluster" && cluster == "" {
			cluster = a.Value.String()
			return true
		}
		rest = append(rest, a)
		return true
	})
	return cluster, rest
}

// messageColor highlights the message of a record worth noticing: routine
// progress stays plain, so the eye lands on what went wrong.
func messageColor(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return ansiBold + ansiRed
	case level >= slog.LevelWarn:
		return ansiYellow
	default:
		return ""
	}
}

// level5 renders the level in a fixed five-column field, coloured by
// severity so a warning or an error is findable by eye in a long run.
func (h *consoleHandler) level5(level slog.Level) string {
	var name, color string
	switch {
	case level < slog.LevelInfo:
		name, color = "DEBUG", ansiDim
	case level < slog.LevelWarn:
		name, color = "INFO", ansiCyan
	case level < slog.LevelError:
		name, color = "WARN", ansiYellow
	default:
		name, color = "ERROR", ansiRed
	}
	return h.paint(pad(name, 5), color)
}

// appendAttr renders one attribute as " key=value", resolving groups into
// dotted keys and quoting values that would otherwise be ambiguous.
func (h *consoleHandler) appendAttr(b *strings.Builder, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}

	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		if len(attrs) == 0 {
			return
		}
		nested := groups
		if a.Key != "" {
			nested = append(slices.Clip(groups), a.Key)
		}
		for _, nestedAttr := range attrs {
			h.appendAttr(b, nested, nestedAttr)
		}
		return
	}

	key := a.Key
	if len(groups) > 0 {
		key = strings.Join(groups, ".") + "." + key
	}

	b.WriteByte(' ')
	b.WriteString(h.paint(key+"=", ansiDim))
	b.WriteString(quoteIfNeeded(formatValue(a.Value)))
}

// formatValue renders a resolved value for a human: durations rounded to a
// readable precision (a step's "took=12m3.481920317s" is noise past the
// second) and times to the second, without the monotonic clock suffix.
func formatValue(v slog.Value) string {
	switch v.Kind() {
	case slog.KindDuration:
		return roundDuration(v.Duration()).String()
	case slog.KindTime:
		return v.Time().Format(time.RFC3339)
	default:
		return v.String()
	}
}

func roundDuration(d time.Duration) time.Duration {
	switch {
	case d >= time.Minute:
		return d.Round(time.Second)
	case d >= time.Second:
		return d.Round(100 * time.Millisecond)
	default:
		return d.Round(time.Millisecond)
	}
}

// quoteIfNeeded quotes a value containing whitespace or a quote, so a
// multi-word value cannot be misread as two attributes. Everything else is
// left bare — quoting every value is what makes slog's text output hard to
// scan.
func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t\n\"=") {
		return strconv.Quote(s)
	}
	return s
}

// pad right-pads s to width, leaving anything longer intact.
func pad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// paint wraps s in an ANSI colour when colour is enabled. The padding is
// applied by the caller before painting, so the escape sequences never count
// towards a column's width.
func (h *consoleHandler) paint(s, color string) string {
	if !h.color || s == "" || color == "" {
		return s
	}
	return color + s + ansiReset
}
