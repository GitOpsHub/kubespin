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
//	11:32:55 INFO  Starting Step 2/4              eks-auto-dev         step="create and seed repository"
//	11:33:08 INFO  Completed Step 2/4             eks-auto-dev         step="create and seed repository" phase=repo-pushed took=13.2s
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
	// rendered, so Handle does no work per record that it can avoid.
	attrs  string
	groups []string
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

	line.WriteString(h.paint(r.Time.Format(time.TimeOnly), ansiDim))
	line.WriteByte(' ')
	line.WriteString(h.level5(r.Level))
	line.WriteByte(' ')

	// The cluster is hoisted out of the attributes and given its own column:
	// it is the one field nearly every record carries, and the one that tells
	// three interleaved provisions apart.
	cluster, attrs := splitCluster(r)

	line.WriteString(h.paint(pad(r.Message, consoleMessageWidth), messageColor(r.Level)))
	line.WriteByte(' ')
	line.WriteString(h.paint(pad(cluster, consoleClusterWidth), ansiBold))

	line.WriteString(h.attrs)
	var errAttr *slog.Attr
	for _, a := range attrs {
		if a.Key == consoleErrorKey && len(h.groups) == 0 {
			errAttr = &a
			continue
		}
		h.appendAttr(&line, h.groups, a)
	}
	if errAttr != nil {
		line.WriteByte(' ')
		line.WriteString(h.paint(consoleErrorKey+"="+quoteIfNeeded(formatValue(errAttr.Value.Resolve())), ansiRed))
	}

	line.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, strings.TrimRight(line.String(), " \n")+"\n")
	if err != nil {
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
