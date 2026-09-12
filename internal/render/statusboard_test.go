package render

import (
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/model"
)

// boardRows builds the board and indexes it by subsystem for assertions.
func boardRows(t *testing.T, c *model.Context) map[string]boardRow {
	t.Helper()
	rows := buildBoard(c)
	out := make(map[string]boardRow, len(rows))
	for _, r := range rows {
		out[r.subsystem] = r
	}
	return out
}

// TestBoard_neverOkOverFindings is the regression guard for bug_report.md N2:
// the --full board used to hardcode "ok" for the WAL/checkpoints/replication/
// settings rows and ignored mxid_wraparound and fk_unindexed — printing "ok"
// directly above a CRITICAL finding on the same screen. Every row whose
// subsystem has findings must reflect the worst of them.
func TestBoard_neverOkOverFindings(t *testing.T) {
	c := &model.Context{
		Server:      model.ServerInfo{Database: "db"},
		WAL:         &model.WAL{BytesPerSec: f64(1024)},
		IO:          &model.IO{CheckpointsTimed: 1, CheckpointsReq: 9},
		Replication: &model.Replication{Replicas: []model.ReplicaRow{{AppName: "standby1"}}},
		Settings:    &model.Settings{Overrides: map[string]string{"fsync": "off"}},
		Limits:      &model.Limits{ConnectionsMax: 100, ConnectionsUsed: 10, MaxXIDAge: 200, MaxMXIDAge: 300},
		Indexes:     &model.Indexes{Total: 10},
		Findings: []model.Finding{
			{ID: "sync_rep_degraded", Severity: model.SeverityCritical, Title: "sync replication degraded"},
			{ID: "fsync_off", Severity: model.SeverityCritical, Title: "fsync is off"},
			{ID: "checkpoints_forced", Severity: model.SeverityWarn, Title: "checkpoints forced"},
			{ID: "archiving_stalled", Severity: model.SeverityCritical, Title: "archiving stalled"},
			{ID: "mxid_wraparound", Severity: model.SeverityWarn, Title: "mxid age high"},
			{ID: "fk_unindexed", Severity: model.SeverityWarn, Title: "fk unindexed"},
		},
	}
	rows := boardRows(t, c)
	cases := []struct {
		subsystem, status string
		kind              statusKind
	}{
		{"replication", "fail", kBad},   // sync_rep_degraded is critical
		{"settings", "fail", kBad},      // fsync_off is critical
		{"WAL", "fail", kBad},           // archiving_stalled is critical
		{"checkpoints", "warn", kWatch}, // checkpoints_forced is warn
		{"wraparound", "warn", kWatch},  // mxid_wraparound must be honored too
		{"indexes", "warn", kWatch},     // fk_unindexed must be honored too
	}
	for _, tc := range cases {
		r, ok := rows[tc.subsystem]
		if !ok {
			t.Fatalf("board is missing the %s row", tc.subsystem)
		}
		if r.status != tc.status || r.kind != tc.kind {
			t.Errorf("%s row = %q/%v, want %q/%v", tc.subsystem, r.status, r.kind, tc.status, tc.kind)
		}
	}
}

// TestBoard_okWhenClean: with no findings, every derived row reads ok — the
// board must not cry wolf either.
func TestBoard_okWhenClean(t *testing.T) {
	c := &model.Context{
		Server:      model.ServerInfo{Database: "db"},
		WAL:         &model.WAL{BytesPerSec: f64(1024)},
		IO:          &model.IO{CheckpointsTimed: 9, CheckpointsReq: 0},
		Replication: &model.Replication{Replicas: []model.ReplicaRow{{AppName: "standby1"}}},
		Settings:    &model.Settings{Overrides: map[string]string{"work_mem": "64MB"}},
		Limits:      &model.Limits{ConnectionsMax: 100, ConnectionsUsed: 10, MaxXIDAge: 200},
		Findings:    []model.Finding{},
	}
	for _, r := range boardRows(t, c) {
		if r.status == "fail" || r.status == "warn" {
			t.Errorf("clean context produced %s row = %q", r.subsystem, r.status)
		}
	}
}

// TestBoard_renderedOutputNamesIt: the rendered board shows the degraded
// status words, so the fix is visible end-to-end, not just in the struct.
func TestBoard_renderedOutput(t *testing.T) {
	c := &model.Context{
		Server:      model.ServerInfo{Database: "db"},
		WAL:         &model.WAL{BytesPerSec: f64(1024)},
		Replication: &model.Replication{Replicas: []model.ReplicaRow{{AppName: "s1"}}},
		Settings:    &model.Settings{Overrides: map[string]string{"fsync": "off"}},
		IO:          &model.IO{CheckpointsTimed: 1, CheckpointsReq: 9},
		Findings: []model.Finding{
			{ID: "sync_rep_degraded", Severity: model.SeverityCritical, Title: "x"},
			{ID: "fsync_off", Severity: model.SeverityCritical, Title: "x"},
			{ID: "checkpoints_forced", Severity: model.SeverityWarn, Title: "x"},
		},
	}
	var b strings.Builder
	renderBoard(&b, styler{on: false}, buildBoard(c))
	out := b.String()
	for _, row := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.Contains(row, "replication") || strings.Contains(row, "settings") {
			if strings.Contains(row, " ok ") {
				t.Errorf("row contradicts its own critical finding: %q", row)
			}
		}
	}
}
