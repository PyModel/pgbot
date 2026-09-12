package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

// promContext builds a minimal context whose findings render as pgbot_finding
// series.
func promContext(db string, findings ...model.Finding) *model.Context {
	return &model.Context{
		Server:   model.ServerInfo{Database: db},
		Findings: findings,
	}
}

// The text format forbids two samples of one family with the same label set —
// a scrape (or promtool) rejects the WHOLE file, so every pgbot metric
// disappears. Two expired ignore rules used to emit two byte-identical
// suppression_expired series (N5); they must now carry distinguishing objects,
// and the writer must refuse a duplicate label set even if a future finding
// source regresses.
func TestPrometheusNoDuplicateSeries(t *testing.T) {
	c := promContext("app",
		model.Finding{ID: "suppression_expired", Severity: model.SeverityInfo,
			Object: "low_cache_hit expires=2026-01-01"},
		model.Finding{ID: "suppression_expired", Severity: model.SeverityInfo,
			Object: "seq_scan_heavy (public.big) expires=2026-02-01"},
	)
	var b bytes.Buffer
	if err := Prometheus(&b, c); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(b.String(), "\n") {
		if !strings.HasPrefix(line, "pgbot_") || strings.HasPrefix(line, "#") {
			continue
		}
		key := labelKey(line)
		if seen[key] {
			t.Fatalf("duplicate series %q — the exposition is invalid:\n%s", key, b.String())
		}
		seen[key] = true
	}
	if len(seen) < 2 {
		t.Errorf("both expired rules must surface as distinct series, got %d:\n%s", len(seen), b.String())
	}
}

// The writer-level guard: even findings with IDENTICAL labels (a upstream
// generator bug) must not produce a duplicate line — validity wins.
func TestPrometheusFamilyDedupesIdenticalLabelSets(t *testing.T) {
	c := promContext("app",
		model.Finding{ID: "x", Severity: model.SeverityWarn},
		model.Finding{ID: "x", Severity: model.SeverityWarn}, // same id, same labels
	)
	var b bytes.Buffer
	if err := Prometheus(&b, c); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(b.String(), `pgbot_finding{database="app",id="x"`); n != 1 {
		t.Errorf("identical label sets must collapse to one series, found %d:\n%s", n, b.String())
	}
}

// labelKey: the identity is the name+labels, never the sample value.
func TestLabelKey(t *testing.T) {
	if got := labelKey(`pgbot_tps{database="app"} 1.5`); got != `pgbot_tps{database="app"}` {
		t.Errorf("labelKey = %q", got)
	}
	if got := labelKey(`pgbot_x{database="a",id="b",object="{weird}"} 1`); got != `pgbot_x{database="a",id="b",object="{weird}"}` {
		t.Errorf("labelKey must skip braces inside quoted values: %q", got)
	}
	if got := labelKey(`pgbot_x{object="say \"hi\"}"} 1`); got != `pgbot_x{object="say \"hi\"}"}` {
		t.Errorf("labelKey must honor escaped quotes: %q", got)
	}
	if got := labelKey(`pgbot_bare 3`); got != "pgbot_bare" {
		t.Errorf("bare sample keyed by name: %q", got)
	}
}
