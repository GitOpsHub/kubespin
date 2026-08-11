package cli

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/fleet"
)

func newFleetDashboardCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Render a static HTML snapshot of fleet sync status, drift, and staleness",
		Long: `dashboard reads the Fleet Registry — the same data 'fleet status' reads — and
renders it as a single self-contained HTML file: no server, no external
assets, nothing to deploy. Open the file in a browser, or publish it wherever
static files already get served.

Rows are correlated by cluster ID, which is what the Fleet Registry and every
cluster's own repository already share. A per-cluster commit SHA is not
included: the registry does not track one (that lives in each cluster's own
.state.yaml), so showing it here would need a repository read per cluster
this command does not otherwise make.

Like 'fleet status', this never connects to a cluster — everything shown
comes from what fleet-status-reporter and 'fleet audit' have already pushed
into the registry.`,
		Example: `  # Snapshot the whole fleet to fleet-dashboard.html
  kubespin fleet dashboard --registry-region us-east-1

  # Only AWS clusters, written somewhere else
  kubespin fleet dashboard --provider aws --output /tmp/fleet.html \
    --registry-region us-east-1`,
		Args: cobra.NoArgs,
		RunE: runFleetDashboard,
	}

	fs := cmd.Flags()
	fs.String("provider", "", "restrict to one cloud provider")
	fs.String("phase", "", "restrict to clusters in one phase")
	fs.String("output", "fleet-dashboard.html", "path to write the rendered HTML snapshot to")
	fs.Duration("stale-threshold", fleet.DefaultStaleThreshold, "how long a cluster may go without reporting before it is stale")

	return cmd
}

func runFleetDashboard(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	_, reg, err := registryPrereqs(cmd)
	if err != nil {
		return err
	}

	filter, err := fleetFilter(cmd, "provider")
	if err != nil {
		return err
	}
	phase, err := cmd.Flags().GetString("phase")
	if err != nil {
		return fmt.Errorf("reading --phase: %w", err)
	}
	filter.Phase = core.Phase(phase)

	threshold, err := cmd.Flags().GetDuration("stale-threshold")
	if err != nil {
		return fmt.Errorf("reading --stale-threshold: %w", err)
	}

	now := time.Now()
	rows, err := fleet.Dashboard(ctx, reg, filter, threshold, now, fleet.WithLogger(LoggerFrom(ctx)))
	if err != nil {
		return fmt.Errorf("running fleet dashboard: %w", err)
	}

	output, err := cmd.Flags().GetString("output")
	if err != nil {
		return fmt.Errorf("reading --output: %w", err)
	}

	html, err := renderDashboardHTML(rows, now)
	if err != nil {
		return fmt.Errorf("rendering dashboard: %w", err)
	}
	if err := os.WriteFile(output, html, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", output, err)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "wrote %d cluster(s) to %s\n", len(rows), output)
	return nil
}

// dashboardData is the template's view of a fleet.Dashboard result.
type dashboardData struct {
	GeneratedAt string
	Rows        []dashboardRowView
	Total       int
	Stale       int
	Drifted     int
}

type dashboardRowView struct {
	ClusterID      string
	Provider       string
	Phase          string
	Stale          bool
	LastReportedAt string
	Findings       []string
	FindingsAt     string
	Audited        bool
}

// dashboardTemplate renders entirely inline — no CDN, no separate CSS/JS
// file — so the output is a single portable HTML file, matching the "no
// server" promise the command makes.
var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>kubespin fleet dashboard</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 2rem; }
  h1 { font-size: 1.25rem; }
  .meta { color: #666; margin-bottom: 1.5rem; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: left; padding: 0.5rem 0.75rem; border-bottom: 1px solid #ccc4; vertical-align: top; }
  th { font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.03em; color: #888; }
  .badge { display: inline-block; padding: 0.1rem 0.5rem; border-radius: 999px; font-size: 0.8rem; }
  .badge-ok { background: #1a7f371a; color: #1a7f37; }
  .badge-warn { background: #9a67001a; color: #9a6700; }
  .badge-bad { background: #cf222e1a; color: #cf222e; }
  .badge-muted { background: #6666661a; color: #666; }
  .findings { margin: 0; padding-left: 1.1rem; }
  .summary { display: flex; gap: 1.5rem; margin-bottom: 1.5rem; }
  .summary div { font-size: 0.9rem; }
  .summary strong { font-size: 1.4rem; display: block; }
</style>
</head>
<body>
<h1>kubespin fleet dashboard</h1>
<div class="meta">Generated {{.GeneratedAt}}</div>
<div class="summary">
  <div><strong>{{.Total}}</strong>clusters</div>
  <div><strong>{{.Stale}}</strong>stale</div>
  <div><strong>{{.Drifted}}</strong>drifted</div>
</div>
<table>
<thead><tr><th>Cluster</th><th>Provider</th><th>Phase</th><th>Reporting</th><th>Last Reported</th><th>Drift</th></tr></thead>
<tbody>
{{range .Rows}}
<tr>
  <td>{{.ClusterID}}</td>
  <td>{{.Provider}}</td>
  <td>{{.Phase}}</td>
  <td>{{if .Stale}}<span class="badge badge-bad">stale</span>{{else}}<span class="badge badge-ok">fresh</span>{{end}}</td>
  <td>{{.LastReportedAt}}</td>
  <td>
    {{if not .Audited}}<span class="badge badge-muted">never audited</span>
    {{else if eq (len .Findings) 0}}<span class="badge badge-ok">clean ({{.FindingsAt}})</span>
    {{else}}<span class="badge badge-warn">{{len .Findings}} finding(s) ({{.FindingsAt}})</span>
      <ul class="findings">{{range .Findings}}<li>{{.}}</li>{{end}}</ul>
    {{end}}
  </td>
</tr>
{{end}}
</tbody>
</table>
</body>
</html>
`))

func renderDashboardHTML(rows []fleet.DashboardRow, generatedAt time.Time) ([]byte, error) {
	data := dashboardData{
		GeneratedAt: generatedAt.UTC().Format(time.RFC3339),
		Total:       len(rows),
	}
	for _, r := range rows {
		view := dashboardRowView{
			ClusterID: string(r.ClusterID),
			Provider:  r.Provider.String(),
			Phase:     r.Phase.String(),
			Stale:     r.Stale,
			Findings:  r.Findings,
			Audited:   !r.FindingsAt.IsZero(),
		}
		if !r.LastReportedAt.IsZero() {
			view.LastReportedAt = r.LastReportedAt.UTC().Format(time.RFC3339)
		} else {
			view.LastReportedAt = "never"
		}
		if !r.FindingsAt.IsZero() {
			view.FindingsAt = r.FindingsAt.UTC().Format(time.RFC3339)
		}
		if r.Stale {
			data.Stale++
		}
		if len(r.Findings) > 0 {
			data.Drifted++
		}
		data.Rows = append(data.Rows, view)
	}

	var buf bytes.Buffer
	if err := dashboardTemplate.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("executing dashboard template: %w", err)
	}
	return buf.Bytes(), nil
}
