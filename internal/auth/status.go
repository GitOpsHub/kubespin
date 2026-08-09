package auth

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// WriteTable renders results the way `login`/`status`/`logout` show them:
// one line per provider, a checkmark or cross, and whatever detail
// IsAuthenticated reported.
func WriteTable(w io.Writer, results []Result) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()

	for _, r := range results {
		mark := "✓"
		detail := r.Status.Message

		switch {
		case r.Err != nil:
			mark = "✗"
			detail = r.Err.Error()
		case !r.Authenticated:
			mark = "✗"
			if detail == "" {
				detail = fmt.Sprintf("not authenticated — run: kubespin login --only %s", strings.ToLower(r.Provider))
			}
		case r.Status.ExpiresAt != nil:
			detail = fmt.Sprintf("%s (session expires in %s)", detail, time.Until(*r.Status.ExpiresAt).Round(time.Minute))
		}

		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", strings.ToUpper(r.Provider), mark, detail)
	}
}
