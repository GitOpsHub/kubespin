package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/GitOpsHub/kubespin/internal/core"
	"github.com/GitOpsHub/kubespin/internal/fleet"
)

func TestRenderDashboardHTML(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := []fleet.DashboardRow{
		{
			ClusterID: "team-a", Provider: core.ProviderAWS, Phase: core.PhaseReady,
			Stale: false, LastReportedAt: now.Add(-time.Minute),
			Findings: nil, FindingsAt: now,
		},
		{
			ClusterID: "team-b", Provider: core.ProviderGCP, Phase: core.PhaseReady,
			Stale: true, Findings: []string{"node pool default drifted"}, FindingsAt: now,
		},
		{
			ClusterID: "team-c", Provider: core.ProviderAzure, Phase: core.PhasePending,
		},
	}

	html, err := renderDashboardHTML(rows, now)
	if err != nil {
		t.Fatalf("renderDashboardHTML: %v", err)
	}
	out := string(html)

	for _, want := range []string{"team-a", "team-b", "team-c", "node pool default drifted", "never audited", "3", "clusters"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered dashboard missing %q", want)
		}
	}

	// A cluster ID or finding containing HTML-significant characters must be
	// escaped by html/template rather than injected verbatim.
	injected := []fleet.DashboardRow{{ClusterID: "team-<script>", Findings: []string{"<b>x</b>"}, FindingsAt: now}}
	html, err = renderDashboardHTML(injected, now)
	if err != nil {
		t.Fatalf("renderDashboardHTML: %v", err)
	}
	if strings.Contains(string(html), "<script>") {
		t.Error("cluster ID was not HTML-escaped")
	}
}
